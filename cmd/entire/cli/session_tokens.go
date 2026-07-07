package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

type sessionTokensReport struct {
	SessionID       string                        `json:"session_id"`
	Agent           string                        `json:"agent"`
	Model           string                        `json:"model,omitempty"`
	Status          string                        `json:"status"`
	Source          string                        `json:"source"`
	Tokens          *sessionTokensUsage           `json:"tokens,omitempty"`
	Context         *sessionTokensContext         `json:"context,omitempty"`
	Contributors    []sessionTokensContributor    `json:"contributors,omitempty"`
	Recommendations []sessionTokensRecommendation `json:"recommendations,omitempty"`
	Limitations     []string                      `json:"limitations,omitempty"`
}

type sessionTokensUsage struct {
	Total         int `json:"total"`
	Input         int `json:"input"`
	CacheRead     int `json:"cache_read"`
	CacheWrite    int `json:"cache_write"`
	Output        int `json:"output"`
	APICalls      int `json:"api_calls"`
	SubagentTotal int `json:"subagent_total,omitempty"`
}

type sessionTokensContext struct {
	Tokens     int `json:"tokens"`
	WindowSize int `json:"window_size"`
	Percent    int `json:"percent"`
}

type sessionTokensContributor struct {
	Kind       string   `json:"kind"`
	Label      string   `json:"label"`
	Tokens     int      `json:"tokens,omitempty"`
	Percent    int      `json:"percent,omitempty"`
	Confidence string   `json:"confidence"`
	Signals    []string `json:"signals,omitempty"`
}

type sessionTokensRecommendation struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Signals  []string `json:"signals,omitempty"`
}

type tokenRecommendationSignals struct {
	Tokens          *sessionTokensUsage
	Context         *sessionTokensContext
	TurnCount       int
	CheckpointCount int
}

// Recommendation thresholds are coarse diagnostics for clear token hotspots, not a cost model or quality verdict.
const (
	recommendationHighCacheReadPercent     = 80
	recommendationHighAPICalls             = 20
	recommendationSubagentShareDenominator = 10
	recommendationHighContextPercent       = 80
	recommendationLongSessionTurns         = 10
	recommendationLongSessionCheckpoints   = 5
)

const agentBriefCostProxyBatchAction = "Use at most 3 batched reads before answering. Continue only if a named file or test can change the verdict; otherwise answer now. Avoid broad grep, broad diffs, broad tests, and repeated token diagnostics; keep the answer tight."

