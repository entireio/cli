// hook_registry.go provides hook command registration for agents.
// The lifecycle dispatcher (DispatchLifecycleEvent) handles all lifecycle events.
// PostTodo is the only hook that's handled directly (not via lifecycle dispatcher).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"

	"github.com/spf13/cobra"
)

// agentHookLogCleanup stores the cleanup function for agent hook logging.
// Set by PersistentPreRunE, called by PersistentPostRunE.
var agentHookLogCleanup func()

// currentHookAgentName stores the agent name for the currently executing hook.
// Set by newAgentHookVerbCmdWithLogging before calling the handler.
// This allows handlers to know which agent invoked the hook without guessing.
var currentHookAgentName types.AgentName

// GetCurrentHookAgent returns the agent for the currently executing hook.
// Returns the agent based on the hook command structure (e.g., "entire hooks claude-code ...")
// rather than guessing from directory presence.
// Falls back to GetAgent() if not in a hook context.
func GetCurrentHookAgent() (agent.Agent, error) {
	if currentHookAgentName == "" {
		return nil, errors.New("not in a hook context: agent name not set")
	}

	ag, err := agent.Get(currentHookAgentName)
	if err != nil {
		return nil, fmt.Errorf("getting hook agent %q: %w", currentHookAgentName, err)
	}
	return ag, nil
}

// newAgentHooksCmd creates a hooks subcommand for an agent that implements HookSupport.
// It dynamically creates subcommands for each hook the agent supports.
func newAgentHooksCmd(agentName types.AgentName, handler agent.HookSupport) *cobra.Command {
	cmd := &cobra.Command{
		Use:    string(agentName),
		Short:  handler.Description() + " hook handlers",
		Hidden: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			agentHookLogCleanup = initHookLogging(cmd.Context())
			return nil
		},
		PersistentPostRunE: func(_ *cobra.Command, _ []string) error {
			if agentHookLogCleanup != nil {
				agentHookLogCleanup()
			}
			return nil
		},
	}

	for _, hookName := range handler.HookNames() {
		cmd.AddCommand(newAgentHookVerbCmdWithLogging(agentName, hookName))
	}

	return cmd
}

// getHookType returns the hook type based on the hook name.
// Returns "subagent" for task-related hooks (pre-task, post-task, post-todo),
// "tool" for tool-related hooks (before-tool, after-tool),
// "agent" for all other agent hooks.
func getHookType(hookName string) string {
	switch hookName {
	case claudecode.HookNamePreTask, claudecode.HookNamePostTask, claudecode.HookNamePostTodo:
		return "subagent"
	case geminicli.HookNameBeforeTool, geminicli.HookNameAfterTool:
		return "tool"
	default:
		return "agent" //nolint:goconst // hook-type label; unrelated to the --agent flag name constant
	}
}

