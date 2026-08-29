package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
	"github.com/spf13/cobra"
)

// checkpointTokensReport is the `checkpoint tokens` report: the checkpoint's
// identity, the shared token-report fields derived from view, and the
// optional --compare comparison. It is the --json document.
type checkpointTokensReport struct {
	CheckpointID string   `json:"checkpoint_id"`
	SessionCount int      `json:"session_count"`
	SessionID    string   `json:"session_id,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	Agents       []string `json:"agents,omitempty"`
	Model        string   `json:"model,omitempty"`
	Models       []string `json:"models,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	// Source is checkpointTokensSourceTranscript when every session's totals
	// were recomputed from its transcript, else checkpointTokensSourceCommitted.
	Source          string           `json:"source"`
	DurationSeconds int              `json:"duration_seconds,omitempty"`
	Effort          *tokenEffortJSON `json:"effort,omitempty"`
	Tokens          *tokenUsageJSON  `json:"tokens,omitempty"`
	// Context is the single session's hook-reported context pressure
	// (SessionMetrics), kept under the previous schema's key.
	Context           *sessionTokensContext       `json:"context,omitempty"`
	Cost              *tokenCostJSON              `json:"cost,omitempty"`
	Contributors      []tokenreport.Contributor   `json:"contributors"`
	Recommendations   []tokenRecommendationJSON   `json:"recommendations,omitempty"`
	AgentReportedCost float64                     `json:"agent_reported_cost,omitempty"`
	Legacy            *tokenLegacyInfo            `json:"legacy,omitempty"`
	Comparison        *checkpointTokensComparison `json:"comparison,omitempty"`
	Limitations       []string                    `json:"limitations,omitempty"`

	// view is what the text writers render; the JSON fields above are
	// derived from it by applyView.
	view tokenReportView
}

// applyView stores the view and derives the shared JSON fields from it.
func (r *checkpointTokensReport) applyView(v tokenReportView) {
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
	r.Legacy = v.Legacy
	r.Limitations = tokenReportNotes(&v)
}

// checkpointTokensComparison is the --compare result: per-class token deltas
// and, when both checkpoints were priced, per-class cost-share deltas.
type checkpointTokensComparison struct {
	BaselineCheckpointID string                          `json:"baseline_checkpoint_id"`
	TargetCheckpointID   string                          `json:"target_checkpoint_id"`
	Status               string                          `json:"status"`
	Total                *checkpointTokensMetricDelta    `json:"total,omitempty"`
	Input                *checkpointTokensMetricDelta    `json:"input,omitempty"`
	CacheRead            *checkpointTokensMetricDelta    `json:"cache_read,omitempty"`
	CacheWrite           *checkpointTokensMetricDelta    `json:"cache_write,omitempty"`
	Output               *checkpointTokensMetricDelta    `json:"output,omitempty"`
	APICalls             *checkpointTokensMetricDelta    `json:"api_calls,omitempty"`
	CostShare            *checkpointTokensCostShareDelta `json:"cost_share,omitempty"`
	CacheReadCaveat      string                          `json:"cache_read_caveat,omitempty"`
	Qualification        string                          `json:"qualification"`
	Limitations          []string                        `json:"limitations,omitempty"`
}

// checkpointTokensMetricDelta is one token class's change between baseline
// and current.
type checkpointTokensMetricDelta struct {
	Baseline      int      `json:"baseline"`
	Current       int      `json:"current"`
	Change        int      `json:"change"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	Direction     string   `json:"direction"`
}

// checkpointTokensCostShareDelta holds the cost-share change of each class.
type checkpointTokensCostShareDelta struct {
	Input      *checkpointTokensShareDelta `json:"input"`
	CacheWrite *checkpointTokensShareDelta `json:"cache_write"`
	CacheRead  *checkpointTokensShareDelta `json:"cache_read"`
	Output     *checkpointTokensShareDelta `json:"output"`
}

// checkpointTokensShareDelta is one class's cost share (0..1) in the baseline
// and current checkpoint, the change in share, the relative change and its
// direction (decided on whole share points).
type checkpointTokensShareDelta struct {
	Baseline      float64  `json:"baseline"`
	Current       float64  `json:"current"`
	Change        float64  `json:"change"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	Direction     string   `json:"direction"`
}

// Comparison statuses and delta directions.
const (
	checkpointComparisonStatusUnavailable       = "unavailable"
	checkpointComparisonStatusObservedReduction = "observed_reduction"
	checkpointComparisonStatusObservedIncrease  = "observed_increase"
	checkpointComparisonStatusObservedNoChange  = "observed_no_change"

	checkpointDeltaDirectionDown      = "down"
	checkpointDeltaDirectionUp        = "up"
	checkpointDeltaDirectionUnchanged = "unchanged"
)

// checkpointCostMixMinPoints is the cost-share move, in whole points, from
// which a class is named in the comparison's cost-mix sentence.
const checkpointCostMixMinPoints = 5

