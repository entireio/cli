package cli

import (
	"fmt"
	"log/slog"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

const gettingStarted = `

Getting Started:
  To get started with Entire CLI, run 'entire enable' to enable
  session tracking in your repository, then 'entire agent add <name>'
  to install hooks for a specific agent. For more information, visit:
  https://docs.entire.io/overview

`

const accessibilityHelp = `
Environment Variables:
  ACCESSIBLE    Set to any value (e.g., ACCESSIBLE=1) to enable accessibility
                mode. This uses simpler text prompts instead of interactive
                TUI elements, which works better with screen readers.
`

// Help groups for the root command. AddGroup order is display order.
// Visible commands without a GroupID render under "Additional Commands"
// (version, labs, agent-help, help) — that placement is intentional.
const (
	groupSetup        = "setup"
	groupSessions     = "sessions"
	groupAccount      = "account"
	groupControlPlane = "controlplane"
)

// inGroup assigns a help group to a command at registration time so all
// grouping stays visible in NewRootCmd rather than spread across constructors.
func inGroup(c *cobra.Command, groupID string) *cobra.Command {
	c.GroupID = groupID
	return c
}

// Run every ancestor's persistent hook, root first, not only the closest one
// cobra picks by default. Without this, the `checkpoint`, `session`, and `agent`
// pre-runs shadow the root's and it never builds a logger — silently, since the
// only symptom is missing log lines.
//
// Set in init() rather than NewRootCmd: it is a cobra package global, so writing
// it per construction races with cobra reading it during Execute (parallel tests
// do both at once).
//
//nolint:gochecknoinits // Set a cobra package global once, before any goroutine can read it (see above).
func init() {
	cobra.EnableTraverseRunHooks = true
}

// isShellCompletion reports whether this is one of cobra's hidden completion
// requests. The shell runs them on every TAB press, so they skip building a
// logger: MkdirAll + OpenFile + the settings read that resolves the level (which
// shells out to git) is real latency, and it left a 0-byte entire.log behind.
func isShellCompletion(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
	}
	return false
}

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "entire",
		Short:   "Entire CLI",
		Long:    "The command-line interface for Entire" + gettingStarted + accessibilityHelp,
		Version: versioninfo.Version,
		// Let main.go handle error printing to avoid duplication
		SilenceErrors: true,
		SilenceUsage:  true,
		// Hide completion command from help but keep it functional
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
		// PersistentPreRunE, not PersistentPreRun, so the `.entire` check can
		// stop the command. Every check below it reads or writes through
		// `.entire` — IsSetUpAny stats .entire/settings.json and ensureLogger
		// opens .entire/logs/entire.log — so the guard has to come first.
		// cobra.EnableTraverseRunHooks (set in init) runs parent hooks before
		// child ones, so this fires ahead of the group pre-runs and every RunE.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if isShellCompletion(cmd) {
				return nil
			}
			safe, err := checkEntireDirBeforeRun(cmd)
			if err != nil {
				return err
			}
			if !safe {
				return nil
			}
			if !settings.IsSetUpAny(cmd.Context()) {
				return nil
			}
			ensureLogger(cmd)
			return nil
		},
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			// Skip for hidden commands (walk parent chain — Cobra doesn't propagate Hidden)
			for c := cmd; c != nil; c = c.Parent() {
				if c.Hidden {
					return
				}
			}

			// Load settings once for telemetry and version check
			var telemetryEnabled *bool
			settings, err := LoadEntireSettings(cmd.Context())
			if err == nil {
				telemetryEnabled = settings.Telemetry
			}

			// Check if telemetry is enabled
			if telemetryEnabled != nil && *telemetryEnabled {
				// Use detached tracking (non-blocking)
				installedAgents := GetAgentsWithHooksInstalled(cmd.Context())
				agentStr := JoinAgentNames(installedAgents)
				telemetry.TrackCommandDetached(cmd, agentStr, settings.Enabled, versioninfo.Version)
			}

			// Version check and notification (synchronous with 2s timeout)
			// Runs AFTER command completes to avoid interfering with interactive modes.
			// Stderr, never stdout: this hook also fires after --json commands whose
			// stdout is piped into jq or captured by scripts — a notice on stdout
			// corrupts that output while staying invisible in the caller's logs.
			versioncheck.CheckAndNotify(cmd.Context(), cmd.ErrOrStderr(), versioninfo.Version)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// If we're in a git repo but Entire isn't set up yet, start the setup flow
			if _, err := paths.WorktreeRoot(ctx); err == nil && !settings.IsSetUpAny(ctx) {
				return runSetupFlow(ctx, cmd.OutOrStdout(), EnableOptions{})
			}
			return cmd.Help()
		},
	}

	addContextFlag(cmd)

	// Help groups; AddGroup order is display order in `entire --help`.
	cmd.AddGroup(
		&cobra.Group{ID: groupSetup, Title: "Entire Setup:"},
		&cobra.Group{ID: groupSessions, Title: "Sessions & Checkpoints:"},
		&cobra.Group{ID: groupAccount, Title: "Account:"},
		&cobra.Group{ID: groupControlPlane, Title: "Control Plane:"},
	)

	// Noun groups (canonical homes for subcommands).
	cmd.AddCommand(inGroup(newSessionsCmd(), groupSessions))                              // 'session' (with 'sessions' as Cobra alias)
	cmd.AddCommand(inGroup(newCheckpointGroupCmd(), groupSessions))                       // 'checkpoint' / 'cp' / 'checkpoints'
	experimental.Register(cmd, newTokensGroupCmd())                                       // 'tokens' (experimental)
	cmd.AddCommand(inGroup(newAgentGroupCmd(), groupSetup))                               // 'agent'
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newAuthCmd(), groupAccount)))         // 'auth'
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newDoctorCmd(), groupSetup)))         // 'doctor' (group: trace/logs/bundle)
	cmd.AddCommand(exemptFromEntireDirCheck(newLabsCmd()))                                // 'labs' (experimental workflow discovery)
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newPluginGroupCmd(), groupSetup)))    // 'plugin' (managed install/list/remove)
	experimental.Register(cmd, newImportCmd())                                            // 'import' (experimental; import pre-existing agent history)
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newOrgCmd(), groupControlPlane)))     // 'org' — control-plane org management
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newProjectCmd(), groupControlPlane))) // 'project' — control-plane project management
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newRepoCmd(), groupControlPlane)))    // 'repo' — control-plane repo lifecycle
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newGrantCmd(), groupControlPlane)))   // 'grant' — control-plane access grants

	// Top-level lifecycle and standalone commands.
	experimental.Register(cmd, cliReview.NewCommand(buildReviewDeps()))        // `review` (experimental)
	experimental.Register(cmd, investigate.NewCommand(buildInvestigateDeps())) // `investigate` (experimental); multi-agent investigation
	cmd.AddCommand(inGroup(newCleanCmd(), groupSetup))
	cmd.AddCommand(inGroup(newSetupCmd(), groupSetup)) // 'configure' — non-agent settings; agent CRUD lives under 'agent'
	cmd.AddCommand(inGroup(newEnableCmd(), groupSetup))
	cmd.AddCommand(inGroup(newDisableCmd(), groupSetup))
	cmd.AddCommand(inGroup(newStatusCmd(), groupSetup))
	experimental.Register(cmd, newBlameCmd()) // 'blame' (experimental)
	experimental.Register(cmd, newWhyCmd())   // 'why' (experimental)
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newLoginCmd(), groupAccount)))
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newLogoutCmd(), groupAccount)))
	cmd.AddCommand(exemptFromEntireDirCheck(newVersionCmd()))
	cmd.AddCommand(inGroup(newDispatchCmd(), groupSessions))
	cmd.AddCommand(inGroup(newActivityCmd(), groupSessions))
	cmd.AddCommand(inGroup(newRecapCmd(), groupSessions))
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newAPICmd(), groupControlPlane)))           // authenticated passthrough to core/cell APIs
	cmd.AddCommand(newAgentHelpCmd(cmd))                                                        // visible: agents on transports without context injection discover it via `entire help`
	cmd.AddCommand(inGroup(newSearchCmd(), groupSessions))                                      // 'search' — canonical top-level spelling; 'checkpoint search' stays a working alias
	cmd.AddCommand(exemptFromEntireDirCheck(inGroup(newTopLevelCloneCmd(), groupControlPlane))) // 'clone' — canonical top-level spelling; 'repo clone' stays a working alias

	// Experimental labs commands (listed via `entire labs`; not deprecation shortcuts).
	experimental.Register(cmd, newExpertsCmd()) // 'experts' (experimental); agent/workflow provenance

	// Hidden infrastructure.
	cmd.AddCommand(newMCPCmd(cmd)) // MCP stdio server for MCP-host agents
	cmd.AddCommand(newHooksCmd())
	cmd.AddCommand(newTrailCmd())
	cmd.AddCommand(newSendAnalyticsCmd())
	cmd.AddCommand(newCurlBashPostInstallCmd())
	cmd.AddCommand(newRefreshTrailEnablementCmd())
	cmd.AddCommand(newSweepSessionsCmd())

	// Experimental command (developer-only visibility; setup/tune runners).
	experimental.Register(cmd, newRunnerCmd()) // 'runner' (experimental)

	cmd.SetVersionTemplate(versionString())

	// Replace default help command with custom one that supports -t flag
	cmd.SetHelpCommand(NewHelpCmd(cmd))

	// Materialize cobra's `completion` command now, so it can be exempted from
	// the `.entire` check. It prints a shell script and reads nothing from the
	// repo, and users eval it from a shell rc — a broken repo must not be able
	// to break someone's shell startup. Cobra calls this again during
	// ExecuteC, where it returns early because the command already exists.
	cmd.InitDefaultCompletionCmd()
	for _, c := range cmd.Commands() {
		if c.Name() == "completion" {
			exemptFromEntireDirCheck(c)
		}
	}

	return cmd
}

