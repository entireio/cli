package antigravity

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.CumulativeTokenSource = (*AntigravityAgent)(nil)

// CumulativeTokenUsage sums the per-call usage on PLANNER_RESPONSE steps in
// transcript_full.jsonl, keyed by step_index so a re-written step counts once.
// The statusline feed is not used: its total_input_tokens is a context-window
// gauge, not a running total.
//
// ASSUMPTION (unverified): input_tokens excludes cache_read_tokens. Captured
// runs read nothing from cache, so the two could not be told apart.
func (a *AntigravityAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	byStep := make(map[string]*agent.TokenUsage)
	forEachNonBlankLine(in.TranscriptData, 0, func(raw []byte) {
		var step struct {
			StepIndex       int    `json:"step_index"`
			Type            string `json:"type"`
			InputTokens     int    `json:"input_tokens"`
			CacheReadTokens int    `json:"cache_read_tokens"`
			OutputTokens    int    `json:"output_tokens"`
		}
		if json.Unmarshal(raw, &step) != nil || step.Type != "PLANNER_RESPONSE" || step.InputTokens+step.OutputTokens == 0 {
			return
		}
		byStep[strconv.Itoa(step.StepIndex)] = &agent.TokenUsage{
			InputTokens: step.InputTokens, CacheReadTokens: step.CacheReadTokens, OutputTokens: step.OutputTokens, APICallCount: 1,
		}
	})
	return agent.SumTokenUsageByKey(byStep), nil, nil
}
