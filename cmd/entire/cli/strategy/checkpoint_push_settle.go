package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/perf"
)

// GitHub rejects a push whose pack arrives within about a second of another
// push to the same repository finishing, and the push it rejects is the later
// of the two. The pre-push hook pushes entire/checkpoints/v1 and returns, and
// git sends the user's pack a few milliseconds later, so a push that carried
// new checkpoints is the user's own push that GitHub fails: the remote cannot
// write the pack into its quarantine directory and rejects the ref. Running
// `git push` again succeeds only because the hook then has nothing to push
// (#2794).
//
// settleBeforeUserPush holds the hook for checkpointPushSettle after such a
// push, so the user's pack lands outside that window.

// checkpointPushSettle is how long the pre-push hook waits, after its own
// checkpoint push moved a ref, before handing back to git. Long enough to
// clear GitHub's window with margin; paid only by a push that actually carried
// new checkpoints. Declared as a var so tests can shrink it.
var checkpointPushSettle = 1200 * time.Millisecond

// settleBeforeUserPush waits checkpointPushSettle unless every destination of
// target is on this machine, where there is no forge to race and where the
// test suites push. It returns early if ctx ends.
func settleBeforeUserPush(ctx context.Context, target string) {
	if pushTargetIsLocal(ctx, target) {
		return
	}
	_, span := perf.Start(ctx, "checkpoint_push_settle")
	defer span.End()
	waitForSettle(ctx, checkpointPushSettle)
}

// waitForSettle is the wait itself. Declared as a var so tests can see whether
// the hook waited without paying for it.
var waitForSettle = func(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// pushTargetIsLocal reports whether every URL a push to target reaches is a
// repository on this machine's filesystem. target is what git passed the hook:
// a remote name, or the URL or path pushed to directly. A configured remote is
// resolved first, whatever its name looks like (a name may contain '@'); any
// other target is classified as a URL itself, which is how git reads it.
func pushTargetIsLocal(ctx context.Context, target string) bool {
	urls := []string{target}
	dir, env := gitHookSafeDirEnv(ctx)
	if pushURLs, err := gitremote.GetPushURLsInDir(ctx, dir, env, target); err == nil {
		urls = pushURLs
	}
	for _, u := range urls {
		if !gitURLIsLocal(u) {
			return false
		}
	}
	return true
}

// gitURLIsLocal follows git's own test for a local repository (transport.c,
// url_is_local_not_ssh): a file:// URL, or no "scheme://" and either no colon
// or a slash before the first one, which tells "../repo.git" and
// "/srv/a:b.git" apart from scp-style "host:path". A Windows drive or UNC
// prefix is local too.
func gitURLIsLocal(u string) bool {
	if strings.HasPrefix(u, "file://") {
		return true
	}
	if strings.Contains(u, "://") {
		return false
	}
	if filepath.VolumeName(u) != "" {
		return true
	}
	colon := strings.IndexByte(u, ':')
	if colon < 0 {
		return true
	}
	slash := strings.IndexByte(u, '/')
	return slash >= 0 && slash < colon
}
