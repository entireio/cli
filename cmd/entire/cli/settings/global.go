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
// tracked-file probe by injection at package initialization: a local settings
// file counts for activation only when it is provably this developer's — not
// in the git index and not reached through a symlink
// (probeLocalSettingsIsVersioned checks both; memoized per process).
var _ = installLocalSettingsProbe()

func installLocalSettingsProbe() struct{} {
	repopolicy.LocalSettingsTrusted = func(ctx context.Context, path string) bool {
		return classifyLocalSettings(ctx, path) != localTracked
	}
	return struct{}{}
}

// UserSettingsPath returns the user-global settings path.
func UserSettingsPath() string {
	return repopolicy.UserSettingsPath()
}

// LoadUserSettings strictly loads the user-global settings file.
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
