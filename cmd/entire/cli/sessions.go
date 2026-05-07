package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/spf13/cobra"
)

// streamTranscriptToStdout copies the contents of the file at path to w.
// Cancellation is wired through a goroutine that closes the file on
// <-ctx.Done(), so reads return promptly when the user hits Ctrl-C on a
// multi-MB transcript instead of blocking until EOF.
//
// Snapshot semantics: the read is bounded to the file size observed at open
// (via io.LimitReader) so writes the agent appends after command start are
// excluded.
//
// Output shaping is agent-aware so the streaming path stays bounded-memory
// for the unbounded case (JSONL transcripts grow with conversation length):
//
//   - JSONL agents (Claude Code, Cursor, Codex, etc.) — line-buffered copy.
//     Only one line is held in memory at a time. A trailing partial line
//     (agent mid-write) is silently dropped so consumers never see a
//     truncated record.
//   - Whole-document JSON agents (Gemini) — read snapshot into memory and
//     validate with json.Valid before emitting. These transcripts are
//     bounded by conversation size and rarely exceed a few MB even for
//     long sessions, so buffering is acceptable here.
//
// path comes from a session-state file that Entire writes exclusively
// under the user's own .git/. The path is therefore as trusted as any
// other entry the local user has on disk; we do not validate it against a
// confinement root.
func streamTranscriptToStdout(ctx context.Context, w io.Writer, path string, agentType types.AgentType) error {
	f, err := os.Open(path) //nolint:gosec // see comment above on trust model
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}

	// Bound the snapshot to the file size at open. Without this, the read
	// would also include bytes the agent appends while in flight, silently
	// extending the "snapshot" past command-start. Stat() failure here means
	// we can't honor the snapshot guarantee — fail loudly rather than
	// emitting an unbounded read with a misleading "snapshot" promise.
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return fmt.Errorf("stat transcript (snapshot bound unavailable): %w", statErr)
	}
	snapshotSize := info.Size()

	// Single owner of Close. Either the cancel goroutine fires it (to
	// unblock the read) or the defer fires it (normal path); sync.Once
	// prevents the double close that would otherwise trip the race
	// detector under heavy fd reuse.
	var closeOnce sync.Once
	closeFn := func() { closeOnce.Do(func() { _ = f.Close() }) }

	closeOnDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeFn()
		case <-closeOnDone:
		}
	}()
	defer func() {
		close(closeOnDone)
		closeFn()
	}()

	reader := io.LimitReader(f, snapshotSize)

	if isWholeDocumentJSONAgent(agentType) {
		return writeWholeDocumentJSONTranscript(ctx, w, reader)
	}
	return writeJSONLTranscript(ctx, w, reader)
}

// isWholeDocumentJSONAgent reports whether an agent's on-disk transcript is
// a single JSON document (e.g. Gemini's session-*.json) versus JSONL.
func isWholeDocumentJSONAgent(agentType types.AgentType) bool {
	return agentType == agent.AgentTypeGemini
}

// writeJSONLTranscript copies a JSONL transcript line-by-line. Each completed
// line (including its newline) is written to w. A trailing partial line at
// EOF is dropped so consumers never see a truncated record.
func writeJSONLTranscript(ctx context.Context, w io.Writer, r io.Reader) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if _, werr := w.Write(line); werr != nil {
				return fmt.Errorf("write transcript: %w", werr)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr //nolint:wrapcheck // propagating context cancellation
			}
			return fmt.Errorf("read transcript: %w", err)
		}
	}
}

// writeWholeDocumentJSONTranscript reads the snapshot, validates it parses as
// JSON, and emits it intact. Trim-to-last-newline would cut the closing
// brace and produce malformed output for these agents. An invalid snapshot
// (agent mid-write or genuinely corrupt) is reported as an error rather
// than emitting empty output, so machine consumers can distinguish "no
// data" from "data unavailable, retry" — exit-code 0 + empty stdout would
// otherwise look identical to a successfully-empty transcript.
func writeWholeDocumentJSONTranscript(ctx context.Context, w io.Writer, r io.Reader) error {
	buf, err := io.ReadAll(r)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr //nolint:wrapcheck // propagating context cancellation
		}
		return fmt.Errorf("read transcript: %w", err)
	}
	// Empty file is a valid snapshot for an agent that hasn't yet written
	// anything; emit nothing and succeed. Non-empty but invalid is an error.
	if len(buf) == 0 {
		return nil
	}
	if !json.Valid(buf) {
		return errors.New("transcript snapshot is not valid JSON (agent may be mid-write); retry the command")
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	return nil
}

func newSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"},
		Short:   "Manage agent sessions tracked by Entire",
		Long: `View and manage agent sessions tracked by Entire.

Commands:
  list     List all sessions across all worktrees
  info     Show detailed information for a specific session
  stop     Stop one or more active sessions
  current  Show the active session for the current worktree
  attach   Attach an existing agent session
  resume   Switch to a branch and resume its session

Examples:
  entire session list                      List all sessions
  entire session info <session-id>         Show session details
  entire session info <session-id> --json  Output as JSON
  entire session stop                      Interactive stop
  entire session current                   Active session for cwd
  entire session attach <session-id>       Attach an external session
  entire session resume <branch>           Resume from a branch`,
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
	cmd.AddCommand(newSessionCurrentCmd())
	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newResumeCmd())

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
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		Long: `List all sessions tracked by Entire, including ended sessions.

For active sessions only, use 'entire status'.

Examples:
  entire sessions list           List all sessions across all worktrees
  entire sessions list --json    Same list as a metadata-only JSON array`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionList(cmd.Context(), cmd, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	return cmd
}

