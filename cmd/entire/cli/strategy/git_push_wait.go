package strategy

import (
	"context"
	"log/slog"
	"os"
	"strconv"
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

// gitAncestorSearchDepth is how far up the process tree the hook looks for
// git: git → hook shell → entire, plus room for hook managers (husky,
// lefthook) that add a layer or two.
const gitAncestorSearchDepth = 6

// Vars for tests.
var (
	// userPushWaitLimit bounds how long new rounds of pushes keep being taken,
	// so a stream of pushes cannot hold the upload off for ever. A push already
	// running is still waited for to its end (userPushPIDWaitLimit).
	userPushWaitLimit = 2 * time.Minute
	// userPushPIDWaitLimit bounds the wait for one recorded git process: a
	// push slower than the round limit must not have the upload land on it.
	userPushPIDWaitLimit = 10 * time.Minute
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
	push := checkpoint.UserPush{PID: pushingGitPID(), At: time.Now()}
	if push.PID > 0 {
		push.Start = processStartTime(push.PID)
	}
	if err := coord.NoteUserPush(push); err != nil {
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
// reused by anything else counts as exited. start, when non-zero, is the start
// time recorded with the PID: a different one means another git reused it.
func isLiveGitProcess(pid int, start int64) bool {
	_, name, ok := processLookup()(pid)
	if !ok || !isGitProcessName(name) {
		return false
	}
	if start == 0 {
		return true
	}
	now := processStartTime(pid)
	return now == 0 || now == start
}

// waitForUserPushes blocks until every user push the hooks recorded has exited
// and settled, including pushes recorded while it waits. Rounds stop being
// taken after userPushWaitLimit; a round already taken is waited out under ctx.
func waitForUserPushes(ctx context.Context, coord *checkpoint.UploadCoordinator) {
	roundsCtx, cancel := context.WithTimeout(ctx, userPushWaitLimit)
	defer cancel()
	start := time.Now()
	waited := 0
	for roundsCtx.Err() == nil && ctx.Err() == nil {
		pushes, err := coord.TakeUserPushes()
		if err != nil || len(pushes) == 0 {
			break
		}
		settle := userPushSettle
		for _, p := range pushes {
			waited++
			if p.PID > 0 {
				waitUserPushExit(ctx, p)
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

func waitUserPushExit(ctx context.Context, p checkpoint.UserPush) {
	ctx, cancel := context.WithTimeout(ctx, userPushPIDWaitLimit)
	defer cancel()
	waitProcessExit(ctx, p.PID, p.Start)
}

// parseProcStat reads Linux /proc/<pid>/stat: "pid (comm) state ppid ...",
// where comm may contain spaces and parens. A zombie (Z) or dead (X) process
// has exited and is reported as not found. start is field 22, the start time
// in clock ticks since boot.
func parseProcStat(s string) (ppid int, comm string, start int64, ok bool) {
	open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < open {
		return 0, "", 0, false
	}
	fields := strings.Fields(s[closeIdx+1:])
	if len(fields) < 20 {
		return 0, "", 0, false
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, "", 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, "", 0, false
	}
	start, err = strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, "", 0, false
	}
	return ppid, s[open+1 : closeIdx], start, true
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
