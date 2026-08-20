package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// probeMarker proves a line reached the log file, not just that a logger existed.
const probeMarker = "root prerun injected this logger"

// markRepoSetUpForLogging satisfies the root pre-run's IsSetUpAny gate.
func markRepoSetUpForLogging(t *testing.T) {
	t.Helper()

	if err := os.MkdirAll(paths.EntireDir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", paths.EntireDir, err)
	}
	if err := os.WriteFile(filepath.Join(paths.EntireDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

// executeThroughRoot runs args through a real root command — the only way to
// exercise the pre-run that builds the logger and the post-run that flushes it.
func executeThroughRoot(t *testing.T, args ...string) error {
	t.Helper()

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	return root.ExecuteContext(context.Background())
}

// setUpRepoForRootLogging makes cwd a set-up git repo. setupStopTestRepo also
// clears the worktree-root and git-common-dir caches, without which a path
// cached by an earlier test resolves these t.Chdir tests against the wrong repo.
func setUpRepoForRootLogging(t *testing.T) string {
	t.Helper()

	setupStopTestRepo(t)
	dir := mustGetwd(t)
	markRepoSetUpForLogging(t)
	return dir
}

// runProbeUnder attaches a probe under the given command path, runs it through
// the real root, and reports the logger its RunE saw — writing probeMarker while
// the file is still open, since the post-run closes on the way out.
//
// Hidden so the post-run's Hidden walk short-circuits before its telemetry and
// version-check calls, which would otherwise hit the network mid-test.
func runProbeUnder(t *testing.T, parents ...string) *logging.Logger {
	t.Helper()

	root := NewRootCmd()
	parent := root
	if len(parents) > 0 {
		found, _, err := root.Find(parents)
		if err != nil {
			t.Fatalf("root.Find(%v): %v", parents, err)
		}
		parent = found
	}

	var observed *logging.Logger
	probe := &cobra.Command{
		Use:    "__root_logging_probe",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			observed = logging.LoggerFromContext(cmd.Context())
			if observed != nil {
				observed.Slog().Warn(probeMarker)
			}
			return nil
		},
	}
	parent.AddCommand(probe)

	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append(append([]string{}, parents...), probe.Use))
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v %s: %v", parents, probe.Use, err)
	}
	return observed
}

// Every command depends on this instead of building a logger itself: after the
// root pre-run, the executing command's context carries one, and lines written
// through it land in .entire/logs/entire.log.
func TestRootPreRun_InjectsLoggerIntoCommandContext(t *testing.T) {
	dir := setUpRepoForRootLogging(t)

	if runProbeUnder(t) == nil {
		t.Fatal("LoggerFromContext() = nil after the root PersistentPreRunE ran")
	}

	content, err := os.ReadFile(filepath.Join(dir, paths.EntireDir, "logs", "entire.log"))
	if err != nil {
		t.Fatalf("read entire.log: %v", err)
	}
	if !bytes.Contains(content, []byte(probeMarker)) {
		t.Errorf("injected logger did not write to entire.log: %s", content)
	}
}

// Pins the dependency on cobra.EnableTraverseRunHooks: these groups define
// their own pre-run, which under cobra's default shadows the root's entirely.
// The failure is silent — every command under them logs to stderr again.
func TestRootPreRun_ReachesLeafUnderGroupWithOwnPreRun(t *testing.T) {
	setUpRepoForRootLogging(t)

	for _, group := range []string{"checkpoint", "session", "agent"} {
		t.Run(group, func(t *testing.T) {
			if runProbeUnder(t, group) == nil {
				t.Errorf("LoggerFromContext() = nil for a leaf under %q, which defines its own PersistentPreRunE", group)
			}
		})
	}
}

