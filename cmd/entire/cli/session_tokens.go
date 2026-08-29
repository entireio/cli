package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
	"github.com/spf13/cobra"
)

// sessionTokensSourceState is the Source of a session token report whose
// totals came from the session state's running token_usage rather than the
// live transcript.
const sessionTokensSourceState = "session_state"

// sessionTokensStateFallback is the tail of every Notes line that explains
// why the totals came from session state.
const sessionTokensStateFallback = "; totals from session state" //nolint:gosec // G101: a Notes suffix, not a credential

// sessionTokensReport is the `session tokens` report: the session's identity
// and status, the shared token-report fields derived from view, and the
// context pressure the agent's hooks last reported. It is the --json
// document.
type sessionTokensReport struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	// Model is the modal model of the attributed calls, else the state's
	// model name.
	Model  string `json:"model,omitempty"`
	Status string `json:"status"`
	// Source is tokenSourceTranscript when the totals were recomputed from
	// the live transcript, else sessionTokensSourceState.
	Source          string           `json:"source"`
	DurationSeconds int              `json:"duration_seconds,omitempty"`
	Effort          *tokenEffortJSON `json:"effort,omitempty"`
	Tokens          *tokenUsageJSON  `json:"tokens,omitempty"`
	// Context is the session's current context pressure as the agent's
	// hooks last reported it.
	Context *sessionTokensContext `json:"context,omitempty"`
	Cost    *tokenCostJSON        `json:"cost,omitempty"`
	// Contributors is always present — an empty array when the session was
	// not attributed — so consumers can iterate without a nil check.
	Contributors      []tokenreport.Contributor `json:"contributors"`
	Recommendations   []tokenRecommendationJSON `json:"recommendations,omitempty"`
	AgentReportedCost float64                   `json:"agent_reported_cost,omitempty"`
	Limitations       []string                  `json:"limitations,omitempty"`

	// ended is true once the session has ended; the header then prints
	// "Duration:" without "so far".
	ended bool
	// view is what the text writers render; the JSON fields above are
	// derived from it by applyView.
	view tokenReportView
}

// applyView stores the view and derives the shared JSON fields from it.
func (r *sessionTokensReport) applyView(v tokenReportView) {
	r.view = v
	r.DurationSeconds = tokenDurationSeconds(v.Report.Duration)
	r.Effort = tokenEffortJSONFor(&v)
	r.Tokens = tokenUsageJSONFor(&v)
	r.Cost = tokenCostJSONFor(&v)
	r.Contributors = v.Report.Attributed.Contributors
	if r.Contributors == nil {
		r.Contributors = []tokenreport.Contributor{}
	}
	r.Recommendations = tokenRecommendationsJSONFor(v.Recommendations)
	r.AgentReportedCost = v.AgentReportedCost
	r.Limitations = tokenReportNotes(&v)
}

// sessionTokensContext is the `context` object of the session and
// checkpoint token reports: the agent-reported context window fill.
type sessionTokensContext struct {
	Tokens     int `json:"tokens"`
	WindowSize int `json:"window_size"`
	Percent    int `json:"percent"`
}

func newTokensCmd() *cobra.Command {
	var jsonFlag bool
	var currentFlag bool
	var agentBriefFlag bool

	cmd := &cobra.Command{
		Use:   "tokens [session-id]",
		Short: "Show token usage and optimization recommendations for a session",
		Long: `Show where a session's tokens went, with cost shares and recommendations.

When no session ID is provided, Entire reports on the most recently active
session, preferring the current worktree and falling back to the newest session
if no state matches this worktree.

The report recomputes the session's usage so far from its live transcript and
attributes it to the tools, skills and subagents that caused it; cost shares
use the provider's list-price ratios. When the transcript cannot be read the
totals fall back to the usage Entire recorded for the session.

Use --agent-brief when an agent needs compact guidance for the next step, for
example: "Use Entire token tracking to check how this session is doing and
optimize next steps."`,
		Example: "  entire session tokens\n  entire session tokens --current --agent-brief\n  entire session tokens --json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonFlag && agentBriefFlag {
				return errors.New("--json and --agent-brief are mutually exclusive")
			}
			if currentFlag && len(args) > 0 {
				return errors.New("--current and session ID argument are mutually exclusive")
			}

			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return runSessionTokens(cmd.Context(), cmd, sessionID, currentFlag, jsonFlag, agentBriefFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&currentFlag, "current", false, "Prefer the current worktree's most recent session")
	cmd.Flags().BoolVar(&agentBriefFlag, "agent-brief", false, "Output compact next-step guidance for agents")
	return cmd
}

