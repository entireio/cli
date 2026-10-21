package agent

import "sync/atomic"

// homeProbesEnabled gates agents that can ask their own CLI where they keep
// per-user state, for the case the environment alone cannot answer.
//
// Claude Code honors a CLAUDE_CONFIG_DIR set in the env block of its settings
// files, which ResolveHome cannot see from a plain shell; only `claude` itself
// knows. Asking spawns the agent CLI, so it is opt-in: a command that runs from
// the user's shell and has to put or find the agent's files without a path from
// the agent (session resume, trail resume, attach) enables it, and the agent
// asks at most once, the first time its home is actually resolved. Hooks never
// enable it and need not: they run as the agent's children, and the agent
// exports its settings env to them.
var homeProbesEnabled atomic.Bool

// EnableHomeProbes lets agents ask their own CLI for their home for the rest of
// the process. See homeProbesEnabled.
func EnableHomeProbes() { homeProbesEnabled.Store(true) }

// HomeProbesEnabled reports whether EnableHomeProbes has been called.
func HomeProbesEnabled() bool { return homeProbesEnabled.Load() }
