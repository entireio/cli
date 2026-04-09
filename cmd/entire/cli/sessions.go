package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/spf13/cobra"
)

func newSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage agent sessions tracked by Entire",
		Long: `View and manage agent sessions tracked by Entire.

Commands:
  list    List all sessions across all worktrees
  info    Show detailed information for a specific session
  stop    Stop one or more active sessions

Examples:
  entire sessions list                     List all sessions
  entire sessions info <session-id>        Show session details
  entire sessions info <session-id> --json Output as JSON
  entire sessions stop                     Interactive stop`,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.WorktreeRoot(cmd.Context()); err != nil {
				return errors.New("not a git repository")
			}
			return nil
		},
	}

	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newInfoCmd())
	cmd.AddCommand(newStopCmd())

	return cmd
}

func newStopCmd() *cobra.Command {
	var allFlag bool
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "stop [session-id]",
		Short: "Stop one or more active sessions",
		Long: `Mark one or more active sessions as ended.

Fires EventSessionStop through the state machine with a no-op action handler,
so no condensation or checkpoint-writing occurs. To flush pending work, commit first.

Examples:
  entire sessions stop                     No sessions: exits. One session: confirm and stop. Multiple: show selector
  entire sessions stop <session-id>        Stop a specific session by ID
  entire sessions stop --all               Stop all active sessions
  entire sessions stop --force             Skip confirmation prompt`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			var sessionID string
			if len(args) > 0 {
				sessionID = args[0]
			}

			if allFlag && sessionID != "" {
				return errors.New("--all and session ID argument are mutually exclusive")
			}

			return runStop(ctx, cmd, sessionID, allFlag, forceFlag)
		},
	}

	cmd.Flags().BoolVar(&allFlag, "all", false, "Stop all active sessions")
	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt")

	return cmd
}

// runStop is the main logic for the stop command.
func runStop(ctx context.Context, cmd *cobra.Command, sessionID string, all, force bool) error {
	// --session path: stop a specific session by explicit ID (no worktree scoping).
	if sessionID != "" {
		return runStopSession(ctx, cmd, sessionID, force)
	}

	// List all session states
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	activeSessions := filterActiveSessions(states)

	// --all path: stop all active sessions across all worktrees.
	if all {
		return runStopAll(ctx, cmd, activeSessions, force)
	}

	// No-flags path: show all active sessions across all worktrees.
	// This aligns with `entire status` which displays sessions globally.
	// Users see worktree labels in the multi-select to make informed choices.
	if len(activeSessions) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No active sessions.")
		return nil
	}

	// One active session: confirm + stop.
	if len(activeSessions) == 1 {
		return runStopSession(ctx, cmd, activeSessions[0].SessionID, force)
	}

	// Multiple active sessions: show TUI multi-select.
	return runStopMultiSelect(ctx, cmd, activeSessions, force)
}

// filterActiveSessions returns sessions that have not been explicitly ended.
// A session is considered ended if Phase == PhaseEnded OR EndedAt is set.
// This matches the logic in status.go's writeActiveSessions for consistency:
// any session visible in `entire status` should also be visible in `sessions stop`.
func filterActiveSessions(states []*strategy.SessionState) []*strategy.SessionState {
	var active []*strategy.SessionState
	for _, s := range states {
		if s == nil {
			continue
		}
		if s.Phase != session.PhaseEnded && s.EndedAt == nil {
			active = append(active, s)
		}
	}
	return active
}

// sessionWorktreeLabel returns the worktree display label for a session.
// Uses WorktreeID if available, falls back to the last path component of
// WorktreePath, or "(unknown)" for empty values (legacy sessions without
// worktree tracking). Matches status.go's unknownPlaceholder convention.
func sessionWorktreeLabel(s *strategy.SessionState) string {
	if s.WorktreeID != "" {
		return s.WorktreeID
	}
	if s.WorktreePath != "" {
		return filepath.Base(s.WorktreePath)
	}
	return unknownPlaceholder
}

// sessionPhaseLabel returns the display status for a session.
func sessionPhaseLabel(s *strategy.SessionState) string {
	if s.EndedAt != nil {
		return "ended"
	}
	status := string(s.Phase)
	if status == "" {
		return "idle"
	}
	return status
}

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		Long: `List all sessions tracked by Entire, including ended sessions.

For active sessions only, use 'entire status'.

Examples:
  entire sessions list    List all sessions across all worktrees`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionList(cmd.Context(), cmd)
		},
	}

	return cmd
}

