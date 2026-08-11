package agent

import "context"

// UserHookSupport is an optional extension of HookSupport for agents whose
// hooks can also be installed at the USER level (home-directory config) — the
// activation surface behind global tracking (`entire enable --global`).
// Implement it only for agents with a verified user/repo dedup story (both
// scopes installed must never double-fire hooks); never fake it for agents
// without a real user-level surface. The full contract and the per-agent
// audit live in docs/architecture/agent-integration-checklist.md under
// "User-Level Hook Support (Global Tracking)".
type UserHookSupport interface {
	HookSupport

	// InstallUserHooks installs Entire's hooks in the agent's user-level
	// config. Idempotent: returns 0 when everything is already installed.
	InstallUserHooks(ctx context.Context) (int, error)

	// UninstallUserHooks removes Entire's hooks (and only Entire's) from the
	// agent's user-level config. A missing config file is not an error.
	UninstallUserHooks(ctx context.Context) error

	// AreUserHooksInstalled reports whether Entire's hooks are present in the
	// agent's user-level config.
	AreUserHooksInstalled(ctx context.Context) bool
}

// AsUserHookSupport returns the agent as UserHookSupport if it implements the
// interface. Built-in-only capability: external plugin agents have no
// user-level install protocol, so this resolves by type assertion alone.
func AsUserHookSupport(ag Agent) (UserHookSupport, bool) {
	return builtinCapability[UserHookSupport](ag)
}