func runSessionTokens(ctx context.Context, cmd *cobra.Command, sessionID string, current, jsonOutput, agentBrief bool) error {
	if sessionID == "" {
		if current {
			sessionID = strategy.FindMostRecentSessionInCurrentWorktree(ctx)
		} else {
			sessionID = strategy.FindMostRecentSession(ctx)
		}
		if sessionID == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "No active session found in this worktree.")
			return nil
		}
	}

	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return tokenCommandError(fmt.Errorf("failed to load session: %w", err))
	}
	if state == nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Session not found.")
		return NewSilentError(fmt.Errorf("session not found: %s", sessionID))
	}

	report := buildSessionTokensReport(ctx, state, sessionPhaseLabel(state))
	if jsonOutput {
		return printJSON(cmd.OutOrStdout(), report)
	}
	if agentBrief {
		writeTokenAgentBrief(cmd.OutOrStdout(), "Session token brief", "Session", report.SessionID, &report.view)
		return nil
	}
	writeSessionTokensText(cmd.OutOrStdout(), &report)
	return nil
}

func tokenCommandError(err error) error {
	if err == nil {
		return nil
	}
	var silent *SilentError
	if errors.As(err, &silent) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewSilentError(err)
	}
	return err
}

// buildSessionTokensReport builds the report for one session from its state.
// The totals are recomputed from the live transcript — Σ attributed calls plus
// Σ subagent transcripts — when the agent implements TokenAttributor and the
// transcript is readable (Source "transcript"); otherwise they are the
// state's running token_usage (Source "session_state") and a Notes line says
// why. The per-session analysis and the view assembly are the ones
// `checkpoint tokens` uses, fed the state in checkpoint-metadata shape
// (sessionTokensMetadata), so both commands compute every figure the same
// way. Nothing here fails the command: every problem becomes a note.
func buildSessionTokensReport(ctx context.Context, state *strategy.SessionState, status string) sessionTokensReport {
	report := sessionTokensReport{SessionID: state.SessionID, Agent: string(state.AgentType), Status: status, Source: sessionTokensSourceState, ended: state.EndedAt != nil}
	if report.Agent == "" {
		report.Agent = unknownPlaceholder
	}
	report.Context = buildSessionTokensContext(state.ContextTokens, state.ContextWindowSize)

	meta := sessionTokensMetadata(state)
	analysis := attributeSessionTokens(ctx, state, meta)
	finishSessionTokenAnalysis(&analysis)
	view := assembleTokenReportView([]sessionTokenAnalysis{analysis}, []*checkpoint.Metadata{meta})
	if view.Report.Model == "" {
		view.Report.Model = state.ModelName
	}
	view.Limitations = append(view.Limitations, analysis.notes...)
	if analysis.unmatchedSubagentRefs > 0 {
		view.Limitations = append(view.Limitations, sessionUnmatchedSubagentNote(state.AgentType, analysis.unmatchedSubagentRefs))
	}
	if view.HasUsage {
		view.Recommendations = tokenreport.Recommend(view.Report)
	}
	if analysis.recomputed {
		report.Source = tokenSourceTranscript
	}
	report.Model = view.Report.Model
	report.applyView(view)
	return report
}

