package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/google/uuid"
)

// gitHubHTTPSRe / gitHubSSHRe / gitHubBareRe parse the GitHub URL shapes
// `mirror add`/`remove` accept, mirroring the standalone entiredb CLI:
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
	// gitHubOwnerRepoPat is the pair every GitHub shape ends in. One spelling,
	// so the charset boundary above has one place to change: `.git` handling
	// moved CLI-wide once while these patterns' own `(?:\.git)?` groups stayed
	// put, which is the kind of miss five copies invite.
	gitHubOwnerRepoPat = gitHubOwnerPat + `/` + gitHubRepoPat
)

var (
	gitHubHTTPSRe = regexp.MustCompile(`^https?://github\.com/` + gitHubOwnerRepoPat + `(?:\.git)?$`)
	gitHubSSHRe   = regexp.MustCompile(`^git@github\.com:` + gitHubOwnerRepoPat + `(?:\.git)?$`)
	gitHubBareRe  = regexp.MustCompile(`^(?:github\.com/)?` + gitHubOwnerRepoPat + `(?:\.git)?$`)

	// gitHubHostedBareRe is gitHubBareRe with the host REQUIRED. The optional
	// `github.com/` in gitHubBareRe is what keeps it out of
	// parseHostedGitHubURL: it also matches a bare `owner/repo`, which names no
	// forge at all. Anchoring the host makes the shape unambiguous, so a caller
	// that must not guess a forge can still recognise `github.com/owner/repo`.
	gitHubHostedBareRe = regexp.MustCompile(`^github\.com/` + gitHubOwnerRepoPat + `(?:\.git)?$`)

	// dotOnlyRe matches repo segments that are entirely dots
	// (".", "..", ...). The tightened owner charset already excludes
	// dots, but gitHubRepoPat allows ".", and a dot-only repo name would
	// embed a literal ".." in both /gh/<owner>/<repo> and the
	// token-exchange audience. Reject at the boundary.
	//
	// Not forge-specific, despite living beside the GitHub patterns: dropping
	// `.git` can MANUFACTURE a dot-only name out of one that was not (`..git`
	// → `.`, `...git` → `..`), so every grammar that trims the suffix has to
	// re-check afterwards, native ones included.
	dotOnlyRe = regexp.MustCompile(`^\.+$`)
)

func parseGitHubURL(rawURL string) (owner, repo string, err error) {
	return matchGitHubURL(rawURL, gitHubHTTPSRe, gitHubSSHRe, gitHubBareRe)
}

// parseHostedGitHubURL accepts the shapes that name github.com explicitly —
// https, ssh, and the host-qualified bare form — but never an unqualified
// `owner/repo`, which names no forge and so cannot be attributed to one. Its
// caller uses that to point a GitHub URL at the `/gh/` ref it should have been;
// guessing a forge for a bare pair is exactly what `repo clone`'s grammar
// refuses to do. Same dot-only guard as parseGitHubURL.
func parseHostedGitHubURL(rawURL string) (owner, repo string, err error) {
	return matchGitHubURL(rawURL, gitHubHTTPSRe, gitHubSSHRe, gitHubHostedBareRe)
}

func matchGitHubURL(rawURL string, res ...*regexp.Regexp) (owner, repo string, err error) {
	for _, re := range res {
		m := re.FindStringSubmatch(rawURL)
		if m == nil {
			continue
		}
		owner, repo = strings.ToLower(m[1]), strings.ToLower(m[2])
		if dotOnlyRe.MatchString(repo) {
			return "", "", fmt.Errorf("invalid GitHub URL: repo cannot be dot-only: %s", rawURL)
		}
		return owner, repo, nil
	}
	return "", "", fmt.Errorf("not a recognized GitHub URL: %s", rawURL)
}

// mirrorPollInterval is the cadence for placement and clone-status polls.
var mirrorPollInterval = 2 * time.Second

// maxConsecutivePollErrors bounds how many back-to-back poll failures a wait
// tolerates before giving up. Both phases share the budget, for two different
// reasons.
//
// The clone wait has two failure modes: a brief network/API glitch during a
// long clone, and — the common one — the stale-read window right after the
// placement, where the control plane returns 404 "mirror not found" because the
// just-written repo#list grant / placement row isn't yet visible to the
// region's minimize_latency + follower reads (~4.8s nominal, but it spikes
// under concurrent multi-region adds). At the 2s cadence, 15 tolerated errors
// ≈ 30s — enough to ride out that window, while a genuinely persistent error
// (deleted mirror, revoked auth) still surfaces well before --timeout. This is
// a stopgap: the durable fix is server-side, making GetMirror check the grant
// fully-consistent and read the row from the CRDB leaseholder so a fresh mirror
// is visible on the first poll.
//
// The placement wait reuses the number rather than tuning a second one. Its
// failure profile is different — the request row is written before the 202
// returns, so it has no equivalent stale-read window and only transient
// control-plane errors land here — but ~30s of tolerance is comfortably inside
// the same --timeout. Split the constant rather than re-tuning it if the two
// phases ever need different budgets.
const maxConsecutivePollErrors = 15

var (
	// errMirrorCloneFailed reports the mirror's initial clone reached the
	// terminal "failed" status — the server gave up cloning the upstream.
	errMirrorCloneFailed = errors.New("initial clone failed")
	// errMirrorSuspended reports the placement is suspended: registered, but the
	// cluster won't serve it. Recovery is operator-side.
	errMirrorSuspended = errors.New("mirror is suspended")
	// errMirrorPlacementFailed reports the mirror request itself reached the
	// terminal "failed" status: the server has given its verdict and nothing
	// is still in flight. mirrorRequestFailureError wraps it with the server's
	// code and message.
	errMirrorPlacementFailed = errors.New("mirror placement failed")
)