func newTokensCmd() *cobra.Command {
	var jsonFlag bool
	var currentFlag bool
	var agentBriefFlag bool

	cmd := &cobra.Command{
		Use:   "tokens [session-id]",
		Short: "Show token usage and optimization recommendations for a session",
		Long: `Show token usage and optimization recommendations for a session.

When no session ID is provided, Entire reports on the most recently active
session, preferring the current worktree and falling back to the newest session
if no state matches this worktree. The report uses token and context data Entire
already captured for the session.

Use --agent-brief when an agent needs compact guidance for the next step, for
example: "Use Entire token tracking to check how this session is doing and
optimize next steps."`,
		Args: cobra.MaximumNArgs(1),
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

	report := buildSessionTokensReport(state, sessionPhaseLabel(state))
	if jsonOutput {
		return printJSON(cmd.OutOrStdout(), report)
	}
	if agentBrief {
		writeSessionTokensAgentBrief(cmd.OutOrStdout(), report)
		return nil
	}
	writeSessionTokensText(cmd.OutOrStdout(), report)
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

func buildSessionTokensReport(state *strategy.SessionState, status string) sessionTokensReport {
	agentLabel := string(state.AgentType)
	if agentLabel == "" {
		agentLabel = unknownPlaceholder
	}

	report := sessionTokensReport{
		SessionID: state.SessionID,
		Agent:     agentLabel,
		Model:     state.ModelName,
		Status:    status,
		Source:    "session_state",
	}

	if tokens := buildSessionTokensUsage(state.TokenUsage); tokens != nil {
		report.Tokens = tokens
		if tokens.SubagentTotal > 0 {
			report.Contributors = append(report.Contributors, sessionTokensContributor{
				Kind:       "subagents",
				Label:      "Subagents",
				Tokens:     tokens.SubagentTotal,
				Confidence: "reported",
				Signals:    []string{"subagent_tokens"},
			})
		}
	} else {
		report.Limitations = append(report.Limitations, "No token usage recorded for this session.")
		report.Recommendations = append(report.Recommendations, sessionTokensRecommendation{
			ID:       "no-token-data",
			Severity: "low",
			Message:  "Token usage is unavailable for this session; the agent may not expose token data yet, or no checkpoint has captured it.",
			Signals:  []string{"missing_token_usage"},
		})
	}

	if contextInfo := buildSessionTokensContext(state.ContextTokens, state.ContextWindowSize); contextInfo != nil {
		report.Context = contextInfo
		report.Contributors = append(report.Contributors, sessionTokensContributor{
			Kind:       "context_pressure",
			Label:      "Context pressure",
			Percent:    contextInfo.Percent,
			Confidence: "reported",
			Signals:    []string{"context_tokens"},
		})
	}

	if labels := skillEventLabels(state.SkillEvents); len(labels) > 0 {
		report.Contributors = append(report.Contributors, sessionTokensContributor{
			Kind:       "skills",
			Label:      "Skills/slash commands: " + strings.Join(labels, ", "),
			Confidence: "reported",
			Signals:    []string{"skill_events"},
		})
	}

	report.Recommendations = append(report.Recommendations, recommendationRules(tokenRecommendationSignals{
		Tokens:          report.Tokens,
		Context:         report.Context,
		TurnCount:       state.SessionTurnCount,
		CheckpointCount: state.StepCount,
	})...)
	return report
}

func buildSessionTokensUsage(usage *agent.TokenUsage) *sessionTokensUsage {
	if usage == nil {
		return nil
	}
	total := totalTokens(usage)
	if total == 0 && usage.APICallCount == 0 {
		return nil
	}
	return &sessionTokensUsage{
		Total:         total,
		Input:         usage.InputTokens,
		CacheRead:     usage.CacheReadTokens,
		CacheWrite:    usage.CacheCreationTokens,
		Output:        usage.OutputTokens,
		APICalls:      usage.APICallCount,
		SubagentTotal: totalTokens(usage.SubagentTokens),
	}
}

func topLevelSessionTokenTotal(tokens *sessionTokensUsage) int {
	if tokens == nil {
		return 0
	}
	total := saturatingIntAdd(tokens.Input, tokens.CacheWrite)
	total = saturatingIntAdd(total, tokens.CacheRead)
	return saturatingIntAdd(total, tokens.Output)
}

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

func recommendationRules(signals tokenRecommendationSignals) []sessionTokensRecommendation {
	var recs []sessionTokensRecommendation

	cacheReadHotspot := false
	if signals.Tokens != nil && signals.Tokens.CacheRead > 0 {
		cacheReadPercent := tokenPercent(signals.Tokens.CacheRead, topLevelSessionTokenTotal(signals.Tokens))
		if cacheReadPercent >= recommendationHighCacheReadPercent {
			cacheReadHotspot = true
			recs = append(recs, sessionTokensRecommendation{
				ID:       "context-replay-hotspot",
				Severity: "high",
				Message: fmt.Sprintf(
					"Cache/context replay is %s of token volume; reduce unnecessary follow-up calls in this large-context session.",
					formatPercent(cacheReadPercent),
				),
				Signals: []string{"cache_read_tokens"},
			})
		}
	}
	if signals.Tokens != nil && signals.Tokens.APICalls >= recommendationHighAPICalls {
		message := fmt.Sprintf("API call count is high for one session: %d calls. Batch the next diagnosis and reduce iterative calls.", signals.Tokens.APICalls)
		if cacheReadHotspot {
			message = fmt.Sprintf("Large context was replayed across %d API calls; batch the next diagnosis and reduce iterative tool calls.", signals.Tokens.APICalls)
		}
		recs = append(recs, sessionTokensRecommendation{
			ID:       "api-call-amplification",
			Severity: "medium",
			Message:  message,
			Signals:  []string{"api_call_count"},
		})
	}
	if signals.Tokens != nil && tokenShareAtLeastOneTenth(signals.Tokens.SubagentTotal, signals.Tokens.Total) {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "subagent-heavy",
			Severity: "medium",
			Message:  "Scope subagent tasks tightly; give each subagent a narrow objective and expected output.",
			Signals:  []string{"subagent_tokens"},
		})
	}
	if signals.Tokens != nil && signals.Tokens.Total > 0 &&
		tokenClassPressure(signals.Tokens.CacheWrite, signals.Tokens.Total, 5000, 10, 50_000) {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "cache-write-pressure",
			Severity: "medium",
			Message:  "Cache write is elevated; avoid broad new context and narrow the next read before continuing.",
			Signals:  []string{"cache_write_tokens"},
		})
	}
	if signals.Tokens != nil && signals.Tokens.Total > 0 &&
		tokenClassPressure(signals.Tokens.Output, signals.Tokens.Total, 3000, 2, 10_000) {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "output-pressure",
			Severity: "medium",
			Message:  "Output tokens are elevated; keep the next answer tight and avoid restating evidence.",
			Signals:  []string{"output_tokens"},
		})
	}
	if signals.Context != nil && signals.Context.Percent >= recommendationHighContextPercent {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "high-context-pressure",
			Severity: "medium",
			Message:  fmt.Sprintf("Context pressure is %d%% of the window; preserve only relevant context before continuing.", signals.Context.Percent),
			Signals:  []string{"context_tokens"},
		})
	}
	if cacheReadHotspot && signals.Tokens != nil && signals.Tokens.APICalls >= recommendationHighAPICalls {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "summarize-before-boundary",
			Severity: "low",
			Message:  "Compact or restart after summarizing this investigation; do not discard useful findings just because cache read is high.",
			Signals:  []string{"cache_read_tokens", "api_call_count"},
		})
	}
	if signals.TurnCount >= recommendationLongSessionTurns || signals.CheckpointCount >= recommendationLongSessionCheckpoints {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "long-session",
			Severity: "low",
			Message:  "Compact or restart after summarizing the useful findings if older context is no longer needed.",
			Signals:  []string{"turn_count", "checkpoint_count"},
		})
	}

	return recs
}

