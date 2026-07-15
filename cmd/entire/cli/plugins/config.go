package plugins

import "time"

// Timeouts bounding plugin execution. Kept small: plugins are event callbacks
// on the agent's critical path (a git commit, a turn boundary), so a slow or
// runaway plugin must never stall the host. They are enforced via the Lua
// state's context, which aborts a running script between VM instructions.
const (
	// loadTimeout bounds the one-time entry-script execution at load. The entry
	// only registers hooks/commands, so this is generous headroom.
	loadTimeout = 5 * time.Second

	// observerHookTimeout bounds a single observer callback invocation.
	observerHookTimeout = 2 * time.Second
)
