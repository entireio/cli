// Package brainnotify sends content-free lifecycle hints to the optional
// Entire Brain plugin without putting plugin work on a host hook's critical
// path.
package brainnotify

import (
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// Event is a lifecycle event understood by `entire brain memory notify`.
type Event string

const (
	EventSessionStart Event = "session_start"
	EventCheckpoint   Event = "checkpoint"
	EventSessionEnd   Event = "session_end"

	memoryWorkerOriginEnv = "ENTIRE_BRAIN_MEMORY_WORKER"
)

var spawnDetached = execx.SpawnDetached

// Notify launches `entire brain memory notify` as detached, best-effort work.
// The argv is deliberately limited to content-free routing metadata. Repository
// identity stays owned by Brain, which resolves it from repoRoot.
//
// Notify has no result by design: a missing or failing Brain plugin must never
// change the host hook's result or output. Canonical reconciliation repairs a
// missed hint later.
func Notify(repoRoot string, event Event, sessionID, branch string) {
	if os.Getenv(memoryWorkerOriginEnv) == "1" {
		return
	}
	if strings.TrimSpace(repoRoot) == "" || strings.TrimSpace(sessionID) == "" || !event.valid() {
		return
	}

	args := []string{
		"brain", "memory", "notify",
		"--event", string(event),
		"--session", sessionID,
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}

	spawnDetached(repoRoot, args...)
}

func (e Event) valid() bool {
	switch e {
	case EventSessionStart, EventCheckpoint, EventSessionEnd:
		return true
	default:
		return false
	}
}
