package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

type sessionTokensReport struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Model     string `json:"model,omitempty"`
	Status    string `json:"status"`
	// Duration is how long the session has been worked on, as an interaction
	// span rather than elapsed wall-clock. Empty when no interaction has been
	// recorded.
	Duration   string                     `json:"duration,omitempty"`
	Source     string                     `json:"source"`
	Resolution strategy.SessionResolution `json:"resolution,omitempty"`
	Tokens     *sessionTokensUsage        `json:"tokens,omitempty"`
	// Classes is the billing-class breakdown — volume and, where a verified
	// ratio row applies, cost share per class. Built from the same resolver and
	// rendered by the same writer as `checkpoint tokens`, so a live session and
	// its own committed checkpoint cannot show different tables.
	Classes         *tokenClassBreakdown          `json:"classes,omitempty"`
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

When no session ID is provided, Entire identifies the caller using agent session
IDs and process ancestry, falling back to the most recently active session in
this worktree and then elsewhere in the repository. The report includes how the
session was resolved; ambiguous and cross-worktree matches also warn on stderr.
Use --current to select only the current worktree's most recent session.
The report uses token and context data Entire already captured for the session.

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
	resolution := strategy.ResolutionNone
	if sessionID == "" {
		// --current pins the answer to this worktree and nothing else, which
		// is the one thing the resolver deliberately will not do: it prefers
		// the caller's own session wherever that session lives. Keep the flag
		// literal, and let the default path identify the caller.
		if current {
			sessionID = strategy.FindMostRecentSessionInCurrentWorktree(ctx)
			resolution = strategy.ResolutionWorktree
		} else {
			resolved := strategy.ResolveCallerSession(ctx)
			if resolved.Found() && !resolved.Tracked {
				return reportUntrackedCallerSession(cmd, resolved, jsonOutput || agentBrief)
			}
			sessionID = resolved.SessionID
			resolution = resolved.Resolution
			if resolution == strategy.ResolutionCallerAmbiguous {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"[entire] Caller session is ambiguous; these tokens may belong to another session. Confirm the session ID before acting on the recommendations.")
			}
			if resolved.Resolution == strategy.ResolutionOtherWorktree {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"[entire] No session is recorded in this worktree; reporting the most recent one from elsewhere in this repository. It is not this command's caller.")
			}
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
	report.Resolution = resolution
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
		Duration:  sessionTokensDuration(state),
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
			ID:       recNoTokenData,
			Severity: "low",
			Message:  "Token usage is unavailable for this session; the agent may not expose token data yet, or no checkpoint has captured it.",
			Signals:  []string{"missing_token_usage"},
		})
	}

	if state.TokenUsage != nil {
		weights, unpricedReason := tokenWeightsForSession(state.ModelName, state.TokenUsage)
		if classes, ok := tokenClassShares(state.TokenUsage, weights, sessionTokenTTLKnown(state.AgentType)); ok {
			// tokenClassShares sees only empty weights and names the generic
			// reason; the resolver is the one that knows which case it was.
			if !classes.Priced && unpricedReason != "" {
				classes.UnpricedReason = unpricedReason
			}
			report.Classes = &classes
		}
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

// sessionTokensDuration reports how long the session has been worked on, as the
// span between its first and last recorded interaction. Elapsed wall-clock
// would count a session left open overnight as a twelve-hour session; a token
// report is about work done, not calendar time. Empty when no interaction has
// been recorded — "0m" would claim a measurement that was never taken.
func sessionTokensDuration(state *strategy.SessionState) string {
	if state == nil || state.LastInteractionTime == nil {
		return ""
	}
	span := state.LastInteractionTime.Sub(state.StartedAt)
	// Below a second there is no work span to report, and "0s so far" is the
	// same false claim as the "0m" this function exists to avoid: it states a
	// duration of zero for a session that has been worked on. Found by running
	// the command against a real session whose first turn ended immediately —
	// every unit test here uses a synthetic multi-hour span.
	if span < time.Second {
		return ""
	}
	// "so far" claims the work is ongoing, which is true of a live session and
	// false of a finished one — and this command is run against both
	// (sessionPhaseLabel reports "ended" off the same state). An ended
	// session's span is final, so it gets the bare figure.
	if state.EndedAt != nil {
		return formatDurationShort(span)
	}
	return formatDurationShort(span) + " so far"
}

// formatDurationShort renders a work span as "2h 14m", "14m" or "45s". Package
// cli has no helper for this shape: formatRelativeDuration (status.go) appends
// "ago", and formatSummaryDuration (explain.go) returns Duration.String(),
// which prints "2h14m0s".
//
// Units below the leading one are dropped once minutes are on the clock —
// seconds are noise beside hours of work — and a whole number of hours prints
// "3h", not "3h 0m".
func formatDurationShort(d time.Duration) string {
	switch {
	case d >= time.Hour:
		hours := int(d / time.Hour)
		minutes := int((d % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// sessionTokenTTLKnown reports whether an absent 1-hour cache-write figure can
// be trusted to mean zero on live state — which depends on the agent, not on
// the CLI version a checkpoint was written by.
//
// An earlier version returned true unconditionally, reasoning that the figure
// is written by the same binary now reading it, so absence means the provider
// reported none. That holds only for a parser that READS the field. Claude
// Code and Pi do; Factory AI Droid reads cache_creation_input_tokens and has no
// 1-hour field at all, so its figure is always zero — and treating that as "no
// 1-hour writes" prices every cache write at the 5-minute rate (1.25x) when the
// real ones bill at 2x, silently understating cost. Agents declare the property
// via agent.CacheWriteTTLRecorder; anything that does not is treated the way a
// legacy checkpoint is, with cost withheld rather than guessed.
func sessionTokenTTLKnown(agentType types.AgentType) bool {
	if agentType == "" {
		return false
	}
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return false
	}
	_, ok := agent.AsCacheWriteTTLRecorder(ag)
	return ok
}

func buildSessionTokensUsage(usage *agent.TokenUsage) *sessionTokensUsage {
	if usage == nil {
		return nil
	}
	// Total and the class figures come from one walk. flattenTokenUsageForClasses
	// folds subagent usage in, bounded at types.MaxSubagentDepth and clamped at
	// zero per field; totalTokens is neither, so deriving Total from it left this
	// struct's own parts not summing to its own total on a chain that was deeper
	// than the bound or carried negatives — two different answers under one label,
	// which is the bug the flattening exists to fix. Subagent usage stays
	// separately visible as SubagentTotal.
	flat := flattenTokenUsageForClasses(usage)
	total := sumTokenClasses(flat)
	if total == 0 && usage.APICallCount == 0 {
		return nil
	}
	return &sessionTokensUsage{
		Total:         total,
		Input:         flat.InputTokens,
		CacheRead:     flat.CacheReadTokens,
		CacheWrite:    flat.CacheCreationTokens,
		Output:        flat.OutputTokens,
		APICalls:      usage.APICallCount,
		SubagentTotal: totalTokens(usage.SubagentTokens),
	}
}

// subagentTotalOf reads the subagent figure off a usage block, tolerating the
// nil that means "no usage recorded at all".
func subagentTotalOf(tokens *sessionTokensUsage) int {
	if tokens == nil {
		return 0
	}
	return tokens.SubagentTotal
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

// Recommendation IDs. These are matched by string in several places
// (the agent brief, its signal list, tests), so a deletion compiles clean and
// every stale reference silently goes false. Naming them makes the next
// deletion a build failure instead.
const (
	recAPICallAmplification = "api-call-amplification"
	recSubagentHeavy        = "subagent-heavy"
	recHighContextPressure  = "high-context-pressure"
	recLongSession          = "long-session"
	recNoTokenData          = "no-token-data"
)

// cacheReadHotspot reports whether replayed context dominates this session.
//
// It is deliberately NOT a recommendation. Measured over 135 distinct
// committed checkpoints it was true of 91%, and the spec's own calibration
// says why: cache read is the largest class in ~90% of sessions, so on its own
// it describes the baseline rather than a finding. It survives as a qualifier
// because two things still need it — the API-call message, which asserts
// replay, and the agent brief's next action, which must not change just
// because a printed recommendation was removed.
func cacheReadHotspot(tokens *sessionTokensUsage) bool {
	if tokens == nil || tokens.CacheRead <= 0 {
		return false
	}
	return tokenPercent(tokens.CacheRead, topLevelSessionTokenTotal(tokens)) >= recommendationHighCacheReadPercent
}

func recommendationRules(signals tokenRecommendationSignals) []sessionTokensRecommendation {
	var recs []sessionTokensRecommendation

	hotspot := cacheReadHotspot(signals.Tokens)
	if signals.Tokens != nil && signals.Tokens.APICalls >= recommendationHighAPICalls {
		message := fmt.Sprintf("API call count is high for one session: %d calls. Batch the next diagnosis and reduce iterative calls.", signals.Tokens.APICalls)
		if hotspot {
			message = fmt.Sprintf("Large context was replayed across %d API calls; batch the next diagnosis and reduce iterative tool calls.", signals.Tokens.APICalls)
		}
		recs = append(recs, sessionTokensRecommendation{
			ID:       recAPICallAmplification,
			Severity: "medium",
			Message:  message,
			Signals:  []string{"api_call_count"},
		})
	}
	if signals.Tokens != nil && tokenShareAtLeastOneTenth(signals.Tokens.SubagentTotal, signals.Tokens.Total) {
		recs = append(recs, sessionTokensRecommendation{
			ID:       recSubagentHeavy,
			Severity: "medium",
			Message:  "Scope subagent tasks tightly; give each subagent a narrow objective and expected output.",
			Signals:  []string{"subagent_tokens"},
		})
	}
	if signals.Context != nil && signals.Context.Percent >= recommendationHighContextPercent {
		recs = append(recs, sessionTokensRecommendation{
			ID:       recHighContextPressure,
			Severity: "medium",
			Message:  fmt.Sprintf("Context pressure is %d%% of the window; preserve only relevant context before continuing.", signals.Context.Percent),
			Signals:  []string{"context_tokens"},
		})
	}
	if signals.TurnCount >= recommendationLongSessionTurns || signals.CheckpointCount >= recommendationLongSessionCheckpoints {
		recs = append(recs, sessionTokensRecommendation{
			ID:       recLongSession,
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
	if report.Resolution != strategy.ResolutionNone {
		fmt.Fprintf(w, "Resolved: %s\n", sessionResolutionLabel(report.Resolution))
	}
	fmt.Fprintf(w, "Agent:   %s\n", report.Agent)
	if report.Model != "" {
		fmt.Fprintf(w, "Model:   %s\n", report.Model)
	}
	fmt.Fprintf(w, "Status:  %s\n", report.Status)
	if report.Duration != "" {
		fmt.Fprintf(w, "Duration: %s\n", report.Duration)
	}

	writeTokenUsageSection(w, report.Tokens)
	writeTokenClasses(w, report.Classes, subagentTotalOf(report.Tokens))
	if len(report.Recommendations) > 0 {
		writeTokenRecommendations(w, report.Recommendations)
	}

	writeTokenContributors(w, report.Contributors, report.Context,
		subagentShareFitsBlock(report.Classes, subagentTotalOf(report.Tokens)))
	writeTokenLimitations(w, report.Limitations)
}

func writeSessionTokensAgentBrief(w io.Writer, report sessionTokensReport) {
	fmt.Fprintln(w, "Session token brief")
	fmt.Fprintf(w, "Session: %s\n", report.SessionID)
	if report.Resolution != strategy.ResolutionNone {
		fmt.Fprintf(w, "Resolved: %s\n", sessionResolutionLabel(report.Resolution))
	}
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
	if hasTokenRecommendation(report, recNoTokenData) {
		return "Token usage is not available yet. Use this as a context check, not a spend diagnosis; continue after the next checkpoint captures usage."
	}
	if action, ok := agentBriefOptimizationAction(report); ok {
		return action
	}
	return "Continue normally; no high-signal token optimization is available from this session yet."
}

func agentBriefOptimizationAction(report sessionTokensReport) (string, bool) {
	// The replay arm keys on the usage itself, not on a recommendation. PR 5a
	// stopped printing context-replay-hotspot because it fired on 91% of real
	// checkpoints, but the brief returns exactly one next action and always
	// returned something here; letting a recommendation deletion silently
	// change the agent's instruction is the coupling Decision 7 rejects.
	switch {
	case hasTokenRecommendation(report, recAPICallAmplification):
		return agentBriefCostProxyBatchAction, true
	case cacheReadHotspot(report.Tokens):
		return "Use at most 2 focused reads only if a named file or test can change the answer; otherwise answer now. Avoid broad grep, broad diffs, and broad tests.", true
	case hasTokenRecommendation(report, recSubagentHeavy):
		return "Do not launch broad subagents. Use one narrowly scoped check with a concrete expected output.", true
	case hasTokenRecommendation(report, recHighContextPressure):
		return "Preserve useful findings, then answer with at most 2 focused reads if more evidence is required.", true
	case hasTokenRecommendation(report, recLongSession):
		return "Summarize useful findings and stop unless one focused read can change the answer.", true
	default:
		return "", false
	}
}

func agentBriefSignals(report sessionTokensReport) []string {
	var signals []string
	if cacheReadHotspot(report.Tokens) {
		signals = append(signals, "Cache/context replay dominates token volume.")
	}
	if hasTokenRecommendation(report, recAPICallAmplification) {
		signals = append(signals, "API call count is high for one session.")
	}
	if hasTokenRecommendation(report, recSubagentHeavy) {
		signals = append(signals, "Subagent usage is a meaningful part of total tokens.")
	}
	if hasTokenRecommendation(report, recHighContextPressure) {
		signals = append(signals, "Context pressure is high.")
	}
	if hasTokenRecommendation(report, recLongSession) {
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

func writeTokenContributors(w io.Writer, contributors []sessionTokensContributor, contextInfo *sessionTokensContext, subagentInBlock bool) {
	lines := contributorLines(contributors, contextInfo, subagentInBlock)
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Likely contributors")
	for _, line := range lines {
		fmt.Fprintf(w, "- %s\n", line)
	}
}

// contributorLines renders the contributor lines, so that the section header
// and its body come from one pass. The guard used to be len(contributors) > 0,
// which counted entries that render nothing: context_pressure has always been
// silent without a context block, and the subagents entry became silent when
// its figure moved into the billed block (it stays in the report for --json).
// A session with subagent usage, no context data and no skill events therefore
// printed the "Likely contributors" header with nothing under it. Building the
// lines first makes the header impossible to disagree with its own body.
func contributorLines(contributors []sessionTokensContributor, contextInfo *sessionTokensContext, subagentInBlock bool) []string {
	lines := make([]string, 0, len(contributors))
	for _, contributor := range contributors {
		switch contributor.Kind {
		case "subagents":
			// Normally rendered inside the billed block, with its share of the
			// total (Decision 3) — the entry stays in the report for --json
			// either way. But the block's line is suppressed when it cannot
			// state a share, and the figure must not simply disappear from the
			// text: it was always visible before the move. Fall back to the
			// bare figure here, which claims to be a share of nothing.
			if !subagentInBlock {
				lines = append(lines, fmt.Sprintf("%s: %s tokens",
					contributor.Label, formatTokenCount(contributor.Tokens)))
			}
		case "context_pressure":
			if contextInfo != nil {
				lines = append(lines, fmt.Sprintf("%s: %d%% of %s tokens",
					contributor.Label, contextInfo.Percent, formatTokenCount(contextInfo.WindowSize)))
			}
		default:
			lines = append(lines, contributor.Label)
		}
	}
	return lines
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
