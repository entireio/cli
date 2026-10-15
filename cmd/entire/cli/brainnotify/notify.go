// Package brainnotify sends content-free lifecycle hints to the optional
// Entire Brain plugin without putting plugin work on a host hook's critical
// path.
package brainnotify

import (
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// memoryWorkerOriginEnv is owned by the entire-brain plugin: it sets the
// variable in its own worker processes and its notify handler already ignores
// worker-origin notifications. Checking it here as well is a spawn-cost
// optimization, not the recursion guard.
const memoryWorkerOriginEnv = "ENTIRE_BRAIN_MEMORY_WORKER"

var spawnDetached = execx.SpawnDetached

// NotifyCheckpoint launches `entire brain memory notify --event checkpoint` as
// detached, best-effort work. The argv is deliberately limited to content-free
// routing metadata. Repository identity stays owned by Brain, which resolves
// it from repoRoot.
//
// NotifyCheckpoint has no result by design: a missing or failing Brain plugin
// must never change the host hook's result or output. Canonical reconciliation
// repairs a missed hint later.
//
// There is deliberately NO plugin-presence probe here: built-in commands run
// after main.go's restorePATH(), so the managed plugin dir is NOT on this
// process's PATH and exec.LookPath cannot see a managed install — only the
// spawned `entire` child, whose own startup re-prepends that dir, can resolve
// the plugin. A machine without Brain pays one short-lived fork whose
// dispatcher exits on the unknown verb.
func NotifyCheckpoint(repoRoot, sessionID, branch string) {
	if os.Getenv(memoryWorkerOriginEnv) == "1" {
		return
	}
	repoRoot = strings.TrimSpace(repoRoot)
	sessionID = strings.TrimSpace(sessionID)
	branch = strings.TrimSpace(branch)
	if repoRoot == "" || sessionID == "" {
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
