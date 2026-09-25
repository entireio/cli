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

// TestOPFFlushCmd_RunsThroughRootAndLogsToFile guards two things at once that
// only the real root command wires up: that __opf_flush is registered at all
// (the pre-push path spawns it by name, so a missing registration is an
// immediate, silent no-op in production), and that the detached child — whose
// stdout/stderr are discarded — leaves its reasoning in
// .entire/logs/entire.log. Constructing the subcommand alone would exercise a
// wiring production never uses, because the root PersistentPreRunE is what
// opens the log file.
//
// OPF off is the case under test because it is the one that must still exit 0:
// a child that finds nothing to do is the overwhelmingly common outcome, and
// nothing watches its exit code.
func TestOPFFlushCmd_RunsThroughRootAndLogsToFile(t *testing.T) {
	setupStopTestRepo(t)
	markRepoSetUpForLogging(t)
	redact.ResetOPFConfigForTest()
	t.Cleanup(redact.ResetOPFConfigForTest)
	// The Once behind EnsureRedactionConfigured is process-global; without this
	// the assertion below would pass or fail on test order rather than on the
	// wiring under test.
	strategy.ResetRedactionConfiguredForTest()
	t.Cleanup(strategy.ResetRedactionConfiguredForTest)
	t.Setenv("ENTIRE_LOG_LEVEL", "debug")

	require.NoError(t, executeThroughRoot(t, "__opf_flush"),
		"the detached flush child must exit 0; it is best-effort and unwatched")

	root, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	logData, err := os.ReadFile(filepath.Join(root, ".entire", "logs", "entire.log"))
	require.NoError(t, err)
	require.Contains(t, string(logData), "opf flush skipped: OPF is not enabled",
		"a background flush that does nothing must say why in .entire/logs/entire.log")
}