// executeAgentHook runs the core hook execution logic for a given agent and hook name.
// It handles git repo checks, enabled checks, logging, event parsing, and lifecycle dispatch.
// Used by both the registered subcommand path and the RunE fallback for external agents.
// When initLogging is true, it initializes and cleans up hook logging (used by the RunE fallback
// since it doesn't go through PersistentPreRunE). Built-in agent subcommands pass false since
// their parent command's PersistentPreRunE already handles logging.
func executeAgentHook(cmd *cobra.Command, agentName types.AgentName, hookName string, initLogging bool) error {
	// Skip if not in a git repository - hooks shouldn't prevent the agent
	// from working. On SessionStart only, and only when the user opted in to
	// global tracking (tier configured AND enabled), leave a one-line notice:
	// that is the literal wrong-folder case — the user asked for machine-wide
	// tracking and this location can't be tracked. With the tier off or
	// unconfigured, silence: explicit off is a durable answer, exactly like
	// the repo-level disable veto below. Verb check first so the user-settings
	// read happens only on the cold session-start-outside-a-repo path.
	worktreeRoot, err := paths.WorktreeRoot(cmd.Context())
	if err != nil {
		if hookName == sessionStartHookVerb && settings.GlobalTierEnabled(cmd.Context()) {
			warnInactiveOnSessionStart(cmd.ErrOrStderr(), hookName, notGitRepoSessionStartNotice)
		}
		return nil
	}

	// Skip if Entire is not set up and enabled. This must fail closed: any
	// settings read error (missing file, corrupted JSON, transient I/O
	// failure) is treated as disabled so a hook never silently falls through
	// to full lifecycle work just because settings couldn't be read. Using
	// IsEnabled here previously failed OPEN on error (`err == nil && !enabled`
	// only short-circuits when the read succeeded), which meant a corrupted
	// or unreadable settings file made every hook invocation pay the full
	// dispatch cost instead of exiting fast (#524).
	// settings.IsActiveForRepo(WithReason) is the same fail-closed gate the
	// git hooks use (see PersistentPreRunE in hooks_git_cmd.go). It extends
	// IsSetUpAndEnabled with the user-global tier: repos with no repo-level
	// setup proceed when global mode is on and the repo is not excluded.
	if active, reason := settings.IsActiveForRepoWithReason(cmd.Context()); !active {
		warnInactiveOnSessionStart(cmd.ErrOrStderr(), hookName, inactiveSessionStartNotice(reason))
		return nil
	}

	if initLogging {
		cleanup := initHookLogging(cmd.Context())
		defer cleanup()
	}

	// Lazy invisible setup for globally tracked repos (no repo-level setup,
	// user-global tier active): first hook activity installs the git hooks
	// (skipped when core.hooksPath resolves inside the worktree — see
	// MaybeEnsureGlobalSetup) and seeds the checkpoint metadata ref; nothing
	// it does creates a worktree file. Cheap no-op once the clone-prefs
	// marker is set or when repo-level setup exists. Runs after logging is
	// initialized — on the built-in agent route the parent command's
	// PersistentPreRunE did that, on the RunE fallback the block above did —
	// so its Debug-level failure ladder is recorded. The git-hook route
	// triggers the same setup (hooks_git_cmd.go).
	strategy.MaybeEnsureGlobalSetup(cmd.Context())

	// Initialize logging context with agent name
	ctx := logging.WithAgent(logging.WithComponent(cmd.Context(), "hooks"), agentName)

	// Strategy name for logging
	strategyName := strategy.StrategyNameManualCommit

	hookType := getHookType(hookName)

	// Start root perf span — child spans in lifecycle handlers and strategy
	// methods will automatically nest under this span.
	ctx, span := perf.Start(ctx, hookName,
		slog.String("hook_type", hookType))
	defer span.End()

	logging.Debug(ctx, "hook invoked",
		slog.String("hook", hookName),
		slog.String("hook_type", hookType),
		slog.String("strategy", strategyName),
	)

	// Set the current hook agent so handlers can retrieve it
	currentHookAgentName = agentName
	defer func() { currentHookAgentName = "" }()

	// Use the lifecycle dispatcher for all hooks
	var hookErr error
	ag, agentErr := agent.Get(agentName)
	if agentErr != nil {
		return fmt.Errorf("failed to get agent %q: %w", agentName, agentErr)
	}

	handler, ok := agent.AsHookSupport(ag)
	if !ok {
		return fmt.Errorf("agent %q does not support hooks", agentName)
	}

	// Use cmd.InOrStdin() to support testing with cmd.SetIn()
	event, parseErr := handler.ParseHookEvent(ctx, hookName, cmd.InOrStdin())
	if parseErr != nil {
		return fmt.Errorf("failed to parse hook event: %w", parseErr)
	}

	claudePostTodoCheckpointHook := event == nil && agentName == agent.AgentNameClaudeCode && hookName == claudecode.HookNamePostTodo
	eventType := agent.EventType(0)

	if event != nil {
		// Cross-agent guard: when Cursor IDE invokes a hook configured under
		// .claude/settings.json (because .cursor/hooks.json is missing), the
		// hook payload's transcript_path proves the session belongs to Cursor.
		// Skip dispatch so the session isn't claimed for the wrong agent (#1262).
		if shouldSkipForwardedHook(ctx, ag, event) {
			logging.Debug(ctx, "skipping forwarded hook: transcript belongs to another agent",
				slog.String("hook", hookName),
				slog.String("firing_agent", string(agentName)),
				slog.String("session_ref", event.SessionRef),
			)
			return nil
		}
		eventType = event.Type
	}

	if eventType == agent.SessionStart {
		skipSessionStart, err := shouldSkipSessionStartForPolicy(ctx, cmd.ErrOrStderr(), agentName, ag, worktreeRoot)
		if err != nil {
			span.RecordError(err)
			return err
		}
		if skipSessionStart {
			return nil
		}
	} else if hookWritesCheckpointData(eventType, claudePostTodoCheckpointHook) {
		writeHook := agentWriteHookLabel(eventType, claudePostTodoCheckpointHook)
		if err := rejectUnsupportedCheckpointWritePolicy(ctx, cmd.ErrOrStderr(), agentName, writeHook, worktreeRoot); err != nil {
			span.RecordError(err)
			return err
		}
	}

	if event != nil {
		// Lifecycle event — use the generic dispatcher
		hookErr = DispatchLifecycleEvent(ctx, ag, event)
	} else if claudePostTodoCheckpointHook {
		// PostTodo is Claude-specific: creates incremental checkpoints during subagent execution
		hookErr = handleClaudeCodePostTodo(ctx)
	}
	// Other pass-through hooks (nil event, no special handling) are no-ops

	span.RecordError(hookErr)
	return hookErr
}