func runSessionList(ctx context.Context, cmd *cobra.Command) error {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	var filtered []*strategy.SessionState
	for _, s := range states {
		if s != nil {
			filtered = append(filtered, s)
		}
	}

	w := cmd.OutOrStdout()

	if len(filtered) == 0 {
		fmt.Fprintln(w, "No sessions.")
		return nil
	}

	// Sort by StartedAt descending (newest first)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})

	sty := newStatusStyles(w)

	fmt.Fprintln(w, sty.sectionRule("Sessions", sty.width))
	fmt.Fprintln(w)

	for _, s := range filtered {
		writeSessionCard(w, s, sty)
	}

	// Footer
	fmt.Fprintln(w, sty.horizontalRule(sty.width))
	if len(filtered) == 1 {
		fmt.Fprintln(w, sty.render(sty.dim, "1 session"))
	} else {
		fmt.Fprintln(w, sty.render(sty.dim, fmt.Sprintf("%d sessions", len(filtered))))
	}
	fmt.Fprintln(w)

	return nil
}

// writeSessionCard renders a single session in status-style card format.
func writeSessionCard(w io.Writer, s *strategy.SessionState, sty statusStyles) {
	agentLabel := string(s.AgentType)
	if agentLabel == "" {
		agentLabel = "(unknown)"
	}
	wt := sessionWorktreeLabel(s)

	// Line 1: Agent · Model · worktree · session <id> [· checkpoint <id>]
	fmt.Fprint(w, sty.render(sty.agent, agentLabel))
	if s.ModelName != "" {
		fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), sty.render(sty.dim, s.ModelName))
	}
	fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), wt)
	fmt.Fprintf(w, " %s session %s", sty.render(sty.dim, "·"), s.SessionID)
	if s.LastCheckpointID != "" {
		fmt.Fprintf(w, " %s checkpoint %s", sty.render(sty.dim, "·"), string(s.LastCheckpointID))
	}
	fmt.Fprintln(w)

	// Line 2: > "prompt" (truncated)
	if s.LastPrompt != "" {
		prompt := stringutil.TruncateRunes(s.LastPrompt, 60, "...")
		fmt.Fprintf(w, "%s \"%s\"\n", sty.render(sty.dim, ">"), prompt)
	}

	// Line 3: status · started X ago · active X ago · tokens X.Xk
	var stats []string
	stats = append(stats, sessionPhaseLabel(s))
	stats = append(stats, "started "+timeAgo(s.StartedAt))
	if s.LastInteractionTime != nil && s.LastInteractionTime.Sub(s.StartedAt) > time.Minute {
		stats = append(stats, activeTimeDisplay(s.LastInteractionTime))
	}
	if t := totalTokens(s.TokenUsage); t > 0 {
		stats = append(stats, "tokens "+formatTokenCount(t))
	}
	statsLine := strings.Join(stats, sty.render(sty.dim, " · "))
	fmt.Fprintln(w, sty.render(sty.dim, statsLine))
	fmt.Fprintln(w)
}

func newInfoCmd() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "info <session-id>",
		Short: "Show detailed session information",
		Long: `Display detailed state for a session.

Shows agent, model, status, worktree, timing, token usage, checkpoint linkage,
and files touched. Works for both active and ended sessions.

Examples:
  entire sessions info <session-id>
  entire sessions info <session-id> --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionInfo(cmd.Context(), cmd, args[0], jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")

	return cmd
}

func runSessionInfo(ctx context.Context, cmd *cobra.Command, sessionID string, jsonOutput bool) error {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session: %w", err)
	}
	if state == nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Session not found.")
		return NewSilentError(fmt.Errorf("session not found: %s", sessionID))
	}

	status := sessionPhaseLabel(state)

	if jsonOutput {
		return writeSessionInfoJSON(cmd.OutOrStdout(), state, status)
	}
	return writeSessionInfoText(cmd.OutOrStdout(), state, status)
}

// sessionInfoJSON is the JSON output structure for sessions info --json.
type sessionInfoJSON struct {
	SessionID      string         `json:"session_id"`
	Agent          string         `json:"agent"`
	Model          string         `json:"model,omitempty"`
	Status         string         `json:"status"`
	WorktreeID     string         `json:"worktree_id,omitempty"`
	WorktreePath   string         `json:"worktree_path,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	EndedAt        *time.Time     `json:"ended_at,omitempty"`
	LastActive     *time.Time     `json:"last_active,omitempty"`
	Turns          int            `json:"turns"`
	Checkpoints    int            `json:"checkpoints"`
	LastCheckpoint string         `json:"last_checkpoint_id,omitempty"`
	Tokens         *tokenInfoJSON `json:"tokens,omitempty"`
	LastPrompt     string         `json:"last_prompt,omitempty"`
	FilesTouched   []string       `json:"files_touched,omitempty"`
}

