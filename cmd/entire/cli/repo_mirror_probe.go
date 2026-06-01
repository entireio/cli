package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"

	"github.com/entireio/cli/cmd/entire/cli/auth"
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
// unescaped into the STS audience (auth.RepoScopedToken) and the smart-HTTP
// probe URL (waitForMirrorClone); a loose pattern would admit ?, #, %, .. and
// control chars, letting a name like `repo?bypass=1` smuggle a query string
// or `repo#x` truncate the path. GitHub owners are [A-Za-z0-9-] and repos are
// [A-Za-z0-9._-], so matching upstream reality closes those vectors at the
// boundary instead of relying on whatever the server does with weird strings.
const (
	gitHubOwnerPat = `([A-Za-z0-9-]+)`
	gitHubRepoPat  = `([A-Za-z0-9._-]+?)`
)

var (
	gitHubHTTPSRe = regexp.MustCompile(`^https?://github\.com/` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubSSHRe   = regexp.MustCompile(`^git@github\.com:` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubBareRe  = regexp.MustCompile(`^(?:github\.com/)?` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
)

func parseGitHubURL(rawURL string) (owner, repo string, err error) {
	for _, re := range []*regexp.Regexp{gitHubHTTPSRe, gitHubSSHRe, gitHubBareRe} {
		if m := re.FindStringSubmatch(rawURL); m != nil {
			return strings.ToLower(m[1]), strings.ToLower(m[2]), nil
		}
	}
	return "", "", fmt.Errorf("not a recognized GitHub URL: %s", rawURL)
}

// waitForMirrorClone blocks until the mirror at /gh/<owner>/<repo> on
// clusterHost advertises a resolvable HEAD (the initial GitHub→EntireDB
// clone has landed) or the deadline expires. It probes the data plane's
// smart-HTTP info/refs endpoint every 2s, printing a heartbeat so a long
// clone doesn't look hung.
//
// Repo-scoped pull tokens are short-lived (minutes) while the default wait
// is 30m, so a single token can't cover the whole wait. The loop re-mints
// from the (long-lived) login token whenever a probe comes back 401,
// rather than minting once up front.
func waitForMirrorClone(ctx context.Context, out io.Writer, clusterHost, owner, repo string, timeout time.Duration) error {
	repoSlug := "/gh/" + owner + "/" + repo
	checkURL := fmt.Sprintf("https://%s%s/info/refs?service=git-upload-pack", clusterHost, repoSlug)

	mintToken := func() (string, error) {
		return auth.RepoScopedToken(ctx, "https://"+clusterHost, repoSlug, "pull")
	}
	token, err := mintToken()
	if err != nil {
		return fmt.Errorf("authorize clone probe: %w", err)
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	cloning := false
	for {
		ready, status := mirrorAdvertisesHead(ctx, checkURL, token)
		switch {
		case ready:
			if cloning {
				fmt.Fprintln(out, " ready")
			} else {
				fmt.Fprintln(out, "  ready to use")
			}
			return nil
		case status == http.StatusUnauthorized:
			// The repo-scoped token expired mid-wait; mint a fresh one from
			// the login token and re-probe after the usual interval (no dot,
			// since this is a token refresh, not clone progress).
			if token, err = mintToken(); err != nil {
				if cloning {
					fmt.Fprintln(out)
				}
				return fmt.Errorf("re-authorize clone probe: %w", err)
			}
		default:
			if !cloning {
				fmt.Fprint(out, "  cloning")
				cloning = true
			}
			fmt.Fprint(out, ".")
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			// User cancellation (Ctrl+C) should exit quietly, not print a
			// "timed out…: context canceled" line. NewSilentError signals
			// main.go to skip printing; a real deadline still reports.
			if errors.Is(ctx.Err(), context.Canceled) {
				return NewSilentError(ctx.Err())
			}
			return fmt.Errorf("timed out waiting for initial clone: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// mirrorAdvertisesHead fetches the smart-HTTP ref advertisement and reports
// whether HEAD resolves to a real commit, plus the HTTP status so the
// caller can distinguish an expired token (401, re-mint) from a
// still-cloning mirror (200 with no resolvable HEAD, or 404/503). status is
// 0 for transport/build/decode failures, treated as "not ready, keep
// waiting". Auth is the repo-scoped token as HTTP basic-auth password, the
// same shape git presents over the entire:// transport.
func mirrorAdvertisesHead(ctx context.Context, checkURL, token string) (ready bool, status int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, 0
	}
	req.SetBasicAuth("entire-cli", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode
	}
	// Smart-HTTP wraps the advertisement in a "# service=..." pkt-line
	// header + flush; AdvRefs.Decode expects to start at the first ref
	// line, so strip the wrapper first.
	var sr packp.SmartReply
	if err := sr.Decode(resp.Body); err != nil {
		return false, 0
	}
	var adv packp.AdvRefs
	if err := adv.Decode(resp.Body); err != nil {
		return false, 0
	}
	if _, err := adv.ResolvedHead(); err != nil {
		return false, http.StatusOK // reachable, clone still in progress
	}
	return true, http.StatusOK
}
