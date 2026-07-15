package plugins

import (
	"os"
	"strconv"
	"time"
)

// Timeouts bounding plugin execution. Kept small: plugins are event callbacks
// on the agent's critical path (a git commit, a turn boundary), so a slow or
// runaway plugin must never stall the host. They are enforced via the Lua
// state's context, which aborts a running script between VM instructions.
const (
	// loadTimeout bounds the one-time entry-script execution at load. The entry
	// only registers hooks/commands, so this is generous headroom.
	loadTimeout = 5 * time.Second

	// observerHookTimeout bounds a single observer callback invocation. Can be
	// raised via ENTIRE_PLUGIN_HOOK_TIMEOUT_MS for plugins that make slower
	// capability calls (http/exec).
	observerHookTimeout = 2 * time.Second

	// Capability call limits. These bound the blast radius of the privileged
	// APIs even for an allow-listed plugin.
	httpCapTimeout       = 10 * time.Second
	httpMaxResponseBytes = 5 << 20 // 5 MiB
	execCapTimeout       = 30 * time.Second
	fsMaxReadBytes       = 10 << 20 // 10 MiB
)

// hookTimeoutEnv overrides observerHookTimeout when set to a positive integer
// number of milliseconds.
const hookTimeoutEnv = "ENTIRE_PLUGIN_HOOK_TIMEOUT_MS"

// pluginsDisabledEnv is a process-wide kill switch: when set to 1/true, no
// plugin is loaded regardless of the allow-list.
const pluginsDisabledEnv = "ENTIRE_PLUGINS_DISABLED"

// hookTimeout returns the effective per-hook timeout, honoring the env override.
func hookTimeout() time.Duration {
	if v := os.Getenv(hookTimeoutEnv); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return observerHookTimeout
}

// pluginsDisabledByEnv reports whether the kill switch is set.
func pluginsDisabledByEnv() bool {
	v := os.Getenv(pluginsDisabledEnv)
	return v == "1" || v == "true"
}
