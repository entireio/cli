package strategy

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Background checkpoint uploads must never land just before a user's push.
// GitHub rejects a push whose pack arrives within about a second of another
// push to the same repository finishing (the client sees "(failed)", "missing
// necessary objects" or "fatal error in commit_refs"), and the one rejected is
// the later of the two. The pre-push hook therefore records its git push
// (noteUserPush) and the worker waits for every recorded push to exit, plus a
// settle delay, before it uploads (waitForUserPushes).
// userPushWaitLimit bounds the whole wait, however many pushes follow.
const userPushWaitLimit = 2 * time.Minute

// gitAncestorSearchDepth is how far up the process tree the hook looks for
// git: git → hook shell → entire, plus room for hook managers (husky,
// lefthook) that add a layer or two.
const gitAncestorSearchDepth = 6

// Vars for tests.
var (
	// userPushSettle follows the last recorded push's exit. It is long enough
	// for a chained push (`git push && git push --tags`) to reach its own
	// pre-push hook and be recorded, so the worker waits for it too.
	userPushSettle = 1500 * time.Millisecond
	// unknownUserPushDelay stands in for the exit of a push whose git process
	// the hook could not find: the push is assumed done this long after its hook.
	unknownUserPushDelay = 3 * time.Second
)

// noteUserPush records the git push running this pre-push hook for the worker
// to wait on. Best-effort: a failure only loses the wait, and the worker's
// retry (isPushRaceRejection) still covers its own push.
func noteUserPush(ctx context.Context, coord *checkpoint.UploadCoordinator) {
	if err := coord.NoteUserPush(checkpoint.UserPush{PID: pushingGitPID(), At: time.Now()}); err != nil {
		logging.Debug(ctx, "record user push for checkpoint upload failed", slog.String("error", err.Error()))
	}
}

// pushingGitPID returns the PID of the `git push` running this pre-push hook,
// or 0 when no git ancestor is found.
func pushingGitPID() int {
	lookup := processLookup()
	pid := os.Getppid()
	for range gitAncestorSearchDepth {
		if pid <= 1 {
			return 0
		}
		ppid, name, ok := lookup(pid)
		if !ok {
			return 0
		}
		if isGitProcessName(name) {
			return pid
		}
		pid = ppid
	}
	return 0
}

func isGitProcessName(name string) bool {
	name = strings.ToLower(name)
	return name == "git" || name == "git.exe"
}

// isLiveGitProcess reports whether pid is still a running git process; a PID
// reused by anything else counts as exited.
func isLiveGitProcess(pid int) bool {
	_, name, ok := processLookup()(pid)
	return ok && isGitProcessName(name)
}

// waitForUserPushes blocks until every user push the hooks recorded has exited
// and settled, including pushes recorded while it waits. Bounded by
// userPushWaitLimit.
func waitForUserPushes(ctx context.Context, coord *checkpoint.UploadCoordinator) {
	ctx, cancel := context.WithTimeout(ctx, userPushWaitLimit)
	defer cancel()
	start := time.Now()
	waited := 0
	for ctx.Err() == nil {
		pushes, err := coord.TakeUserPushes()
		if err != nil || len(pushes) == 0 {
			break
		}
		settle := userPushSettle
		for _, p := range pushes {
			waited++
			if p.PID > 0 {
				waitProcessExit(ctx, p.PID)
				continue
			}
			if d := unknownUserPushDelay - time.Since(p.At); d > settle {
				settle = d
			}
		}
		sleepCtx(ctx, settle)
	}
	if waited > 0 {
		logging.Info(ctx, "waited for user pushes before checkpoint upload",
			slog.Int("pushes", waited), slog.Int64("waited_ms", time.Since(start).Milliseconds()))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
