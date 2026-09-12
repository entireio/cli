package settings

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// Compatibility aliases keep the public settings API stable while the leaf
// package owns the user-global schema and policy inputs.
type GlobalConfig = repopolicy.GlobalConfig
type UserSettings = repopolicy.UserSettings
type InactiveReason = repopolicy.InactiveReason

const (
	UserSettingsFileName         = repopolicy.UserSettingsFileName
	InactiveReasonNone           = repopolicy.InactiveReasonNone
	InactiveReasonRepoDisabled   = repopolicy.InactiveReasonRepoDisabled
	InactiveReasonGlobalExcluded = repopolicy.InactiveReasonGlobalExcluded
	InactiveReasonGlobalOff      = repopolicy.InactiveReasonGlobalOff
)

// The leaf cannot import this package (cycle through paths), so it takes the
// tracked-file probe by injection at package initialization. The three-state
// answer is passed through as-is: a proven-tracked file is ignored, a verified
// own file (not in the git index, not reached through a symlink —
// probeLocalSettingsIsVersioned checks both; memoized per process) may
// override the user's exclusions, and an unverifiable one keeps its settings
// but may not — the same asymmetry the merged loader applies to the OPF
// command (see localTrust).
var _ = installLocalSettingsProbe()

func installLocalSettingsProbe() struct{} {
	repopolicy.ClassifyLocalSettings = func(ctx context.Context, path string) repopolicy.LocalSettingsVerdict {
		switch classifyLocalSettings(ctx, path) {
		case localTracked:
			return repopolicy.LocalSettingsTracked
		case localOwn:
			return repopolicy.LocalSettingsOwn
		case localUnverifiable:
			return repopolicy.LocalSettingsUnverifiable
		}
		return repopolicy.LocalSettingsUnverifiable
	}
	return struct{}{}
}

// IsActiveAtRoot reports whether Entire captures sessions in the repository
// at root. It is the full form of "is this repo enabled?":
//
//	(the repo's own settings enable it)  OR  (global tracking covers it)
//
// with the qualifiers each half needs — a settings file that says
// enabled:false is a veto, a settings.local.json only counts when it is this
// developer's own untracked file, and the global tier only counts when the
// repo is not carved out by the user's exclude lists. A bare "does a settings
// file exist" check is the first half without its qualifiers and misses the
// second entirely (a globally tracked repo has no settings files), which is
// why callers that reason about a FOREIGN repo from a process running
// elsewhere (session binding adopting a session into a repo the agent
// touched) must use this. Activation only — no egress decision, which is
// cwd-scoped and would read the wrong repo's election. Errors fail closed.
func IsActiveAtRoot(ctx context.Context, root string) bool {
	policy, err := repopolicy.ClassifyActivationAt(ctx, root)
	return err == nil && policy.Active
}

// UserSettingsPath returns the user-global settings path.
func UserSettingsPath() string {
	return repopolicy.UserSettingsPath()
}

// LoadUserSettings loads the user-global settings file (per-block strictness:
// the global block rejects unknown keys, unknown top-level blocks are kept —
// see repopolicy.UserSettings).
func LoadUserSettings(ctx context.Context) (*UserSettings, error) {
	return repopolicy.LoadUserSettings(ctx) //nolint:wrapcheck // compatibility facade preserves the public error contract
}

// ModifyUserSettings atomically mutates the user-global settings file.
func ModifyUserSettings(ctx context.Context, fn func(*UserSettings) error) error {
	return repopolicy.ModifyUserSettings(ctx, fn) //nolint:wrapcheck // compatibility facade preserves the public error contract
}

// IsActiveForRepo reports whether repository-local or global policy activates
// tracking for the current worktree.
func IsActiveForRepo(ctx context.Context) bool {
	active, _ := IsActiveForRepoWithReason(ctx)
	return active
}

// GlobalTierEnabled reports the file-level global enable bit without resolving
// repository state.
func GlobalTierEnabled(ctx context.Context) bool {
	settings, err := LoadUserSettings(ctx)
	return err == nil && settings.GlobalEnabled()
}

// IsActiveForRepoWithReason classifies once (or reuses a hook-boundary
// snapshot). Any classification error is logged and reads as inactive.
func IsActiveForRepoWithReason(ctx context.Context) (bool, InactiveReason) {
	policy, err := currentPolicy(ctx)
	if err != nil {
		logging.Debug(ctx, "repository policy inactive (fail closed)", slog.String("error", err.Error()))
	}
	return policy.Active, policy.InactiveReason
}
