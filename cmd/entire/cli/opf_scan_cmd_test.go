package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/redact"
)

// TestOPFScanCmd_RunsThroughRootAndLogsToFile guards what only the real root
// command wires up: that __opf_scan is registered (pre-push spawns it by name,
// so a missing registration is a silent no-op in production) and that the
// detached child, whose stdout/stderr are discarded, leaves its reasoning in
// .entire/logs/entire.log. OPF off is the case under test because it must
// still exit 0: nothing watches the child's exit code.
func TestOPFScanCmd_RunsThroughRootAndLogsToFile(t *testing.T) {
	setupStopTestRepo(t)
	markRepoSetUpForLogging(t)
	redact.ResetOPFConfigForTest()
	t.Cleanup(redact.ResetOPFConfigForTest)
	strategy.ResetRedactionConfiguredForTest()
	t.Cleanup(strategy.ResetRedactionConfiguredForTest)
	t.Setenv("ENTIRE_LOG_LEVEL", "debug")

	require.NoError(t, executeThroughRoot(t, "__opf_scan", "origin"),
		"the detached scan child must exit 0; it is best-effort and unwatched")

	root, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	logData, err := os.ReadFile(filepath.Join(root, ".entire", "logs", "entire.log"))
	require.NoError(t, err)
	require.Contains(t, string(logData), "opf scan skipped: OPF is not enabled")
}
