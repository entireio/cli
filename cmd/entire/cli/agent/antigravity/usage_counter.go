package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UsageCounterSource = (*AntigravityAgent)(nil)

const plannerResponse = "PLANNER_RESPONSE"

// recentStepWindow is how many recent steps stay open to a re-written copy.
const recentStepWindow = 64

// NewUsageCounter counts the per-call usage on PLANNER_RESPONSE steps in
// transcript_full.jsonl once per step_index, a re-written step's latest copy
// winning. The statusline feed is not used: its total_input_tokens is a
// context-window gauge, not a running total.
//
// ASSUMPTION (unverified): input_tokens excludes cache_read_tokens. Captured
// runs read nothing from cache, so the two could not be told apart.
func (a *AntigravityAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{Steps: agent.KeyedUsage{Policy: agent.KeepLast, Window: recentStepWindow, Remember: true}}
}

type usageCounter struct {
	Steps agent.KeyedUsage `json:"steps"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{{Path: in.TranscriptPath}}
}

func (u *usageCounter) Add(_ string, data []byte, firstLine int) {
	agent.ForEachLine(data, firstLine, func(lineNo int, raw []byte) {
		if !bytes.Contains(raw, []byte(plannerResponse)) {
			return
		}
		var step struct {
			StepIndex       *int   `json:"step_index"`
			Type            string `json:"type"`
			InputTokens     int    `json:"input_tokens"`
			CacheReadTokens int    `json:"cache_read_tokens"`
			OutputTokens    int    `json:"output_tokens"`
		}
		if json.Unmarshal(raw, &step) != nil || step.Type != plannerResponse || step.InputTokens+step.CacheReadTokens+step.OutputTokens == 0 {
			return
		}
		key := "line:" + strconv.Itoa(lineNo)
		if step.StepIndex != nil {
			key = strconv.Itoa(*step.StepIndex)
		}
		u.Steps.Add("", key, agent.TokenUsage{
			InputTokens: step.InputTokens, CacheReadTokens: step.CacheReadTokens, OutputTokens: step.OutputTokens, APICallCount: 1,
		})
	})
}

func (u *usageCounter) Reset(string) { u.Steps.Reset() }

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	return u.Steps.Sum(""), nil
}
