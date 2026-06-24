package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/spf13/cobra"
)

type checkpointTokensReport struct {
	CheckpointID    string                        `json:"checkpoint_id"`
	SessionCount    int                           `json:"session_count"`
	SessionID       string                        `json:"session_id,omitempty"`
	Agent           string                        `json:"agent,omitempty"`
	Agents          []string                      `json:"agents,omitempty"`
	Model           string                        `json:"model,omitempty"`
	Models          []string                      `json:"models,omitempty"`
	Branch          string                        `json:"branch,omitempty"`
	Source          string                        `json:"source"`
	Tokens          *sessionTokensUsage           `json:"tokens,omitempty"`
	Context         *sessionTokensContext         `json:"context,omitempty"`
	Contributors    []sessionTokensContributor    `json:"contributors,omitempty"`
	Recommendations []sessionTokensRecommendation `json:"recommendations,omitempty"`
	Comparison      *checkpointTokensComparison   `json:"comparison,omitempty"`
	Limitations     []string                      `json:"limitations,omitempty"`
}

type checkpointTokensComparison struct {
	BaselineCheckpointID string                       `json:"baseline_checkpoint_id"`
	TargetCheckpointID   string                       `json:"target_checkpoint_id"`
	Status               string                       `json:"status"`
	Total                *checkpointTokensMetricDelta `json:"total,omitempty"`
	Input                *checkpointTokensMetricDelta `json:"input,omitempty"`
	CacheRead            *checkpointTokensMetricDelta `json:"cache_read,omitempty"`
	CacheWrite           *checkpointTokensMetricDelta `json:"cache_write,omitempty"`
	Output               *checkpointTokensMetricDelta `json:"output,omitempty"`
	APICalls             *checkpointTokensMetricDelta `json:"api_calls,omitempty"`
	CacheReadCaveat      string                       `json:"cache_read_caveat,omitempty"`
	Qualification        string                       `json:"qualification"`
	Limitations          []string                     `json:"limitations,omitempty"`
}

type checkpointTokensMetricDelta struct {
	Baseline      int      `json:"baseline"`
	Current       int      `json:"current"`
	Change        int      `json:"change"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	Direction     string   `json:"direction"`
}

const (
	checkpointComparisonStatusUnavailable       = "unavailable"
	checkpointComparisonStatusObservedReduction = "observed_reduction"
	checkpointComparisonStatusObservedIncrease  = "observed_increase"
	checkpointComparisonStatusObservedNoChange  = "observed_no_change"

	checkpointDeltaDirectionDown      = "down"
	checkpointDeltaDirectionUp        = "up"
	checkpointDeltaDirectionUnchanged = "unchanged"
)

func newCheckpointTokensCmd() *cobra.Command {
	var jsonFlag bool
	var compareFlag string
	var agentBriefFlag bool

	cmd := &cobra.Command{
		Use:   "tokens <checkpoint-id>",
		Short: "Show token usage and optimization recommendations for a checkpoint",
		Long: `Show token usage and optimization recommendations for a checkpoint.

The report reads committed checkpoint metadata using the same checkpoint
resolution path as 'entire checkpoint explain'. Checkpoint IDs may be abbreviated
as long as the prefix is unambiguous; positional targets may also resolve from a
commit ref with an Entire-Checkpoint trailer, and missing metadata may be fetched
from the checkpoint remote.

Use --compare <checkpoint-id> to compare this checkpoint against a previous
checkpoint and qualify observed token reduction or increase.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonFlag && agentBriefFlag {
				return errors.New("--json and --agent-brief are mutually exclusive")
			}
			return runCheckpointTokens(cmd.Context(), cmd, args[0], jsonFlag, compareFlag, agentBriefFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&compareFlag, "compare", "", "Compare against a baseline checkpoint ID")
	cmd.Flags().BoolVar(&agentBriefFlag, "agent-brief", false, "Output compact next-step guidance for agents")
	return cmd
}