// sessionStartHookVerb is the hook verb most built-in agents use for their
// session-start hook (see each agent's HookNameSessionStart). It is NOT
// universal: pi uses "session_start" (agent/pi/lifecycle.go mirrors Pi's
// native snake_case event names), so pi sessions never get the
// inactive-location notice — an accepted gap. A non-matching verb is the
// safe direction: the notice below stays off rather than firing on every
// hook.
const sessionStartHookVerb = "session-start"

// notGitRepoSessionStartNotice is the SessionStart notice for hooks firing
// outside any git repository. Emitted only when the global tier is configured
// AND enabled (see the gate in executeAgentHook): that combination means the
// user opted in to machine-wide tracking and this location cannot provide it
// — the literal wrong-folder case.
const notGitRepoSessionStartNotice = "entire: not tracking this session (not a git repo)"

// inactiveSessionStartNotice maps the gate's inactive reason to the one-line
// SessionStart notice, or "" for reasons that must stay silent. The notice
// exists to explain why an opted-in user's session is NOT tracked here, so it
// fires only when the global tier is on and this location is carved out
// (excluded repo; the not-a-git-repo case is gated before the reason variant
// runs). Note InactiveReasonGlobalExcluded also covers the fail-closed
// could-not-verify shapes (unusable exclude pattern, unparseable origin) —
// the "repo excluded by global settings" wording is a slight simplification
// for those, kept because the remedy (fix the exclude config) is the same.
// Explicit off is a durable answer and means silence in both scopes:
// InactiveReasonRepoDisabled (the repo-level veto) and
// InactiveReasonGlobalOff (the user answered no to — or later disabled —
// machine-wide tracking). Nagging a globally-off user to re-enable on every
// SessionStart would also contradict disable --global's "inert while global
// tracking is off" report about user-level hooks left in place.
func inactiveSessionStartNotice(reason settings.InactiveReason) string {
	switch reason {
	case settings.InactiveReasonGlobalExcluded:
		return "entire: not tracking this session (repo excluded by global settings)"
	case settings.InactiveReasonNone, settings.InactiveReasonRepoDisabled, settings.InactiveReasonGlobalOff:
		return ""
	default:
		return ""
	}
}