func newCheckpointTokensCmd() *cobra.Command {
	var jsonFlag bool
	var compareFlag string
	var agentBriefFlag bool

	cmd := &cobra.Command{
		Use:   "tokens <checkpoint-id>",
		Short: "Show where a checkpoint's tokens went, with cost shares and recommendations",
		Long: `Show where a checkpoint's tokens went, with cost shares and recommendations.

The report recomputes each session's usage from its stored transcript, sliced
at the checkpoint's token window, and attributes it to the tools, skills and
subagents that caused it; cost shares use the provider's list-price ratios.
Checkpoints written before token scoping (v0.11) fall back to their committed
token usage and are labelled. The checkpoint is resolved the same way as
'entire checkpoint explain': IDs may be abbreviated as long as the prefix is
unambiguous, positional targets may also resolve from a commit ref with an
Entire-Checkpoint trailer, and missing metadata may be fetched from the
checkpoint remote.

Use --compare <checkpoint-id> to compare this checkpoint against a previous
checkpoint and qualify the observed token and cost-share change.`,
		Example: "  entire checkpoint tokens a1b2\n  entire checkpoint tokens a1b2 --compare c3d4\n  entire checkpoint tokens a1b2 --json",
		Args:    cobra.ExactArgs(1),
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
		report.Comparison = buildCheckpointTokensComparison(&report, &baselineReport)
	}

	if jsonOutput {
		return printJSON(cmd.OutOrStdout(), report)
	}
	if agentBrief {
		writeTokenAgentBrief(cmd.OutOrStdout(), "Checkpoint token brief", "Checkpoint", report.CheckpointID, &report.view)
		return nil
	}
	writeCheckpointTokensText(cmd.OutOrStdout(), &report)
	return nil
}

// saturatingIntAdd returns a+b pinned at math.MaxInt / math.MinInt. It mirrors
// the unexported types.saturatingAdd; kept for totalTokens and
// topLevelSessionTokenTotal until the two are consolidated.
func saturatingIntAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	if b < 0 && a < math.MinInt-b {
		return math.MinInt
	}
	return a + b
}

// buildCheckpointTokensComparison compares target against baseline: token
// deltas per class, the cost-share deltas when both were priced, and a
// qualification that names how the cost mix moved.
func buildCheckpointTokensComparison(target, baseline *checkpointTokensReport) *checkpointTokensComparison {
	comparison := &checkpointTokensComparison{
		BaselineCheckpointID: baseline.CheckpointID,
		TargetCheckpointID:   target.CheckpointID,
	}
	if target.Tokens == nil || baseline.Tokens == nil {
		comparison.Status = checkpointComparisonStatusUnavailable
		comparison.Qualification = checkpointComparisonQualification(comparison.Status)
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
	if target.Cost != nil && baseline.Cost != nil {
		comparison.CostShare = buildCheckpointCostShareDelta(&baseline.Cost.Shares, &target.Cost.Shares)
		if shift := checkpointCostMixShift(comparison.CostShare); shift != "" {
			comparison.Qualification += " " + shift
		}
	} else {
		comparison.Limitations = append(comparison.Limitations, "Cost shares are not compared: one checkpoint has no priced usage.")
	}
	return comparison
}

// buildCheckpointCostShareDelta computes each class's cost-share delta.
func buildCheckpointCostShareDelta(baseline, current *tokenreport.CostShares) *checkpointTokensCostShareDelta {
	return &checkpointTokensCostShareDelta{
		Input:      buildCheckpointShareDelta(baseline.Input, current.Input),
		CacheWrite: buildCheckpointShareDelta(baseline.CacheWrite, current.CacheWrite),
		CacheRead:  buildCheckpointShareDelta(baseline.CacheRead, current.CacheRead),
		Output:     buildCheckpointShareDelta(baseline.Output, current.Output),
	}
}

// buildCheckpointShareDelta computes one class's cost-share delta. Direction
// is decided on whole share points so a sub-point drift reads as unchanged.
func buildCheckpointShareDelta(baseline, current float64) *checkpointTokensShareDelta {
	delta := &checkpointTokensShareDelta{Baseline: baseline, Current: current, Change: current - baseline}
	delta.Direction = checkpointDeltaDirection(sharePoints(delta.Change))
	if baseline > 0 {
		percent := delta.Change / baseline * 100
		delta.ChangePercent = &percent
	}
	return delta
}

// sharePoints rounds a share difference to whole percentage points.
func sharePoints(change float64) int {
	return int(math.Round(change * 100))
}

// checkpointCostMixShift names the classes whose cost share moved by at
// least checkpointCostMixMinPoints, e.g. "Cost mix: cache write 41% → 30%
// (down 11 points); output 36% → 48% (up 12 points)."; "" when none moved.
func checkpointCostMixShift(d *checkpointTokensCostShareDelta) string {
	classes := []struct {
		name  string
		delta *checkpointTokensShareDelta
	}{
		{"input", d.Input}, {"cache write", d.CacheWrite}, {"cache read", d.CacheRead}, {"output", d.Output},
	}
	var parts []string
	for _, c := range classes {
		points := sharePoints(c.delta.Change)
		if absInt(points) < checkpointCostMixMinPoints {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s (%s %d points)", c.name,
			tokenreport.FormatPercent(c.delta.Baseline), tokenreport.FormatPercent(c.delta.Current),
			c.delta.Direction, absInt(points)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Cost mix: " + strings.Join(parts, "; ") + "."
}

// absInt is |n|.
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
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

// saturatingIntSub returns a-b pinned at math.MaxInt / math.MinInt.
func saturatingIntSub(a, b int) int {
	if b < 0 {
		if b == math.MinInt {
			if a >= 0 {
				return math.MaxInt
			}
			return a - b
		}
		if a > math.MaxInt-(-b) {
			return math.MaxInt
		}
	}
	if b > 0 && a < math.MinInt+b {
		return math.MinInt
	}
	return a - b
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
