package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/google/uuid"
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
	return matchGitHubURL(rawURL, gitHubHTTPSRe, gitHubSSHRe, gitHubBareRe)
}

// parseHostedGitHubURL accepts only the github.com-hosted shapes (https, ssh),
// not the bare `owner/repo` one, for callers where a bare pair means something
// else (the native clone shorthand). Same dot-only guard as parseGitHubURL.
func parseHostedGitHubURL(rawURL string) (owner, repo string, err error) {
	return matchGitHubURL(rawURL, gitHubHTTPSRe, gitHubSSHRe)
}

func matchGitHubURL(rawURL string, res ...*regexp.Regexp) (owner, repo string, err error) {
	for _, re := range res {
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

// mirrorPollInterval is the cadence for placement and clone-status polls.
var mirrorPollInterval = 2 * time.Second

// maxConsecutivePollErrors keeps both poll phases bounded. In the clone phase,
// 15 attempts at the two-second cadence preserve the accepted ~30s tolerance
// for a newly placed mirror to become visible.
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

type mirrorRequestGetter interface {
	GetMirrorRequest(ctx context.Context, params coreapi.GetMirrorRequestParams) (*coreapi.MirrorRequest, error)
}

func awaitMirrorPlacement(ctx context.Context, c mirrorRequestGetter, initial coreapi.MirrorRequest, location string, onStatus func(coreapi.MirrorRequestStatus)) (*coreapi.CreatedMirror, error) {
	serverURL, requestID, err := mirrorRequestPollTarget(location)
	if err != nil {
		return nil, err
	}
	if serverURL != nil {
		ctx = coreapi.WithServerURL(ctx, serverURL)
	}
	if initial.RequestId != requestID {
		return nil, fmt.Errorf("mirror request Location identifies %s but response identifies %s", requestID, initial.RequestId)
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
			return &coreapi.CreatedMirror{
				MirrorId:  result.MirrorId,
				MirrorUrl: result.MirrorUrl,
				PublicUrl: result.PublicUrl,
			}, nil
		case coreapi.MirrorRequestStatusFailed:
			return nil, mirrorRequestFailureError(*request)
		case coreapi.MirrorRequestStatusPending, coreapi.MirrorRequestStatusProcessing:
		default:
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

func mirrorRequestPollTarget(location string) (*url.URL, uuid.UUID, error) {
	if strings.TrimSpace(location) == "" {
		return nil, uuid.Nil, errors.New("mirror request response is missing Location")
	}
	locationURL, err := url.Parse(location)
	if err != nil || locationURL.User != nil || locationURL.RawQuery != "" || locationURL.Fragment != "" {
		return nil, uuid.Nil, fmt.Errorf("invalid mirror request Location %q", location)
	}
	const prefix = "/api/v1/mirror-requests/"
	requestIDText, ok := strings.CutPrefix(locationURL.Path, prefix)
	if !ok || requestIDText == "" || strings.Contains(requestIDText, "/") {
		return nil, uuid.Nil, fmt.Errorf("invalid mirror request Location %q", location)
	}
	requestID, err := uuid.Parse(requestIDText)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("invalid mirror request Location %q: %w", location, err)
	}
	if locationURL.IsAbs() {
		if locationURL.Scheme != "https" && locationURL.Scheme != "http" {
			return nil, uuid.Nil, fmt.Errorf("invalid mirror request Location %q", location)
		}
		return &url.URL{Scheme: locationURL.Scheme, Host: locationURL.Host, Path: "/api/v1"}, requestID, nil
	}
	if locationURL.Host != "" {
		return nil, uuid.Nil, fmt.Errorf("invalid mirror request Location %q", location)
	}
	return nil, requestID, nil
}

func mirrorRequestFailureError(request coreapi.MirrorRequest) error {
	failure, ok := request.Failure.Get()
	if !ok {
		return errors.New("mirror placement failed without failure details")
	}

	known := false
	switch failure.Code {
	case "repo_inaccessible", "github_link_required", "source_too_large", "github_unavailable",
		"mirror_url_conflict", "mirror_conflict", "placement_deleted", "mirror_name_conflict",
		"invalid_request", "internal":
		known = true
	}
	var message string
	if known {
		message = fmt.Sprintf("mirror placement failed (%s): %s", failure.Code, failure.Message)
	} else {
		message = fmt.Sprintf("mirror placement failed with unknown failure code %q: %s", failure.Code, failure.Message)
	}
	if failure.Retryable {
		message += "; retry this command"
	}
	return errors.New(message)
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
