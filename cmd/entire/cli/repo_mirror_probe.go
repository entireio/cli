package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/internal/coreapi"
)

// gitHubHTTPSRe / gitHubSSHRe / gitHubBareRe parse the GitHub URL shapes
// `mirror create`/`remove` accept, mirroring the standalone entiredb CLI:
//
//	https://github.com/<owner>/<repo>(.git)
//	git@github.com:<owner>/<repo>(.git)
//	(github.com/)<owner>/<repo>
//
// owner/repo are lowercased so the synthesised /gh/<owner>/<repo> slug
// matches what the server persists.
//
// The owner/repo capture groups are restricted to GitHub's real identifier
// charset rather than a permissive "anything but slash". owner/repo flow
// unescaped into the STS audience (entireclient/repocreds) and the clone URL;
// a loose pattern would admit ?, #, %, .. and control chars, letting a name
// like `repo?bypass=1` smuggle a query string or `repo#x` truncate the path.
// GitHub owners are [A-Za-z0-9-] and repos are [A-Za-z0-9._-], so matching
// upstream reality closes those vectors at the boundary instead of relying on
// whatever the server does with weird strings.
const (
	gitHubOwnerPat = `([A-Za-z0-9-]+)`
	gitHubRepoPat  = `([A-Za-z0-9._-]+?)`
)

var (
	gitHubHTTPSRe = regexp.MustCompile(`^https?://github\.com/` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubSSHRe   = regexp.MustCompile(`^git@github\.com:` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubBareRe  = regexp.MustCompile(`^(?:github\.com/)?` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)

	// gitHubDotOnlyRe matches repo segments that are entirely dots
	// (".", "..", ...). The tightened owner charset already excludes
	// dots, but gitHubRepoPat allows ".", and a dot-only repo name would
	// embed a literal ".." in both /gh/<owner>/<repo> and the
	// token-exchange audience. Reject at the boundary.
	gitHubDotOnlyRe = regexp.MustCompile(`^\.+$`)
)

func parseGitHubURL(rawURL string) (owner, repo string, err error) {
	for _, re := range []*regexp.Regexp{gitHubHTTPSRe, gitHubSSHRe, gitHubBareRe} {
		m := re.FindStringSubmatch(rawURL)
		if m == nil {
			continue
		}
		owner, repo = strings.ToLower(m[1]), strings.ToLower(m[2])
		if gitHubDotOnlyRe.MatchString(repo) {
			return "", "", fmt.Errorf("invalid GitHub URL: repo cannot be dot-only: %s", rawURL)
		}
		return owner, repo, nil
	}
	return "", "", fmt.Errorf("not a recognized GitHub URL: %s", rawURL)
}

// mirrorPollInterval is the cadence between mirror-status polls while waiting
// for the initial clone. A package var (not const) so tests can shorten it.
var mirrorPollInterval = 2 * time.Second

// maxConsecutivePollErrors bounds how many back-to-back GetMirror failures the
// clone wait tolerates before giving up. Two failure modes share this budget: a
// brief network/API glitch during a long clone, and — the common one — the
// stale-read window right after create, where the control plane returns 404
// "mirror not found" because the just-written repo#list grant / placement row
// isn't yet visible to the region's minimize_latency + follower reads (~4.8s
// nominal, but it spikes under concurrent multi-region creates). At the 2s
// cadence, 15 tolerated errors ≈ 30s — enough to ride out that window, while a
// genuinely persistent error (deleted mirror, revoked auth) still surfaces well
// before the 30m --wait-timeout. This is a stopgap: the durable fix is
// server-side, making GetMirror check the grant fully-consistent and read the
// row from the CRDB leaseholder so a fresh mirror is visible on the first poll.
// The counter resets on any successful poll.
const maxConsecutivePollErrors = 15

var (
	// errMirrorCloneFailed reports the mirror's initial clone reached the
	// terminal "failed" status — the server gave up cloning the upstream.
	errMirrorCloneFailed = errors.New("initial clone failed")
	// errMirrorSuspended reports the placement is suspended: registered, but the
	// cluster won't serve it. Recovery is operator-side (explainSuspendedMirror).
	errMirrorSuspended = errors.New("mirror is suspended")
)

// mirrorStatusGetter is the slice of *coreapi.Client that awaitMirrorReady
// needs, declared as an interface so the poll is unit-testable with a fake.
type mirrorStatusGetter interface {
	GetMirror(ctx context.Context, params coreapi.GetMirrorParams) (*coreapi.Mirror, error)
}

// awaitMirrorReady polls the control plane for a mirror's clone lifecycle until
// it reaches a terminal status or the deadline/cancellation fires. It returns
// the last observed status plus:
//
//   - nil                     when ready (the repo is clonable)
//   - errMirrorCloneFailed    when the initial clone failed
//   - errMirrorSuspended      when the placement is suspended
//   - a timeout/transport err when the wait deadline passed, or polls kept
//     erroring past maxConsecutivePollErrors (transient glitches are retried)
//
// "processing" keeps the loop running. This replaces the old smart-HTTP
// info/refs probe: the control plane now reports clone readiness directly via
// Mirror.status, so a single authenticated control-plane call per tick suffices
// — no repo-scoped token exchange or data-plane round trip.
//
// onStatus (may be nil) is invoked with each observed status so callers can show
// live per-mirror progress (e.g. the wizard's Docker-style line list).
func awaitMirrorReady(ctx context.Context, c mirrorStatusGetter, mirrorID string, timeout time.Duration, onStatus func(coreapi.MirrorStatus)) (coreapi.MirrorStatus, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var last coreapi.MirrorStatus
	var consecutiveErrs int
	for {
		m, err := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: mirrorID})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return last, classifyWaitContextErr(ctx.Err())
			}
			// Tolerate transient glitches: the clone may still be progressing,
			// so retry on the next tick. Only give up once errors persist.
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return last, fmt.Errorf("poll mirror status: %w", err)
			}
		default:
			consecutiveErrs = 0
			if s, ok := m.Status.Get(); ok {
				last = s
				if onStatus != nil {
					onStatus(s)
				}
				switch s {
				case coreapi.MirrorStatusReady:
					return s, nil
				case coreapi.MirrorStatusFailed:
					return s, errMirrorCloneFailed
				case coreapi.MirrorStatusSuspended:
					return s, errMirrorSuspended
				case coreapi.MirrorStatusProcessing:
					// keep waiting
				}
			}
		}
		select {
		case <-ctx.Done():
			return last, classifyWaitContextErr(ctx.Err())
		case <-ticker.C:
		}
	}
}

// classifyWaitContextErr maps the clone wait's context error to a user-facing
// error: a user Ctrl+C exits quietly (SilentError, so main.go doesn't reprint
// it), while a real deadline reports the timeout.
func classifyWaitContextErr(err error) error {
	if errors.Is(err, context.Canceled) {
		return NewSilentError(err)
	}
	return fmt.Errorf("timed out waiting for initial clone: %w", err)
}

// explainSuspendedMirror tells the user a suspended placement can't be served
// and to contact support. Suspension usually follows a loss of upstream GitHub
// access (App uninstalled, repo went private, or a transient API error); the
// fix is operator-side, so we point at support rather than leaking an internal
// admin command.
func explainSuspendedMirror(w io.Writer, mirrorID string) {
	fmt.Fprintf(w,
		"\nMirror %s is registered but suspended, so it can't be cloned yet.\n"+
			"This usually means upstream GitHub access was lost (App uninstalled,\n"+
			"the repo went private, or a transient API error). Contact support to\n"+
			"restore it.\n",
		mirrorID)
}