func tokenShareAtLeastOneTenth(part, total int) bool {
	if part <= 0 || total <= 0 {
		return false
	}
	return part >= (total-1)/recommendationSubagentShareDenominator+1
}

func tokenClassPressure(value, total, minTokens int, minPercent float64, highTokens int) bool {
	if value <= 0 || total <= 0 {
		return false
	}
	if value >= highTokens {
		return true
	}
	return value >= minTokens && tokenPercent(value, total) >= minPercent
}

func tokenPercent(value, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(value) * 100 / float64(total)
}

func formatPercent(percent float64) string {
	formatted := fmt.Sprintf("%.1f", percent)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + "%"
}

func skillEventLabels(events []agent.SkillEvent) []string {
	seen := make(map[string]struct{}, len(events))
	labels := make([]string, 0, len(events))
	for _, event := range events {
		label := event.Collapse.Label
		if label == "" && event.Native != nil {
			label = event.Native["command"]
		}
		if label == "" {
			label = event.Skill.Name
		}
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func writeSessionTokensText(w io.Writer, report sessionTokensReport) {
	fmt.Fprintln(w, "Session tokens")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Session: %s\n", report.SessionID)
	fmt.Fprintf(w, "Agent:   %s\n", report.Agent)
	if report.Model != "" {
		fmt.Fprintf(w, "Model:   %s\n", report.Model)
	}
	fmt.Fprintf(w, "Status:  %s\n", report.Status)

	writeTokenUsageSection(w, report.Tokens)
	if len(report.Recommendations) > 0 {
		writeTokenRecommendations(w, report.Recommendations)
	}

	writeTokenContributors(w, report.Contributors, report.Context)
	writeTokenLimitations(w, report.Limitations)
}

func writeSessionTokensAgentBrief(w io.Writer, report sessionTokensReport) {
	fmt.Fprintln(w, "Session token brief")
	fmt.Fprintf(w, "Session: %s\n", report.SessionID)
	fmt.Fprintln(w)
	fmt.Fprintln(w, agentBriefUsageLine(report.Tokens))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next best action:")
	fmt.Fprintln(w, agentBriefNextAction(report))

	signals := agentBriefSignals(report)
	if len(signals) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Signals:")
		for _, signal := range signals {
			fmt.Fprintf(w, "- %s\n", signal)
		}
	}
}

func agentBriefUsageLine(tokens *sessionTokensUsage) string {
	if tokens == nil {
		return "Token usage: unavailable."
	}
	if tokens.CacheRead > 0 {
		return fmt.Sprintf(
			"Token usage: %s total; %s cache/context replay; %s.",
			formatTokenCount(tokens.Total),
			formatPercent(tokenPercent(tokens.CacheRead, topLevelSessionTokenTotal(tokens))),
			formatAPICalls(tokens.APICalls),
		)
	}
	return fmt.Sprintf("Token usage: %s total; %s.", formatTokenCount(tokens.Total), formatAPICalls(tokens.APICalls))
}

func formatAPICalls(count int) string {
	if count == 1 {
		return "1 API call"
	}
	return fmt.Sprintf("%d API calls", count)
}

func agentBriefNextAction(report sessionTokensReport) string {
	if hasTokenRecommendation(report, "no-token-data") {
		return "Token usage is not available yet. Use this as a context check, not a spend diagnosis; continue after the next checkpoint captures usage."
	}
	if action, ok := agentBriefOptimizationAction(report); ok {
		return action
	}
	return "Continue normally; no high-signal token optimization is available from this session yet."
}

func agentBriefOptimizationAction(report sessionTokensReport) (string, bool) {
	switch {
	case (hasTokenRecommendation(report, "cache-write-pressure") || hasTokenRecommendation(report, "output-pressure")) &&
		(hasTokenRecommendation(report, "context-replay-hotspot") || hasTokenRecommendation(report, "api-call-amplification")):
		return agentBriefCostProxyBatchAction, true
	case hasTokenRecommendation(report, "cache-write-pressure") && hasTokenRecommendation(report, "output-pressure"):
		return agentBriefCostProxyBatchAction, true
	case hasTokenRecommendation(report, "cache-write-pressure"):
		return "Use at most 3 batched reads and avoid broad new context until you have one narrowed hypothesis.", true
	case hasTokenRecommendation(report, "output-pressure"):
		return "Keep the next answer tight; cite only necessary evidence and avoid restating prior context.", true
	case hasTokenRecommendation(report, "context-replay-hotspot") && hasTokenRecommendation(report, "api-call-amplification"):
		return agentBriefCostProxyBatchAction, true
	case hasTokenRecommendation(report, "api-call-amplification"):
		return agentBriefCostProxyBatchAction, true
	case hasTokenRecommendation(report, "context-replay-hotspot"):
		return "Use at most 2 focused reads only if a named file or test can change the answer; otherwise answer now. Avoid broad grep, broad diffs, and broad tests.", true
	case hasTokenRecommendation(report, "subagent-heavy"):
		return "Do not launch broad subagents. Use one narrowly scoped check with a concrete expected output.", true
	case hasTokenRecommendation(report, "high-context-pressure"):
		return "Preserve useful findings, then answer with at most 2 focused reads if more evidence is required.", true
	case hasTokenRecommendation(report, "long-session"):
		return "Summarize useful findings and stop unless one focused read can change the answer.", true
	default:
		return "", false
	}
}

func agentBriefSignals(report sessionTokensReport) []string {
	var signals []string
	if hasTokenRecommendation(report, "context-replay-hotspot") {
		signals = append(signals, "Cache/context replay dominates token volume.")
	}
	if hasTokenRecommendation(report, "api-call-amplification") {
		signals = append(signals, "API call count is high for one session.")
	}
	if hasTokenRecommendation(report, "cache-write-pressure") {
		signals = append(signals, "Cache write/new context pressure is elevated.")
	}
	if hasTokenRecommendation(report, "output-pressure") {
		signals = append(signals, "Output pressure is elevated.")
	}
	if hasTokenRecommendation(report, "subagent-heavy") {
		signals = append(signals, "Subagent usage is a meaningful part of total tokens.")
	}
	if hasTokenRecommendation(report, "high-context-pressure") {
		signals = append(signals, "Context pressure is high.")
	}
	if hasTokenRecommendation(report, "long-session") {
		signals = append(signals, "Session has crossed a long-session or checkpoint boundary.")
	}
	if hasTokenRecommendation(report, "no-token-data") {
		signals = append([]string{"Token usage is unavailable for this session."}, signals...)
	}
	if len(signals) == 0 && report.Tokens != nil {
		signals = append(signals, "No high-signal token risk detected from captured usage.")
	}
	return signals
}

func hasTokenRecommendation(report sessionTokensReport, id string) bool {
	for _, rec := range report.Recommendations {
		if rec.ID == id {
			return true
		}
	}
	return false
}

func writeTokenRecommendations(w io.Writer, recs []sessionTokensRecommendation) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Recommendations")
	for _, rec := range recs {
		fmt.Fprintf(w, "- %s\n", rec.Message)
	}
}