func runCheckpointTokens(ctx context.Context, cmd *cobra.Command, checkpointIDPrefix string, jsonOutput bool, comparePrefix string, agentBrief bool) error {
	report, lookup, err := loadCheckpointTokensReport(ctx, cmd, checkpointIDPrefix)
	if lookup != nil {
		defer lookup.Close()
	}
	if err != nil {
		return tokenCommandError(err)
	}

	if comparePrefix != "" {
		baselineReport, baselineLookup, err := loadCheckpointTokensReport(ctx, cmd, comparePrefix)
		if baselineLookup != nil {
			defer baselineLookup.Close()
		}
		if err != nil {
			return tokenCommandError(err)
		}
		if baselineReport.CheckpointID == report.CheckpointID {
			cmd.SilenceUsage = true
			return fmt.Errorf("cannot compare checkpoint %s to itself", report.CheckpointID)
		}
		report.Comparison = buildCheckpointTokensComparison(report, baselineReport)
	}

	if jsonOutput {
		return writeCheckpointTokensJSON(cmd.OutOrStdout(), report)
	}
	if agentBrief {
		writeCheckpointTokensAgentBrief(cmd.OutOrStdout(), report)
		return nil
	}
	writeCheckpointTokensText(cmd.OutOrStdout(), report)
	return nil
}

func loadCheckpointTokensReport(ctx context.Context, cmd *cobra.Command, checkpointIDPrefix string) (checkpointTokensReport, *explainCheckpointLookup, error) {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, cmd.ErrOrStderr(), explainExportOptions{target: checkpointIDPrefix})
	if err != nil {
		return checkpointTokensReport{}, lookup, err
	}

	summary, err := lookup.store.Read(ctx, cpID)
	if err != nil {
		return checkpointTokensReport{}, lookup, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if summary == nil || len(summary.Sessions) == 0 {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Checkpoint not found.")
		return checkpointTokensReport{}, lookup, NewSilentError(fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, checkpointIDPrefix))
	}

	metas, metadataWarnings, err := readCheckpointTokenSessionMetadata(ctx, lookup.store, cpID, len(summary.Sessions))
	if err != nil {
		return checkpointTokensReport{}, lookup, err
	}

	return buildCheckpointTokensReport(cpID, summary, metas, metadataWarnings), lookup, nil
}

func readCheckpointTokenSessionMetadata(ctx context.Context, store reviewContextSessionMetadataReader, cpID id.CheckpointID, sessionCount int) ([]*checkpoint.Metadata, int, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, ctxErr //nolint:wrapcheck // Propagating context cancellation.
	}
	metas := make([]*checkpoint.Metadata, 0, sessionCount)
	var warnings int
	for i := range sessionCount {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, warnings, ctxErr //nolint:wrapcheck // Propagating context cancellation.
		}
		meta, err := store.ReadSessionMetadata(ctx, cpID, i)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, warnings, ctxErr //nolint:wrapcheck // Propagating context cancellation.
			}
			warnings++
			continue
		}
		metas = append(metas, meta)
	}
	return metas, warnings, nil
}

