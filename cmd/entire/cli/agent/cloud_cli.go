package agent

import "context"

// CloudCLIInstaller is implemented by agents that boot in a remote environment
// that does not inherit the user's local PATH (Cursor Cloud Agents today).
// Enable uses it so committed hook commands that name `entire` actually run.
//
// Implementations must be idempotent and must not create an environment config
// that would override a dashboard-managed environment.
type CloudCLIInstaller interface {
	EnsureCloudCLIInstall(ctx context.Context) (CloudCLIInstallResult, error)
	RemoveCloudCLIInstall(ctx context.Context) error
}

// CloudCLIInstallResult is printed by `entire enable` when an agent wired (or
// could not auto-wire) the Entire CLI into a remote environment.
type CloudCLIInstallResult struct {
	Message string
}
