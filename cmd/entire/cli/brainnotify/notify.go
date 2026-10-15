// Package brainnotify sends content-free lifecycle hints to the optional
// Entire Brain plugin without putting plugin work on a host hook's critical
// path.
package brainnotify

import (
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// memoryWorkerOriginEnv is owned by the entire-brain plugin: it sets the
// variable in its own worker processes and its notify handler already ignores
// worker-origin notifications. Checking it here as well is a spawn-cost
// optimization, not the recursion guard.
const memoryWorkerOriginEnv = "ENTIRE_BRAIN_MEMORY_WORKER"

var (
	spawnDetached = execx.SpawnDetached
	lookPath      = exec.LookPath
)

// NotifyCheckpoint launches `entire brain memory notify --event checkpoint` as
// detached, best-effort work. The argv is deliberately limited to content-free
// routing metadata. Repository identity stays owned by Brain, which resolves
// it from repoRoot.
//
// NotifyCheckpoint has no result by design: a missing or failing Brain plugin
// must never change the host hook's result or output. Canonical reconciliation
// repairs a missed hint later. When the entire-brain plugin is not resolvable
// it skips the spawn entirely — main.go prepends the managed plugin dir to
// PATH, so a machine without Brain pays a LookPath, not a fork, per commit.
func NotifyCheckpoint(repoRoot, sessionID, branch string) {
	if os.Getenv(memoryWorkerOriginEnv) == "1" {
		return
	}
	if strings.TrimSpace(repoRoot) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	if _, err := lookPath("entire-brain"); err != nil {
		return
	}

	args := []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", sessionID,
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}

	spawnDetached(repoRoot, args...)
}