func buildCheckpointTokensReport(cpID id.CheckpointID, summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata, metadataWarnings int) checkpointTokensReport {
	report := checkpointTokensReport{
		CheckpointID: cpID.String(),
		Source:       "committed_checkpoint",
	}
	if summary != nil {
		report.Branch = summary.Branch
		report.SessionCount = len(summary.Sessions)
	}
	if report.SessionCount == 0 {
		report.SessionCount = len(metas)
	}
	report.Agents = checkpointAgentLabels(metas)
	report.Models = checkpointModelLabels(metas)

	if report.SessionCount == 1 && len(metas) == 1 && metas[0] != nil {
		meta := metas[0]
		report.SessionID = meta.SessionID
		if len(report.Agents) > 0 {
			report.Agent = report.Agents[0]
		}
		if len(report.Models) > 0 {
			report.Model = report.Models[0]
		}
		if report.Branch == "" {
			report.Branch = meta.Branch
		}
	} else if len(metas) > 1 && report.Branch == "" {
		report.Branch = firstCheckpointBranch(metas)
	}

	usage := checkpointTokenUsage(summary, metas, metadataWarnings > 0)
	if tokens := buildSessionTokensUsage(usage); tokens != nil {
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
		report.Limitations = append(report.Limitations, "No token usage recorded for this checkpoint.")
		report.Recommendations = append(report.Recommendations, sessionTokensRecommendation{
			ID:       "no-token-data",
			Severity: "low",
			Message:  "Token usage is unavailable for this checkpoint; the agent may not expose token data yet, or this checkpoint predates token tracking.",
			Signals:  []string{"missing_token_usage"},
		})
	}
	if metadataWarnings > 0 {
		report.Limitations = append(report.Limitations, fmt.Sprintf(
			"%d checkpoint session metadata file%s could not be read; used root token summary or readable session metadata where available.",
			metadataWarnings,
			tokenPluralSuffix(metadataWarnings),
		))
	}

	var turnCount int
	var skillEvents []agent.SkillEvent
	if report.SessionCount == 1 && len(metas) == 1 && metas[0] != nil {
		meta := metas[0]
		if metrics := meta.SessionMetrics; metrics != nil {
			turnCount = metrics.TurnCount
			if contextInfo := buildSessionTokensContext(metrics.ContextTokens, metrics.ContextWindowSize); contextInfo != nil {
				report.Context = contextInfo
				report.Contributors = append(report.Contributors, sessionTokensContributor{
					Kind:       "context_pressure",
					Label:      "Context pressure",
					Percent:    contextInfo.Percent,
					Confidence: "reported",
					Signals:    []string{"context_tokens"},
				})
			}
		}
		skillEvents = meta.SkillEvents
	} else {
		for _, meta := range metas {
			if meta == nil {
				continue
			}
			if metrics := meta.SessionMetrics; metrics != nil {
				turnCount += metrics.TurnCount
			}
			skillEvents = append(skillEvents, meta.SkillEvents...)
		}
	}
	if labels := skillEventLabels(skillEvents); len(labels) > 0 {
		report.Contributors = append(report.Contributors, sessionTokensContributor{
			Kind:       "skills",
			Label:      "Skills/slash commands: " + strings.Join(labels, ", "),
			Confidence: "reported",
			Signals:    []string{"skill_events"},
		})
	}

	var checkpointCount int
	if summary != nil {
		checkpointCount = summary.CheckpointsCount
	}
	report.Recommendations = append(report.Recommendations, recommendationRules(tokenRecommendationSignals{
		Tokens:          report.Tokens,
		Context:         report.Context,
		TurnCount:       turnCount,
		CheckpointCount: checkpointCount,
	})...)
	return report
}