// sessionTokensMetadata is the live session's state in the shape the shared
// per-session analysis reads — the fields of a checkpoint's session metadata
// the analysis uses: agent, model, running token_usage, skill events and the
// hook-reported duration. It is built for the analysis only and never
// written.
func sessionTokensMetadata(state *strategy.SessionState) *checkpoint.Metadata {
	return &checkpoint.Metadata{
		SessionID:      state.SessionID,
		Agent:          state.AgentType,
		Model:          state.ModelName,
		TokenUsage:     state.TokenUsage,
		SkillEvents:    state.SkillEvents,
		SessionMetrics: &checkpoint.SessionMetrics{DurationMs: sessionStateDuration(state).Milliseconds()},
	}
}

// sessionStateDuration is the session's duration as its state knows it: the
// agent's hook-reported duration when present, else the span from StartedAt
// to the last interaction, or to EndedAt when no interaction time was
// recorded. 0 when neither is known or the span is negative (clock skew),
// which the header prints as "not recorded".
func sessionStateDuration(state *strategy.SessionState) time.Duration {
	if state.SessionDurationMs > 0 {
		return time.Duration(state.SessionDurationMs) * time.Millisecond
	}
	end := state.LastInteractionTime
	if end == nil {
		end = state.EndedAt
	}
	if end == nil || state.StartedAt.IsZero() {
		return 0
	}
	return max(end.Sub(state.StartedAt), 0)
}

// attributeSessionTokens runs the agent's TokenAttributor over the whole
// live transcript (strategy.ResolveTranscriptPath, start line 0), reading
// subagent transcripts from the session's subagents directory, and labels
// skill loads from the state's skill events. Any reason attribution cannot
// run becomes a note; the analysis then falls back to the state's
// token_usage in finishSessionTokenAnalysis. A state that never recorded a
// transcript path is a normal state, noted without a warning; a path that
// cannot be read is warned about.
func attributeSessionTokens(ctx context.Context, state *strategy.SessionState, meta *checkpoint.Metadata) sessionTokenAnalysis {
	a := sessionTokenAnalysis{meta: meta, efforts: make(map[string]int), models: make(map[string]int)}
	attributor, reason, ok := resolveTokenAttributor(state.AgentType)
	if !ok {
		if reason != "" {
			a.notes = append(a.notes, reason+sessionTokensStateFallback)
		}
		return a
	}
	if state.TranscriptPath == "" {
		a.notes = append(a.notes, "no transcript recorded"+sessionTokensStateFallback)
		return a
	}
	data, transcriptPath, err := readSessionTranscript(state)
	if err != nil {
		// The error names the transcript path (user content); the log sinks
		// to a file, so only a classification is logged.
		a.notes = append(a.notes, "transcript unavailable"+sessionTokensStateFallback)
		logging.Warn(ctx, "session tokens: transcript unreadable",
			slog.String("session_id", state.SessionID),
			slog.String("agent", string(state.AgentType)),
			slog.String("reason", transcriptUnavailableReason(err)))
		return a
	}
	subagentsDir := paths.SubagentsDir(filepath.Dir(transcriptPath), state.SessionID)
	attribution, err := attributor.AttributeTokens(data, 0, subagentsDir)
	if err != nil {
		a.notes = append(a.notes, "transcript could not be attributed"+sessionTokensStateFallback)
		logging.Warn(ctx, "session tokens: attribution failed",
			slog.String("session_id", state.SessionID),
			slog.String("agent", string(state.AgentType)),
			slog.String("error", err.Error()))
		return a
	}
	if attribution == nil || len(attribution.Calls) == 0 {
		a.notes = append(a.notes, "no API calls in the transcript yet"+sessionTokensStateFallback)
		return a
	}
	applySkillEventAnchors(attribution, state.SkillEvents)
	a.attribution = attribution
	return a
}