type tokenInfoJSON struct {
	Total      int `json:"total"`
	Input      int `json:"input"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Output     int `json:"output"`
}

func writeSessionInfoJSON(w io.Writer, state *strategy.SessionState, status string) error {
	agentLabel := string(state.AgentType)
	if agentLabel == "" {
		agentLabel = unknownPlaceholder
	}
	info := sessionInfoJSON{
		SessionID:      state.SessionID,
		Agent:          agentLabel,
		Model:          state.ModelName,
		Status:         status,
		WorktreeID:     state.WorktreeID,
		WorktreePath:   state.WorktreePath,
		StartedAt:      state.StartedAt,
		EndedAt:        state.EndedAt,
		LastActive:     state.LastInteractionTime,
		Turns:          state.SessionTurnCount,
		Checkpoints:    state.StepCount,
		LastCheckpoint: string(state.LastCheckpointID),
		LastPrompt:     state.LastPrompt,
		FilesTouched:   state.FilesTouched,
	}
	if state.TokenUsage != nil {
		info.Tokens = &tokenInfoJSON{
			Total:      totalTokens(state.TokenUsage),
			Input:      state.TokenUsage.InputTokens,
			CacheRead:  state.TokenUsage.CacheReadTokens,
			CacheWrite: state.TokenUsage.CacheCreationTokens,
			Output:     state.TokenUsage.OutputTokens,
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(info); err != nil {
		return fmt.Errorf("failed to encode session info: %w", err)
	}
	return nil
}

func writeSessionInfoText(w io.Writer, state *strategy.SessionState, status string) error {
	fmt.Fprintf(w, "Session %s\n\n", state.SessionID)

	agentLabel := string(state.AgentType)
	if agentLabel == "" {
		agentLabel = "(unknown)"
	}
	fmt.Fprintf(w, "Agent:       %s\n", agentLabel)
	if state.ModelName != "" {
		fmt.Fprintf(w, "Model:       %s\n", state.ModelName)
	}

	fmt.Fprintf(w, "Status:      %s\n", status)

	wt := sessionWorktreeLabel(state)
	fmt.Fprintf(w, "Worktree:    %s\n", wt)

	fmt.Fprintf(w, "Started:     %s (%s)\n",
		state.StartedAt.Local().Format("2006-01-02 15:04"), timeAgo(state.StartedAt))

	if state.EndedAt != nil {
		fmt.Fprintf(w, "Ended:       %s (%s)\n",
			state.EndedAt.Local().Format("2006-01-02 15:04"), timeAgo(*state.EndedAt))
	}

	if state.LastInteractionTime != nil {
		fmt.Fprintf(w, "Last active: %s (%s)\n",
			state.LastInteractionTime.Local().Format("2006-01-02 15:04"),
			timeAgo(*state.LastInteractionTime))
	}

	if state.SessionTurnCount > 0 {
		fmt.Fprintf(w, "Turns:       %d\n", state.SessionTurnCount)
	}

	fmt.Fprintf(w, "Checkpoints: %d\n", state.StepCount)

	if state.LastCheckpointID != "" {
		fmt.Fprintf(w, "Checkpoint:  %s\n", state.LastCheckpointID)
	}

	if t := totalTokens(state.TokenUsage); t > 0 {
		fmt.Fprintf(w, "\nTokens:      %s\n", formatTokenCount(t))

		var parts []string
		if state.TokenUsage.InputTokens > 0 {
			parts = append(parts, "Input: "+formatTokenCount(state.TokenUsage.InputTokens))
		}
		if state.TokenUsage.CacheReadTokens > 0 {
			parts = append(parts, "Cache read: "+formatTokenCount(state.TokenUsage.CacheReadTokens))
		}
		if state.TokenUsage.CacheCreationTokens > 0 {
			parts = append(parts, "Cache write: "+formatTokenCount(state.TokenUsage.CacheCreationTokens))
		}
		if state.TokenUsage.OutputTokens > 0 {
			parts = append(parts, "Output: "+formatTokenCount(state.TokenUsage.OutputTokens))
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s\n", strings.Join(parts, " · "))
		}
	}

	if state.LastPrompt != "" {
		fmt.Fprintf(w, "\nLast prompt: %q\n", state.LastPrompt)
	}

	if len(state.FilesTouched) > 0 {
		fmt.Fprintln(w, "\nFiles touched:")
		for _, f := range state.FilesTouched {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}

	return nil
}

// runStopSession stops a single session by ID, with optional confirmation.
func runStopSession(ctx context.Context, cmd *cobra.Command, sessionID string, force bool) error {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session: %w", err)
	}
	if state == nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Session not found.")
		return NewSilentError(fmt.Errorf("session not found: %s", sessionID))
	}

	if state.Phase == session.PhaseEnded || state.EndedAt != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Session %s is already stopped.\n", sessionID)
		return nil
	}

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop session %s?", sessionID)).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	return stopSessionAndPrint(ctx, cmd, state)
}

// runStopAll stops all active sessions across all worktrees.
func runStopAll(ctx context.Context, cmd *cobra.Command, activeSessions []*strategy.SessionState, force bool) error {
	if len(activeSessions) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No active sessions.")
		return nil
	}

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop %d session(s)?", len(activeSessions))).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	return stopSelectedSessions(ctx, cmd, activeSessions)
}

// runStopMultiSelect shows a TUI multi-select for multiple active sessions.
func runStopMultiSelect(ctx context.Context, cmd *cobra.Command, activeSessions []*strategy.SessionState, force bool) error {
	options := make([]huh.Option[string], len(activeSessions))
	for i, s := range activeSessions {
		wt := sessionWorktreeLabel(s)
		label := fmt.Sprintf("%s · %s · %s", s.AgentType, wt, s.SessionID)
		if s.LastPrompt != "" {
			prompt := stringutil.TruncateRunes(s.LastPrompt, 40, "...")
			label = fmt.Sprintf("%s · %q", label, prompt)
		}
		options[i] = huh.NewOption(label, s.SessionID)
	}

	var selectedIDs []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select sessions to stop").
				Description("Use space to select, enter to confirm.").
				Options(options...).
				Value(&selectedIDs),
		),
	)
	if err := form.Run(); err != nil {
		return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
	}

	if len(selectedIDs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
		return nil
	}

	// Build a map for quick lookup
	stateByID := make(map[string]*strategy.SessionState, len(activeSessions))
	for _, s := range activeSessions {
		stateByID[s.SessionID] = s
	}

	// Confirm only if not forcing
	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop %d session(s)?", len(selectedIDs))).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	var toStop []*strategy.SessionState
	for _, id := range selectedIDs {
		if s, ok := stateByID[id]; ok {
			toStop = append(toStop, s)
		} else {
			// Session was concurrently stopped between form render and confirmation.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: session %s no longer found, skipping.\n", id)
		}
	}
	if len(toStop) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions to stop.")
		return nil
	}
	return stopSelectedSessions(ctx, cmd, toStop)
}

// stopSelectedSessions stops each session in the list and prints a result line.
// Errors from individual sessions are accumulated so a single failure does not
// prevent remaining sessions from being stopped. Each failure is printed to stderr
// immediately so the user knows which sessions could not be stopped.
func stopSelectedSessions(ctx context.Context, cmd *cobra.Command, sessions []*strategy.SessionState) error {
	var errs []error
	for _, s := range sessions {
		if err := stopSessionAndPrint(ctx, cmd, s); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "✗ %v\n", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopSessionAndPrint stops a session and prints a summary line.
// Fields needed for output are read before calling markSessionEnded because
// markSessionEnded loads and operates on its own copy of the session state by ID —
// it does not update the caller's state pointer.
func stopSessionAndPrint(ctx context.Context, cmd *cobra.Command, state *strategy.SessionState) error {
	sessionID := state.SessionID
	lastCheckpointID := state.LastCheckpointID
	stepCount := state.StepCount

	if err := markSessionEnded(ctx, nil, sessionID); err != nil {
		return fmt.Errorf("failed to stop session %s: %w", sessionID, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Session %s stopped.\n", sessionID)
	switch {
	case lastCheckpointID != "":
		fmt.Fprintf(cmd.OutOrStdout(), "  Checkpoint: %s\n", lastCheckpointID)
	case stepCount > 0:
		fmt.Fprintln(cmd.OutOrStdout(), "  Work will be captured in your next checkpoint.")
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "  No work recorded.")
	}
	return nil
}
