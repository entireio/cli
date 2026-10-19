//go:build !windows

package strategy

import (
	"context"
	"time"
)

// waitProcessExit polls until pid is no longer the running git process that
// started at start, or ctx ends. Unix has no portable way to wait on a process
// that is not a child.
func waitProcessExit(ctx context.Context, pid int, start int64) {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for isLiveGitProcess(pid, start) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