// warnInactiveOnSessionStart writes the inactive-location notice to the
// agent-hook stderr stream — and ONLY for the session-start verb, never on
// every hook, and never for an empty notice. This function is reached only
// from the agent-hook route (executeAgentHook); git hooks gate in
// hooks_git_cmd.go and never produce this notice, so git output stays clean.
func warnInactiveOnSessionStart(errW io.Writer, hookName, notice string) {
	if hookName != sessionStartHookVerb || notice == "" {
		return
	}
	fmt.Fprintln(errW, notice)
}

func agentHookPolicy(ctx context.Context, worktreeRoot string) (checkpointpolicy.Policy, error) {
	repo, err := gitrepo.OpenPath(worktreeRoot)
	if err != nil {
		return checkpointpolicy.Policy{}, unreadableCheckpointPolicyError(err)
	}
	defer repo.Close()

	return checkpointPolicyForCheckpointData(ctx, repo)
}

func shouldSkipAgentHookForPolicy(policy checkpointpolicy.Policy) bool {
	return !checkpointpolicy.CanSatisfyPolicy(policy)
}

func shouldSkipSessionStartForPolicy(ctx context.Context, errW io.Writer, agentName types.AgentName, ag agent.Agent, worktreeRoot string) (bool, error) {
	policy, err := agentHookPolicy(ctx, worktreeRoot)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy read failed for agent hook",
			slog.String("error", err.Error()))
		emitCheckpointPolicyBlocked(ctx, telemetry.CheckpointPolicyBlockedEvent{
			Hook:     "session-start",
			HookType: telemetry.PolicyBlockedHookTypeAgent,
			Reason:   telemetry.PolicyBlockedReasonUnreadable,
			Outcome:  telemetry.PolicyBlockedOutcomeSkipped,
			Agent:    string(agentName),
		})
		// Let the agent start; the warning explains that checkpoint capture is
		// disabled until the policy can be read.
		return true, writeUnsupportedPolicySessionStartWarning(errW, ag, sessionStartPolicyReadErrorWarning(err))
	}
	if shouldSkipAgentHookForPolicy(policy) {
		emitCheckpointPolicyBlocked(ctx, telemetry.CheckpointPolicyBlockedEvent{
			Hook:                 "session-start",
			HookType:             telemetry.PolicyBlockedHookTypeAgent,
			Reason:               telemetry.PolicyBlockedReasonUnsupported,
			Outcome:              telemetry.PolicyBlockedOutcomeSkipped,
			Agent:                string(agentName),
			CheckpointVersion:    policy.CheckpointVersion,
			CheckpointMinVersion: policy.CheckpointMinVersion,
		})
		// Let the agent start; the warning explains that checkpoint capture is
		// disabled until the CLI is upgraded.
		return true, writeUnsupportedPolicySessionStartWarning(errW, ag, sessionStartPolicyWarning(policy))
	}
	return false, nil
}

func rejectUnsupportedCheckpointWritePolicy(ctx context.Context, errW io.Writer, agentName types.AgentName, hook string, worktreeRoot string) error {
	policy, err := agentHookPolicy(ctx, worktreeRoot)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy read failed for agent hook",
			slog.String("error", err.Error()))
		emitCheckpointPolicyBlocked(ctx, telemetry.CheckpointPolicyBlockedEvent{
			Hook:     hook,
			HookType: telemetry.PolicyBlockedHookTypeAgent,
			Reason:   telemetry.PolicyBlockedReasonUnreadable,
			Outcome:  telemetry.PolicyBlockedOutcomeBlocked,
			Agent:    string(agentName),
		})
		fmt.Fprint(errW, agentCheckpointCaptureDisabledReadErrorMessage(err))
		return NewSilentError(err)
	}
	if shouldSkipAgentHookForPolicy(policy) {
		emitCheckpointPolicyBlocked(ctx, telemetry.CheckpointPolicyBlockedEvent{
			Hook:                 hook,
			HookType:             telemetry.PolicyBlockedHookTypeAgent,
			Reason:               telemetry.PolicyBlockedReasonUnsupported,
			Outcome:              telemetry.PolicyBlockedOutcomeBlocked,
			Agent:                string(agentName),
			CheckpointVersion:    policy.CheckpointVersion,
			CheckpointMinVersion: policy.CheckpointMinVersion,
		})
		fmt.Fprint(errW, agentCheckpointCaptureDisabledMessage(policy))
		return NewSilentError(errUnsupportedCheckpointPolicy)
	}
	return nil
}