func runSessionList(ctx context.Context, cmd *cobra.Command, jsonOutput bool) error {
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

	// Sort by StartedAt descending (newest first); same order as the prose view.
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})

	w := cmd.OutOrStdout()

	if jsonOutput {
		return writeSessionListJSON(w, filtered)
	}

	if len(filtered) == 0 {
		fmt.Fprintln(w, "No sessions.")
		return nil
	}

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

// writeSessionListJSON emits the list as a JSON array of the same per-session
// envelope returned by `entire session info --json`. Always emits a valid
// array (`[]` for the empty case) so consumers can pipe through `jq` without
// special-casing "no sessions".
func writeSessionListJSON(w io.Writer, states []*strategy.SessionState) error {
	out := make([]sessionInfoJSON, 0, len(states))
	for _, state := range states {
		out = append(out, buildSessionInfoJSON(state, sessionPhaseLabel(state)))
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("failed to encode session list: %w", err)
	}
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

// sessionOutputMode describes how `entire session info` / `session current`
// should render the resolved session. Cobra enforces mutual exclusion at the
// flag layer; the enum makes the trichotomy total in code so we can't
// accidentally combine modes by passing two booleans.
type sessionOutputMode int

const (
	sessionOutputText sessionOutputMode = iota
	sessionOutputJSON
	sessionOutputTranscript
)

func newInfoCmd() *cobra.Command {
	var jsonFlag bool
	var transcriptFlag bool

	cmd := &cobra.Command{
		Use:   "info <session-id>",
		Short: "Show detailed session information",
		Long: `Display detailed state for a session.

Shows agent, model, status, worktree, timing, token usage, checkpoint linkage,
and files touched. Works for both active and ended sessions.

Output modes:
  Default       Human-readable summary.
  --json        Metadata-only JSON envelope (no transcript bytes).
  --transcript  Stream the live raw agent transcript bytes to stdout in
                the agent's native format (JSONL for Claude/Cursor/Codex,
                JSON for Gemini). Snapshot is bounded to the file size
                observed at open. JSONL streams have a trailing partial
                line trimmed; JSON documents are emitted intact.

Examples:
  entire sessions info <session-id>
  entire sessions info <session-id> --json
  entire sessions info <session-id> --transcript > session.jsonl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionInfo(cmd.Context(), cmd, args[0], sessionOutputModeFromFlags(jsonFlag, transcriptFlag))
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&transcriptFlag, "transcript", false, "Stream raw agent transcript bytes to stdout")
	cmd.MarkFlagsMutuallyExclusive("json", "transcript")

	return cmd
}

func sessionOutputModeFromFlags(jsonFlag, transcriptFlag bool) sessionOutputMode {
	switch {
	case transcriptFlag:
		return sessionOutputTranscript
	case jsonFlag:
		return sessionOutputJSON
	default:
		return sessionOutputText
	}
}

func runSessionInfo(ctx context.Context, cmd *cobra.Command, sessionID string, mode sessionOutputMode) error {
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

	switch mode {
	case sessionOutputTranscript:
		return writeSessionTranscript(ctx, cmd, state)
	case sessionOutputJSON:
		return writeSessionInfoJSON(cmd.OutOrStdout(), state, status)
	case sessionOutputText:
		return writeSessionInfoText(cmd.OutOrStdout(), state, status)
	default:
		return fmt.Errorf("unknown session output mode: %d", mode)
	}
}

// writeSessionTranscript streams the live raw agent transcript for a session
// to stdout. The transcript bytes are exactly what the agent has written to
// disk in its native per-agent format (JSONL for Claude Code/Cursor, JSON for
// Gemini, etc.) — Entire performs no normalization here.
func writeSessionTranscript(ctx context.Context, cmd *cobra.Command, state *strategy.SessionState) error {
	if state.TranscriptPath == "" {
		cmd.SilenceUsage = true
		msg := fmt.Sprintf("session %s has no transcript path recorded", state.SessionID)
		fmt.Fprintln(cmd.ErrOrStderr(), msg)
		return NewSilentError(errors.New(msg))
	}

	path, err := strategy.ResolveTranscriptPath(state)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "transcript unavailable: %v\n", err)
		return NewSilentError(fmt.Errorf("transcript unavailable: %w", err))
	}

	// Errors from streaming are runtime issues (Stat failure, mid-write JSON,
	// etc.), not flag-usage problems — don't print cobra's usage block on top
	// of any partial stdout output.
	cmd.SilenceUsage = true
	return streamTranscriptToStdout(ctx, cmd.OutOrStdout(), path, state.AgentType)
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

// buildSessionInfoJSON converts a SessionState into the JSON envelope shared
// by `session info --json`, `session current --json`, and `session list --json`.
func buildSessionInfoJSON(state *strategy.SessionState, status string) sessionInfoJSON {
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
	return info
}

func writeSessionInfoJSON(w io.Writer, state *strategy.SessionState, status string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildSessionInfoJSON(state, status)); err != nil {
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
