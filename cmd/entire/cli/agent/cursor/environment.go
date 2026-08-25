package cursor

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/cloudenv"
)

var _ agent.CloudCLIInstaller = (*CursorAgent)(nil)

// EnsureCloudCLIInstall patches an existing `.cursor/environment.json` so the
// Cloud Agent `install` phase puts `entire` on PATH. It never creates that
// file: a committed copy overrides dashboard-managed environments.
func (c *CursorAgent) EnsureCloudCLIInstall(ctx context.Context) (agent.CloudCLIInstallResult, error) {
	res, err := cloudenv.EnsureCursorEnvironment(ctx)
	if err != nil {
		return agent.CloudCLIInstallResult{}, err
	}
	return agent.CloudCLIInstallResult{Message: res.Message}, nil
}

// RemoveCloudCLIInstall strips the Entire install step from environment.json.
func (c *CursorAgent) RemoveCloudCLIInstall(ctx context.Context) error {
	return cloudenv.RemoveCursorEnvironment(ctx)
}
