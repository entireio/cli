package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// checkpointTokensLegacyScope is the header line of a legacy checkpoint whose
// token_usage may be the session's running total.
const checkpointTokensLegacyScope = "Token scope: legacy — may be the session's running total (written before v0.11)" //nolint:gosec // G101: a display label, not a credential

// writeCheckpointTokensText prints the breakdown-first report: the checkpoint
// header, then the shared body (Where it went, Usage, Recommendations,
// Notes), with the --compare section between the usage table and the
// recommendations when present.
func writeCheckpointTokensText(w io.Writer, report *checkpointTokensReport) {
	fmt.Fprintln(w, "Checkpoint tokens")
	fmt.Fprintln(w)
	writeCheckpointTokensHeader(w, report)
	v := &report.view
	if v.Attributed {
		writeTokenWhereItWent(w, v)
	}
	writeTokenUsageTable(w, v)
	if v.AgentReportedCost > 0 {
		fmt.Fprintf(w, "  Agent-reported cost $%s\n", strconv.FormatFloat(v.AgentReportedCost, 'f', 2, 64))
	}
	writeCheckpointTokenComparison(w, report.Comparison)
	writeTokenRecommendationSentences(w, v.Recommendations)
	writeTokenNotes(w, report.Limitations)
}

// writeCheckpointTokensHeader prints the identity lines: checkpoint, agent
// and model on one line; the session ID or count; duration, calls, volume
// and effort; branch; and the legacy scope label when it applies.
func writeCheckpointTokensHeader(w io.Writer, report *checkpointTokensReport) {
	v := &report.view
	agentLabel, agents := "Agent", report.Agents
	if len(agents) > 1 {
		agentLabel = "Agents"
	}
	modelLabel, models := "Model", report.Models
	if len(models) > 1 {
		modelLabel = "Models"
	}
	first := []string{"Checkpoint: " + report.CheckpointID}
	if len(agents) > 0 {
		first = append(first, agentLabel+": "+strings.Join(agents, ", "))
	}
	if len(models) > 0 {
		first = append(first, modelLabel+": "+strings.Join(models, ", "))
	}
	fmt.Fprintln(w, strings.Join(first, "      "))
	if report.SessionCount > 1 {
		fmt.Fprintf(w, "Sessions:   %d\n", report.SessionCount)
	} else if report.SessionID != "" {
		fmt.Fprintf(w, "Session:    %s\n", report.SessionID)
	}
	duration := "Duration:   " + tokenDurationLine(v)
	if effort := tokenEffortHeader(v); effort != "" {
		duration += "      " + effort
	}
	fmt.Fprintln(w, duration)
	if report.Branch != "" {
		fmt.Fprintf(w, "Branch:     %s\n", report.Branch)
	}
	if v.Legacy != nil && v.Legacy.Cumulative {
		fmt.Fprintln(w, checkpointTokensLegacyScope)
	}
}

// writeCheckpointTokenComparison prints the --compare section: the per-class
// token deltas, the cost-share deltas when priced, and the qualification.
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
		fmt.Fprintf(w, "API calls: %s\n", formatCheckpointMetricDelta(comparison.APICalls, strconv.Itoa))
		writeCheckpointCostShareComparison(w, comparison.CostShare)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Qualification")
	fmt.Fprintln(w, comparison.Qualification)
}

// writeCheckpointCostShareComparison prints one line per class's cost-share
// change; nothing when the shares were not compared.
func writeCheckpointCostShareComparison(w io.Writer, d *checkpointTokensCostShareDelta) {
	if d == nil {
		return
	}
	fmt.Fprintf(w, "Cost share, input: %s\n", formatCheckpointShareDelta(d.Input))
	fmt.Fprintf(w, "Cost share, cache write: %s\n", formatCheckpointShareDelta(d.CacheWrite))
	fmt.Fprintf(w, "Cost share, cache read: %s\n", formatCheckpointShareDelta(d.CacheRead))
	fmt.Fprintf(w, "Cost share, output: %s\n", formatCheckpointShareDelta(d.Output))
}

// formatCheckpointShareDelta renders "up 12 points (36% -> 48%)" or
// "unchanged (23% -> 23%)".
func formatCheckpointShareDelta(delta *checkpointTokensShareDelta) string {
	if delta == nil {
		return "unavailable"
	}
	from, to := tokenreport.FormatPercent(delta.Baseline), tokenreport.FormatPercent(delta.Current)
	if delta.Direction == checkpointDeltaDirectionUnchanged {
		return fmt.Sprintf("unchanged (%s -> %s)", from, to)
	}
	return fmt.Sprintf("%s %d points (%s -> %s)", delta.Direction, absInt(sharePoints(delta.Change)), from, to)
}

// formatCheckpointMetricDelta renders "down 50.5% (1M -> 500k)", "up (0 ->
// 3)" when the baseline was zero, or "unchanged (200 -> 200)".
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

// absFloat is |value|.
func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