func hookWritesCheckpointData(eventType agent.EventType, claudePostTodoCheckpointHook bool) bool {
	if claudePostTodoCheckpointHook {
		return true
	}
	return eventType == agent.TurnEnd || eventType == agent.SubagentEnd
}

func agentWriteHookLabel(eventType agent.EventType, claudePostTodoCheckpointHook bool) string {
	switch {
	case claudePostTodoCheckpointHook:
		return "post-todo"
	case eventType == agent.SubagentEnd:
		return "subagent-end"
	default:
		return "turn-end"
	}
}

func sessionStartPolicyWarning(policy checkpointpolicy.Policy) string {
	message := "Entire CLI is enabled, but this repository's checkpoint policy requires a newer Entire CLI. No Entire checkpoints will be created for this session until you upgrade."
	details := strings.TrimSpace(checkpointpolicy.UnsupportedPolicyMessage(policy, versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)))
	if details == "" {
		return message
	}
	return message + "\n\n" + details
}

func sessionStartPolicyReadErrorWarning(err error) string {
	return fmt.Sprintf("Entire CLI is enabled, but this repository's checkpoint policy could not be read. No Entire checkpoints will be created for this session until the policy can be read.\n\n[entire] Details:\n[entire]   %v", err)
}

func agentCheckpointCaptureDisabledMessage(policy checkpointpolicy.Policy) string {
	var b strings.Builder
	b.WriteString("[entire] Checkpoint capture is disabled for this repository.\n")
	b.WriteString("[entire] No Entire checkpoints will be created until the CLI is upgraded.\n")
	if details := strings.TrimSpace(checkpointpolicy.UnsupportedPolicyMessage(policy, versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version))); details != "" {
		b.WriteString(details)
		b.WriteByte('\n')
	}
	return b.String()
}

func agentCheckpointCaptureDisabledReadErrorMessage(err error) string {
	var b strings.Builder
	b.WriteString("[entire] Checkpoint capture is disabled for this repository.\n")
	b.WriteString("[entire] No Entire checkpoints will be created until the checkpoint policy can be read.\n")
	fmt.Fprintf(&b, "[entire] Details:\n[entire]   %v\n", err)
	return b.String()
}

func writeUnsupportedPolicySessionStartWarning(errW io.Writer, ag agent.Agent, message string) error {
	if writer, ok := agent.AsHookResponseWriter(ag); ok {
		if err := writer.WriteHookResponse(message); err != nil {
			return fmt.Errorf("failed to write hook response: %w", err)
		}
		return nil
	}
	fmt.Fprintln(errW, message)
	return nil
}

// newAgentHookVerbCmdWithLogging creates a command for a specific hook verb with structured logging.
// It uses the lifecycle dispatcher (ParseHookEvent → DispatchLifecycleEvent) as the primary path.
// PostTodo is handled directly as it's Claude-specific and not part of the lifecycle dispatcher.
func newAgentHookVerbCmdWithLogging(agentName types.AgentName, hookName string) *cobra.Command {
	return &cobra.Command{
		Use:    hookName,
		Hidden: true,
		Short:  "Called on " + hookName,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executeAgentHook(cmd, agentName, hookName, false)
		},
	}
}