// Building a logger CREATES .entire/logs/, so a repo that never set Entire up
// must come out of an unrelated command untouched — not seeded with an
// untracked directory no gitignore entry covers yet.
func TestInitRootLogging_SkipsRepoThatNeverEnabledEntire(t *testing.T) {
	setupStopTestRepo(t) // deliberately not marked set up
	dir := mustGetwd(t)

	if runProbeUnder(t) != nil {
		t.Error("LoggerFromContext() must be nil in a repo that never enabled Entire")
	}
	if _, err := os.Stat(filepath.Join(dir, paths.EntireDir)); !os.IsNotExist(err) {
		t.Errorf(".entire/ must not be created in a repo that never enabled Entire (stat err = %v)", err)
	}
}

// Pins the flush main.go depends on: cobra returns out of execute() as soon as
// RunE errors or required-flag validation fails, both before its post-run loop,
// so root's flush never runs and the command ExecuteContextC hands back is the
// only route to the buffered lines. (Errors raised before any pre-run carry no
// logger, so there is nothing to lose.)
func TestExecutedCommandCarriesLoggerOnFailure(t *testing.T) {
	tests := []struct {
		name          string
		requireFlag   bool
		runE          func(cmd *cobra.Command, args []string) error
		wantErrSubstr string
	}{
		{
			name: "RunE returns an error",
			runE: func(cmd *cobra.Command, _ []string) error {
				logging.Warn(cmd.Context(), probeMarker)
				return errors.New("probe failed")
			},
			wantErrSubstr: "probe failed",
		},
		{
			// Validated after the pre-runs, so a logger exists and has buffered
			// content by the time cobra bails out.
			name:        "required flag missing",
			requireFlag: true,
			runE: func(*cobra.Command, []string) error {
				return errors.New("RunE must not be reached")
			},
			wantErrSubstr: `required flag(s) "must" not set`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setUpRepoForRootLogging(t)

			root := NewRootCmd()
			probe := &cobra.Command{
				Use:    "__root_logging_failure_probe",
				Hidden: true,
				RunE:   tt.runE,
			}
			if tt.requireFlag {
				// RunE is never reached on this path.
				probe.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
					logging.Warn(cmd.Context(), probeMarker)
				}
				probe.Flags().String("must", "", "a required flag")
				if err := probe.MarkFlagRequired("must"); err != nil {
					t.Fatalf("MarkFlagRequired: %v", err)
				}
			}
			root.AddCommand(probe)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{probe.Use})

			executed, err := root.ExecuteContextC(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErrSubstr)
			}
			if executed == nil {
				t.Fatal("ExecuteContextC returned no command; main.go would have nothing to close")
			}

			logFile := filepath.Join(dir, paths.EntireDir, "logs", "entire.log")

			// Exactly what main.go does after ExecuteContextC. Nothing else can
			// have flushed: cobra skips its PostRun loop on both these paths.
			if closeErr := logging.LoggerFromContext(executed.Context()).Close(); closeErr != nil {
				t.Fatalf("Close() error = %v", closeErr)
			}

			flushed, readErr := os.ReadFile(logFile)
			if readErr != nil {
				t.Fatalf("read entire.log after close: %v", readErr)
			}
			if !bytes.Contains(flushed, []byte(probeMarker)) {
				t.Errorf("line logged before the failure was lost: %s", flushed)
			}
		})
	}
}

// TestShellCompletion_BuildsNoLogger pins that cobra's hidden completion
// requests skip logger construction. The shell runs them on every TAB press, so
// they must not pay MkdirAll + OpenFile plus the settings read that resolves the
// level (which shells out to git) — and they used to leave a 0-byte entire.log
// behind in any repo where nothing had logged yet.
func TestShellCompletion_BuildsNoLogger(t *testing.T) {
	dir := setUpRepoForRootLogging(t)

	// Set up, so only the completion gate can prevent the logger.
	if err := executeThroughRoot(t, cobra.ShellCompRequestCmd, ""); err != nil {
		t.Fatalf("execute %s: %v", cobra.ShellCompRequestCmd, err)
	}

	if _, err := os.Stat(filepath.Join(dir, paths.EntireDir, "logs")); !os.IsNotExist(err) {
		t.Errorf(".entire/logs must not be created by shell completion (stat err = %v)", err)
	}
}