// readSessionTranscript resolves the session's live transcript path (the
// agent may have relocated the file mid-session) and reads the whole file.
func readSessionTranscript(state *strategy.SessionState) ([]byte, string, error) {
	transcriptPath, err := strategy.ResolveTranscriptPath(state)
	if err != nil {
		return nil, "", fmt.Errorf("resolve transcript path: %w", err)
	}
	data, err := os.ReadFile(transcriptPath) //nolint:gosec // G304: the path is the one Entire's hooks recorded for this session.
	if err != nil {
		return nil, "", fmt.Errorf("read transcript: %w", err)
	}
	return data, transcriptPath, nil
}

// sessionUnmatchedSubagentNote words the "subagent tokens not included" note
// for a live session: Codex and OpenCode run subagents as separate sessions
// (separateSessionSubagentNote); for other agents the subagent's transcript
// is not in the subagents directory — it may not have been written yet.
func sessionUnmatchedSubagentNote(agentType types.AgentType, n int) string {
	if note := separateSessionSubagentNote(agentType); note != "" {
		return note
	}
	return fmt.Sprintf("%d subagent call%s %s no transcript in the subagents directory; that usage is not included.",
		n, tokenPluralSuffix(n), pluralHaveHas(n))
}

// buildSessionTokensContext builds the context-pressure object; nil when
// either figure is unknown.
func buildSessionTokensContext(tokens, windowSize int) *sessionTokensContext {
	if tokens <= 0 || windowSize <= 0 {
		return nil
	}
	return &sessionTokensContext{
		Tokens:     tokens,
		WindowSize: windowSize,
		Percent:    roundedPercent(tokens, windowSize),
	}
}

// roundedPercent is value/total as a whole percentage, rounded half up and
// clamped to 100, without overflowing on large counts.
func roundedPercent(value, total int) int {
	if total <= 0 {
		return 0
	}
	if value <= 0 {
		return 0
	}

	const maxPercent = 100

	hi, lo := bits.Mul64(uint64(value), maxPercent)
	lo, carry := bits.Add64(lo, uint64(total)/2, 0)
	hi += carry
	divisor := uint64(total)
	if hi >= divisor {
		return maxPercent
	}
	quotient, _ := bits.Div64(hi, lo, divisor)
	if quotient > maxPercent {
		return maxPercent
	}
	return int(quotient)
}

// writeSessionTokensText prints the breakdown-first report: the session
// header, then the shared body (Where it went, Usage, Recommendations,
// Notes). The recommendation sentences are tokenreport.Recommend's, unchanged:
// only the header's "so far" marks the session as still running.
func writeSessionTokensText(w io.Writer, report *sessionTokensReport) {
	fmt.Fprintln(w, "Session tokens")
	fmt.Fprintln(w)
	writeSessionTokensHeader(w, report)
	writeTokenReportBody(w, &report.view)
}

// writeSessionTokensHeader prints the identity lines under uniform 10-column
// labels: session, agent and model on one line; the status; the duration
// ("10s so far" while the session is live) with calls, volume and effort;
// and the context fill when the agent's hooks reported it.
func writeSessionTokensHeader(w io.Writer, report *sessionTokensReport) {
	v := &report.view
	first := []string{"Session:  " + report.SessionID, "Agent: " + report.Agent}
	if report.Model != "" {
		first = append(first, "Model: "+report.Model)
	}
	fmt.Fprintln(w, strings.Join(first, "      "))
	fmt.Fprintf(w, "Status:   %s\n", report.Status)
	suffix := " so far"
	if report.ended {
		suffix = ""
	}
	duration := "Duration: " + tokenDurationLineWith(v, suffix)
	if effort := tokenEffortHeader(v); effort != "" {
		duration += "      " + effort
	}
	fmt.Fprintln(w, duration)
	if c := report.Context; c != nil {
		fmt.Fprintf(w, "Context:  %d%% full (%s of %s)\n", c.Percent, tokenreport.FormatTokenCount(c.Tokens), tokenreport.FormatTokenCount(c.WindowSize))
	}
}
