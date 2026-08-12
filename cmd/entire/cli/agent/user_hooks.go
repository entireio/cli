package agent

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

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
	// config. Idempotent: a fully current install is left untouched and
	// reported as the zero result.
	InstallUserHooks(ctx context.Context) (UserHookInstallResult, error)

	// UninstallUserHooks removes Entire's hooks (and only Entire's) from the
	// agent's user-level config. A missing config file is not an error.
	UninstallUserHooks(ctx context.Context) error

	// AreUserHooksInstalled reports whether Entire's hooks are present in the
	// agent's user-level config. A missing config file is (false, nil); a
	// config that cannot be read or parsed returns a non-nil error — callers
	// must not fold that into "not installed", which is how an EACCES-broken
	// config once read as a repo the installer should overwrite.
	AreUserHooksInstalled(ctx context.Context) (bool, error)
}

// UserHookInstallResult reports what a user-level hook install actually did,
// so callers can tell an untouched file from a rewritten one. The zero value
// means "already installed, nothing written".
type UserHookInstallResult struct {
	// Installed is the number of hook entries the install added that were not
	// already present in current form.
	Installed int
	// Repaired reports that pre-existing Entire entries were rewritten
	// (duplicates, alternate command forms, a partial install, or legacy
	// config fields normalized) — the file changed even when Installed is 0,
	// so "already installed" would be dishonest.
	Repaired bool
}

// AsUserHookSupport returns the agent as UserHookSupport if it implements the
// interface. Built-in-only capability: external plugin agents have no
// user-level install protocol, so this resolves by type assertion alone.
func AsUserHookSupport(ag Agent) (UserHookSupport, bool) {
	return builtinCapability[UserHookSupport](ag)
}

// UserHookAgent pairs a registered agent's name with its user-level hook
// support.
type UserHookAgent struct {
	Name    types.AgentName
	Support UserHookSupport
}

// UserHookSupports enumerates the registered, non-test agents by user-level
// hook capability, in sorted name order: supports are the agents implementing
// UserHookSupport, unsupported the remaining agent names. The single home for
// the List→Get→skip-test-only→AsUserHookSupport walk that enable/disable,
// status, and doctor all need.
func UserHookSupports() (supports []UserHookAgent, unsupported []types.AgentName) {
	for _, name := range List() {
		ag, err := Get(name)
		if err != nil {
			continue
		}
		if to, ok := ag.(TestOnly); ok && to.IsTestOnly() {
			continue
		}
		if uhs, ok := AsUserHookSupport(ag); ok {
			supports = append(supports, UserHookAgent{Name: name, Support: uhs})
		} else {
			unsupported = append(unsupported, name)
		}
	}
	return supports, unsupported
}
