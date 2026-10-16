//go:build !windows

package strategy

import (
	"context"
	"time"
)

// waitProcessExit polls until pid is no longer a running git process or ctx
// ends. Unix has no portable way to wait on a process that is not a child.
func waitProcessExit(ctx context.Context, pid int) {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for isLiveGitProcess(pid) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