func checkpointAgentLabels(metas []*checkpoint.Metadata) []string {
	labels := make([]string, 0, len(metas))
	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		label := unknownPlaceholder
		if meta != nil && meta.Agent != "" {
			label = string(meta.Agent)
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func checkpointModelLabels(metas []*checkpoint.Metadata) []string {
	labels := make([]string, 0, len(metas))
	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		if meta == nil || meta.Model == "" {
			continue
		}
		if _, ok := seen[meta.Model]; ok {
			continue
		}
		seen[meta.Model] = struct{}{}
		labels = append(labels, meta.Model)
	}
	return labels
}

func firstCheckpointBranch(metas []*checkpoint.Metadata) string {
	for _, meta := range metas {
		if meta != nil && meta.Branch != "" {
			return meta.Branch
		}
	}
	return ""
}

func aggregateCheckpointTokenUsage(metas []*checkpoint.Metadata) *agent.TokenUsage {
	var total *agent.TokenUsage
	for _, meta := range metas {
		if meta == nil {
			continue
		}
		total = addCheckpointTokenUsage(total, meta.TokenUsage)
	}
	return total
}

func checkpointTokenUsage(summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata, metadataReadWarning bool) *agent.TokenUsage {
	sessionUsage := aggregateCheckpointTokenUsage(metas)
	if !metadataReadWarning && sessionUsage != nil {
		return sessionUsage
	}
	if summary != nil && summary.TokenUsage != nil {
		return summary.TokenUsage
	}
	return sessionUsage
}

func addCheckpointTokenUsage(a, b *agent.TokenUsage) *agent.TokenUsage {
	if a == nil && b == nil {
		return nil
	}
	result := &agent.TokenUsage{}
	if a != nil {
		result.InputTokens = a.InputTokens
		result.CacheCreationTokens = a.CacheCreationTokens
		result.CacheReadTokens = a.CacheReadTokens
		result.OutputTokens = a.OutputTokens
		result.APICallCount = a.APICallCount
	}
	if b != nil {
		result.InputTokens = saturatingIntAdd(result.InputTokens, b.InputTokens)
		result.CacheCreationTokens = saturatingIntAdd(result.CacheCreationTokens, b.CacheCreationTokens)
		result.CacheReadTokens = saturatingIntAdd(result.CacheReadTokens, b.CacheReadTokens)
		result.OutputTokens = saturatingIntAdd(result.OutputTokens, b.OutputTokens)
		result.APICallCount = saturatingIntAdd(result.APICallCount, b.APICallCount)
	}
	result.SubagentTokens = addCheckpointTokenUsage(tokenUsageSubagents(a), tokenUsageSubagents(b))
	return result
}

func saturatingIntAdd(a, b int) int {
	maxValue := int(^uint(0) >> 1)
	minValue := -maxValue - 1
	if b > 0 && a > maxValue-b {
		return maxValue
	}
	if b < 0 && a < minValue-b {
		return minValue
	}
	return a + b
}

func tokenUsageSubagents(usage *agent.TokenUsage) *agent.TokenUsage {
	if usage == nil {
		return nil
	}
	return usage.SubagentTokens
}

func tokenPluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func buildCheckpointTokensComparison(target, baseline checkpointTokensReport) *checkpointTokensComparison {
	comparison := &checkpointTokensComparison{
		BaselineCheckpointID: baseline.CheckpointID,
		TargetCheckpointID:   target.CheckpointID,
	}
	if target.Tokens == nil || baseline.Tokens == nil {
		comparison.Status = checkpointComparisonStatusUnavailable
		comparison.Qualification = "Comparison unavailable because token usage is missing for one checkpoint."
		comparison.Limitations = append(comparison.Limitations, comparison.Qualification)
		return comparison
	}

	comparison.Total = buildCheckpointMetricDelta(baseline.Tokens.Total, target.Tokens.Total)
	comparison.Input = buildCheckpointMetricDelta(baseline.Tokens.Input, target.Tokens.Input)
	comparison.CacheRead = buildCheckpointMetricDelta(baseline.Tokens.CacheRead, target.Tokens.CacheRead)
	comparison.CacheWrite = buildCheckpointMetricDelta(baseline.Tokens.CacheWrite, target.Tokens.CacheWrite)
	comparison.Output = buildCheckpointMetricDelta(baseline.Tokens.Output, target.Tokens.Output)
	comparison.APICalls = buildCheckpointMetricDelta(baseline.Tokens.APICalls, target.Tokens.APICalls)
	comparison.CacheReadCaveat = checkpointComparisonCacheReadCaveat(comparison.CacheRead)
	comparison.Status = checkpointComparisonStatus(comparison.Total)
	comparison.Qualification = checkpointComparisonQualification(comparison.Status)
	if classes := checkpointCostProxyPressureIncreased(comparison); len(classes) > 0 {
		comparison.Qualification += fmt.Sprintf(" Cost-proxy pressure increased for %s even though total tokens decreased.", formatTokenClassList(classes))
	}
	return comparison
}

func checkpointCostProxyPressureIncreased(comparison *checkpointTokensComparison) []string {
	if comparison == nil || comparison.Total == nil || comparison.Total.Change >= 0 {
		return nil
	}
	var classes []string
	if comparison.CacheWrite != nil && comparison.CacheWrite.Change > 0 {
		classes = append(classes, "cache write")
	}
	if comparison.Output != nil && comparison.Output.Change > 0 {
		classes = append(classes, "output")
	}
	return classes
}

func formatTokenClassList(classes []string) string {
	switch len(classes) {
	case 0:
		return ""
	case 1:
		return classes[0]
	case 2:
		return classes[0] + " and " + classes[1]
	default:
		return strings.Join(classes[:len(classes)-1], ", ") + ", and " + classes[len(classes)-1]
	}
}

func buildCheckpointMetricDelta(baseline, current int) *checkpointTokensMetricDelta {
	change := saturatingIntSub(current, baseline)
	delta := &checkpointTokensMetricDelta{
		Baseline:  baseline,
		Current:   current,
		Change:    change,
		Direction: checkpointDeltaDirection(change),
	}
	if baseline != 0 {
		percent := (float64(delta.Change) / float64(baseline)) * 100
		delta.ChangePercent = &percent
	}
	return delta
}

func saturatingIntSub(a, b int) int {
	if b < 0 {
		if b == minInt() {
			if a >= 0 {
				return maxInt()
			}
			return a - b
		}
		if a > maxInt()-(-b) {
			return maxInt()
		}
	}
	if b > 0 && a < minInt()+b {
		return minInt()
	}
	return a - b
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func minInt() int {
	return -maxInt() - 1
}

func checkpointDeltaDirection(change int) string {
	switch {
	case change < 0:
		return checkpointDeltaDirectionDown
	case change > 0:
		return checkpointDeltaDirectionUp
	default:
		return checkpointDeltaDirectionUnchanged
	}
}

func checkpointComparisonStatus(total *checkpointTokensMetricDelta) string {
	if total == nil {
		return checkpointComparisonStatusUnavailable
	}
	switch total.Direction {
	case checkpointDeltaDirectionDown:
		return checkpointComparisonStatusObservedReduction
	case checkpointDeltaDirectionUp:
		return checkpointComparisonStatusObservedIncrease
	default:
		return checkpointComparisonStatusObservedNoChange
	}
}

func checkpointComparisonQualification(status string) string {
	switch status {
	case checkpointComparisonStatusObservedReduction:
		return "Observed total token use decreased for this checkpoint comparison. This does not prove quality was preserved; verify the task outcome or tests before treating it as a successful optimization."
	case checkpointComparisonStatusObservedIncrease:
		return "Observed total token use increased for this checkpoint comparison. Check whether the extra context was necessary before treating it as waste."
	case checkpointComparisonStatusObservedNoChange:
		return "Observed total token use was unchanged for this checkpoint comparison. Quality still depends on the task outcome, not token totals alone."
	default:
		return "Comparison unavailable because token usage is missing for one checkpoint."
	}
}

func checkpointComparisonCacheReadCaveat(delta *checkpointTokensMetricDelta) string {
	if delta == nil || (delta.Baseline == 0 && delta.Current == 0) {
		return ""
	}
	return "Total tokens include cache/context replay; use the cache/context replay delta below before treating total direction as work saved or added."
}

func writeCheckpointTokensJSON(w io.Writer, report checkpointTokensReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("failed to encode checkpoint token report: %w", err)
	}
	return nil
}

func writeCheckpointTokensText(w io.Writer, report checkpointTokensReport) {
	fmt.Fprintln(w, "Checkpoint tokens")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Checkpoint: %s\n", report.CheckpointID)
	switch {
	case report.SessionCount > 1:
		fmt.Fprintf(w, "Sessions:   %d\n", report.SessionCount)
		if len(report.Agents) > 0 {
			fmt.Fprintf(w, "Agents:     %s\n", strings.Join(report.Agents, ", "))
		}
		if len(report.Models) > 0 {
			fmt.Fprintf(w, "Models:     %s\n", strings.Join(report.Models, ", "))
		}
	case report.SessionID != "":
		fmt.Fprintf(w, "Session:    %s\n", report.SessionID)
		if report.Agent != "" {
			fmt.Fprintf(w, "Agent:      %s\n", report.Agent)
		}
		if report.Model != "" {
			fmt.Fprintf(w, "Model:      %s\n", report.Model)
		}
	case report.Agent != "":
		fmt.Fprintf(w, "Agent:      %s\n", report.Agent)
	}
	if report.Branch != "" {
		fmt.Fprintf(w, "Branch:     %s\n", report.Branch)
	}

	writeTokenUsageSection(w, report.Tokens)
	writeCheckpointTokenComparison(w, report.Comparison)
	if len(report.Recommendations) > 0 {
		writeTokenRecommendations(w, report.Recommendations)
	}
	writeTokenContributors(w, report.Contributors, report.Context)
	writeTokenLimitations(w, report.Limitations)
}

func writeCheckpointTokensAgentBrief(w io.Writer, report checkpointTokensReport) {
	fmt.Fprintln(w, "Checkpoint token brief")
	fmt.Fprintf(w, "Checkpoint: %s\n", report.CheckpointID)
	fmt.Fprintln(w)
	fmt.Fprintln(w, agentBriefUsageLine(report.Tokens))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next best action:")
	fmt.Fprintln(w, checkpointAgentBriefNextAction(report))

	signals := agentBriefSignals(checkpointAgentBriefSessionReport(report))
	if len(signals) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Signals:")
		for _, signal := range signals {
			fmt.Fprintf(w, "- %s\n", signal)
		}
	}
}