// mirrorStatusGetter is the slice of *coreapi.Client that awaitMirrorReady
// needs, declared as an interface so the poll is unit-testable with a fake.
type mirrorStatusGetter interface {
	GetMirror(ctx context.Context, params coreapi.GetMirrorParams) (*coreapi.Mirror, error)
}

type mirrorRequestGetter interface {
	GetMirrorRequest(ctx context.Context, params coreapi.GetMirrorRequestParams) (*coreapi.MirrorRequest, error)
}

// awaitMirrorPlacement polls a submitted mirror request until it reaches a
// terminal status, returning the placement it produced. It returns:
//
//   - the placed mirror       when the request succeeded
//   - errMirrorPlacementFailed (wrapped with the server's code and message)
//     when the request reached "failed"
//   - a timeout/transport err when the wait deadline passed, or polls kept
//     erroring past maxConsecutivePollErrors
//
// The request id comes from the 202 body's required requestId, never from the
// Location header. Two reasons, both load-bearing:
//
// Location is an optional response header in the spec, so requiring it would
// fail a request whose body already reports a succeeded placement — and the
// body is what GetMirrorRequest needs anyway (the id alone addresses it).
//
// More importantly, the header's HOST must never become a poll target.
// Re-pointing the client's base URL at a server-named origin would send the
// control-plane bearer there, which is precisely what the cross-juris
// transport's federation-manifest check exists to prevent. Polling therefore
// stays on the client's configured base URL and lets the cross-jurisdiction
// transport handle a 421 to the home core on every tick (see
// internal/coreapi/cross_juris_client.go). That also keeps the wait
// recoverable for its whole duration: the home core answers a foreign-region
// login JWT with a bare 401, and the follower treats that as an exchange
// trigger only on a hop it reached by following a 421. Short-circuiting
// straight to the home core skips the redirect, so it works only until the
// exchanged token leaves the transport's cache and then fails unrecoverably.
func awaitMirrorPlacement(ctx context.Context, c mirrorRequestGetter, initial coreapi.MirrorRequest, onStatus func(coreapi.MirrorRequestStatus)) (*coreapi.MirrorRequestResult, error) {
	requestID := initial.RequestId
	if requestID == uuid.Nil {
		return nil, errors.New("mirror request response is missing a request id")
	}

	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	request := &initial
	var consecutiveErrs int
	for {
		if onStatus != nil {
			onStatus(request.Status)
		}
		switch request.Status {
		case coreapi.MirrorRequestStatusSucceeded:
			result, ok := request.Result.Get()
			if !ok || result.MirrorId == "" || result.MirrorUrl == "" {
				return nil, errors.New("mirror request succeeded without a mirror id and URL")
			}
			return &result, nil
		case coreapi.MirrorRequestStatusFailed:
			return nil, mirrorRequestFailureError(*request)
		case coreapi.MirrorRequestStatusPending, coreapi.MirrorRequestStatusProcessing:
		default:
			// Defensive only: MirrorRequestStatus is a generated CLOSED enum,
			// so a status this CLI doesn't know fails in UnmarshalText and
			// surfaces as a decode error from GetMirrorRequest below — retried,
			// then reported — rather than reaching here. Treating an unknown
			// status as a generic terminal state, as the operation's contract
			// asks, needs the enum generated open, which is a spec change.
			return nil, fmt.Errorf("mirror request returned unknown status %q", request.Status)
		}

		select {
		case <-ctx.Done():
			return nil, classifyWaitContextErr(ctx.Err(), "waiting for mirror placement")
		case <-ticker.C:
		}

		next, err := c.GetMirrorRequest(ctx, coreapi.GetMirrorRequestParams{RequestId: requestID})
		if err != nil {
			if ctx.Err() != nil {
				return nil, classifyWaitContextErr(ctx.Err(), "waiting for mirror placement")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return nil, fmt.Errorf("poll mirror request: %w", err)
			}
			continue
		}
		consecutiveErrs = 0
		if next.RequestId != requestID {
			return nil, fmt.Errorf("mirror request poll returned id %s, want %s", next.RequestId, requestID)
		}
		request = next
	}
}

func mirrorRequestFailureError(request coreapi.MirrorRequest) error {
	failure, ok := request.Failure.Get()
	if !ok {
		return fmt.Errorf("%w without failure details", errMirrorPlacementFailed)
	}

	known := false
	switch failure.Code {
	case "repo_inaccessible", "github_link_required", "source_too_large", "github_unavailable",
		"mirror_url_conflict", "mirror_conflict", "placement_deleted", "mirror_name_conflict",
		"invalid_request", "internal":
		known = true
	}
	var detail string
	if known {
		detail = fmt.Sprintf("(%s): %s", failure.Code, failure.Message)
	} else {
		detail = fmt.Sprintf("with unknown failure code %q: %s", failure.Code, failure.Message)
	}
	if failure.Retryable {
		detail += "; retry this command"
	}
	return fmt.Errorf("%w %s", errMirrorPlacementFailed, detail)
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
func awaitMirrorReady(ctx context.Context, c mirrorStatusGetter, mirrorID string, timeout time.Duration) (coreapi.MirrorStatus, error) {
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
				return last, classifyWaitContextErr(ctx.Err(), "waiting for initial clone")
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
			return last, classifyWaitContextErr(ctx.Err(), "waiting for initial clone")
		case <-ticker.C:
		}
	}
}

func classifyWaitContextErr(err error, what string) error {
	if errors.Is(err, context.Canceled) {
		return NewSilentError(err)
	}
	return fmt.Errorf("timed out %s: %w", what, err)
}
