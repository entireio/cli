package settings

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
	if err := repopolicy.ModifyUserSettings(ctx, fn); err != nil {
		return err //nolint:wrapcheck // compatibility facade preserves the public error contract
	}
	// Runtime routing still lives in paths during this compatibility stage.
	paths.ClearInvisibleRuntimeCache()
	return nil
}

// ClearGlobalModeCache clears leaf-owned global classification state.
func ClearGlobalModeCache() {
	repopolicy.ClearGlobalModeCache()
}

func globalModeStatus(ctx context.Context) (bool, InactiveReason) {
	policy, err := repopolicy.GlobalModeStatus(ctx)
	if err != nil {
		logging.Debug(ctx, "global repository policy inactive (fail closed)", slog.String("error", err.Error()))
	}
	return policy.Active, policy.InactiveReason
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

// IsActiveForRepoWithReason returns the existing repository-local override
// result before falling back to leaf-owned global classification.
func IsActiveForRepoWithReason(ctx context.Context) (bool, InactiveReason) {
	if policy, ok := repopolicy.RepoPolicyFromContext(ctx); ok {
		return policy.Active, policy.InactiveReason
	}
	configured, err := RepoActivationConfigured(ctx)
	if err != nil {
		logging.Debug(ctx, "repo settings unreadable; treating repo as inactive", slog.String("error", err.Error()))
		return false, InactiveReasonRepoDisabled
	}
	if configured {
		if repoSettingsEnabled(ctx) {
			return true, InactiveReasonNone
		}
		return false, InactiveReasonRepoDisabled
	}
	return globalModeStatus(ctx)
}