func writeTokenUsageSection(w io.Writer, tokens *sessionTokensUsage) {
	writeTokenUsageSectionWithTitle(w, "Token usage", tokens)
}

func writeTokenUsageSectionWithTitle(w io.Writer, title string, tokens *sessionTokensUsage) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, title)
	if tokens != nil {
		fmt.Fprintf(w, "Total:  %s tokens\n", formatTokenCount(tokens.Total))
		parts := []string{
			"Input: " + formatTokenCount(tokens.Input),
			"Cache read: " + formatTokenCount(tokens.CacheRead),
			"Cache write: " + formatTokenCount(tokens.CacheWrite),
			"Output: " + formatTokenCount(tokens.Output),
			fmt.Sprintf("API calls: %d", tokens.APICalls),
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, " | "))
	} else {
		fmt.Fprintln(w, "Token data: unavailable")
	}
}

func writeTokenContributors(w io.Writer, contributors []sessionTokensContributor, contextInfo *sessionTokensContext) {
	if len(contributors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Likely contributors")
		for _, contributor := range contributors {
			switch contributor.Kind {
			case "subagents":
				fmt.Fprintf(w, "- %s: %s tokens\n", contributor.Label, formatTokenCount(contributor.Tokens))
			case "context_pressure":
				if contextInfo != nil {
					fmt.Fprintf(w, "- %s: %d%% of %s tokens\n", contributor.Label, contextInfo.Percent, formatTokenCount(contextInfo.WindowSize))
				}
			default:
				fmt.Fprintf(w, "- %s\n", contributor.Label)
			}
		}
	}
}

func writeTokenLimitations(w io.Writer, limitations []string) {
	if len(limitations) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Limitations")
		for _, limitation := range limitations {
			fmt.Fprintf(w, "- %s\n", limitation)
		}
	}
}
