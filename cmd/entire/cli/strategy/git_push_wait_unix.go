//go:build !windows

package strategy

import (
	"context"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

// pushingGit returns the `git push` running this pre-push hook: the nearest
// git ancestor within gitAncestorSearchDepth. ok is false when there is none
// or the platform cannot introspect processes.
func pushingGit() (proclive.Identity, bool) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		return proclive.Identity{}, false
	}
	for i, id := range ancestry.Chain() {
		if i >= gitAncestorSearchDepth {
			break
		}
		if isGitProcessName(id.Name) {
			return id, true
		}
	}
	return proclive.Identity{}, false
}

// gitProcessIdentity fingerprints pid when it is a running git process.
func gitProcessIdentity(pid int) (proclive.Identity, bool) {
	id, ok := proclive.IdentityOf(pid)
	if !ok || !isGitProcessName(id.Name) {
		return proclive.Identity{}, false
	}
	return id, true
}

// isLiveGitProcess reports whether id's git process is still running. An
// exited one (even unreaped), or its PID reused by another process, is not.
func isLiveGitProcess(id proclive.Identity) bool {
	return isGitProcessName(id.Name) && proclive.Check(id) == proclive.LivenessAlive
}

// waitProcessExit polls until id's process is no longer running, or ctx ends.
// Unix has no portable way to wait on a process that is not a child.
func waitProcessExit(ctx context.Context, id proclive.Identity) {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for isLiveGitProcess(id) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