func checkpointAgentBriefNextAction(report checkpointTokensReport) string {
	sessionReport := checkpointAgentBriefSessionReport(report)
	if hasTokenRecommendation(sessionReport, "no-token-data") {
		return "Do not spend extra commands on token optimization for this checkpoint. Continue with the task and capture a newer checkpoint before rechecking tokens."
	}
	if action, ok := agentBriefOptimizationAction(sessionReport); ok {
		return action
	}
	return "Continue normally; no high-signal token optimization is available from this checkpoint."
}

func checkpointAgentBriefSessionReport(report checkpointTokensReport) sessionTokensReport {
	return sessionTokensReport{
		Tokens:          report.Tokens,
		Context:         report.Context,
		Recommendations: report.Recommendations,
		Limitations:     report.Limitations,
	}
}

func writeCheckpointTokenComparison(w io.Writer, comparison *checkpointTokensComparison) {
	if comparison == nil {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Comparison")
	fmt.Fprintf(w, "Baseline: %s\n", comparison.BaselineCheckpointID)
	if comparison.CacheReadCaveat != "" {
		fmt.Fprintf(w, "Caveat: %s\n", comparison.CacheReadCaveat)
	}
	if comparison.Status != checkpointComparisonStatusUnavailable {
		fmt.Fprintf(w, "Total tokens: %s\n", formatCheckpointMetricDelta(comparison.Total, formatTokenCount))
		fmt.Fprintf(w, "Input: %s\n", formatCheckpointMetricDelta(comparison.Input, formatTokenCount))
		fmt.Fprintf(w, "Cache/context replay: %s\n", formatCheckpointMetricDelta(comparison.CacheRead, formatTokenCount))
		fmt.Fprintf(w, "Cache write: %s\n", formatCheckpointMetricDelta(comparison.CacheWrite, formatTokenCount))
		fmt.Fprintf(w, "Output: %s\n", formatCheckpointMetricDelta(comparison.Output, formatTokenCount))
		fmt.Fprintf(w, "API calls: %s\n", formatCheckpointMetricDelta(comparison.APICalls, formatPlainCount))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Qualification")
	fmt.Fprintln(w, comparison.Qualification)
}

func formatCheckpointMetricDelta(delta *checkpointTokensMetricDelta, formatValue func(int) string) string {
	if delta == nil {
		return "unavailable"
	}
	from := formatValue(delta.Baseline)
	to := formatValue(delta.Current)
	if delta.Direction == checkpointDeltaDirectionUnchanged {
		return fmt.Sprintf("unchanged (%s -> %s)", from, to)
	}
	if delta.ChangePercent == nil {
		return fmt.Sprintf("%s (%s -> %s)", delta.Direction, from, to)
	}
	return fmt.Sprintf("%s %s (%s -> %s)", delta.Direction, formatPercent(absFloat(*delta.ChangePercent)), from, to)
}

func formatPlainCount(value int) string {
	return strconv.Itoa(value)
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