func versionString() string {
	return fmt.Sprintf("Entire CLI %s\nGo version: %s\nOS/Arch: %s/%s\n",
		versioninfo.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show build information",
		Run: func(cmd *cobra.Command, _ []string) {
			// Use OutOrStdout explicitly — cobra's cmd.Print() defaults to
			// stderr in v1.10+, but version output should go to stdout.
			fmt.Fprint(cmd.OutOrStdout(), versionString())
		},
	}
}

// newSendAnalyticsCmd creates the hidden command for sending analytics from a detached subprocess.
// This command is invoked by TrackCommandDetached and should not be called directly by users.
func newSendAnalyticsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__send_analytics",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			telemetry.SendEvents(args[0])
		},
	}
}

// newSweepSessionsCmd creates the hidden command the session-start hook
// spawns detached to fix zombie sessions in the background (see
// runSessionSweep). Not for direct use; `entire doctor` is the interactive
// surface for the same repairs.
func newSweepSessionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__sweep_sessions",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Detached child with discarded stdout/stderr: make sure a file
			// logger is attached so a failing background sweep (e.g. a zombie
			// that can't self-heal) is diagnosable in .entire/logs/entire.log
			// rather than vanishing. Idempotent, and it resolves the worktree
			// root itself — a child whose worktree was removed between spawn
			// and exec gets no logger rather than a stray .entire/logs/ in an
			// arbitrary directory. The root PersistentPostRun closes whichever
			// logger ends up on the context.
			ensureLogger(cmd)
			ctx := cmd.Context()
			// Log the top-level error too: main.go prints RunE errors to
			// stderr, which is io.Discard for a detached child — without this
			// line a sweep that fails before its loop leaves no trace.
			if err := runSessionSweep(ctx); err != nil {
				logging.Error(ctx, "session sweep failed", slog.String("error", err.Error()))
				return err
			}
			return nil
		},
	}
}
