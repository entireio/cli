package antigravity

import (
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.TokenCalculator = (*AntigravityAgent)(nil)

// agyStepTypePlannerResponse is the transcript step agy writes for each model
// call.
const agyStepTypePlannerResponse = "PLANNER_RESPONSE"

// agyTokenStep is the token-bearing subset of a transcript step. In current
// agy (verified on 1.4.2), every PLANNER_RESPONSE step carries the usage of
// the model call that produced it. The fields are pointers because their presence is what matters:
// older agy writes steps without them, and those transcripts must report "no
// data" so the statusline fallback runs instead of a zero count.
//
// output_tokens includes reasoning: agy's print-mode usage reports
// thinking_tokens as a part of output_tokens, and the step carries the same
// output total. input_tokens is mapped to InputTokens as is. Whether it already
// includes cache_read_tokens is unconfirmed, because agy reported no cache
// reads in any session observed so far.
type agyTokenStep struct {
	StepIndex       int    `json:"step_index"`
	Type            string `json:"type"`
	InputTokens     *int   `json:"input_tokens"`
	OutputTokens    *int   `json:"output_tokens"`
	CacheReadTokens *int   `json:"cache_read_tokens"`
}

func (s *agyTokenStep) hasTokens() bool {
	return s.InputTokens != nil || s.OutputTokens != nil || s.CacheReadTokens != nil
}

// CalculateTokenUsage implements agent.TokenCalculator. It sums the per-call
// usage on the PLANNER_RESPONSE steps after fromOffset, a count of non-blank
// lines like every other offset in this package, with one API call per step.
//
// It returns nil when no step in range carries token fields: older agy
// writes none, and nil lets the lifecycle fall back to the statusline source
// (see statusline.go) and condensation to the pending checkpoint usage.
// A parent conversation's steps exclude its subagents' calls; each subagent
// conversation has its own transcript and session.
func (a *AntigravityAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	usage := &agent.TokenUsage{}
	seen := make(map[int]struct{})
	forEachNonBlankLine(transcriptData, fromOffset, func(raw []byte) {
		var step agyTokenStep
		if json.Unmarshal(raw, &step) != nil || step.Type != agyStepTypePlannerResponse || !step.hasTokens() {
			return
		}
		if _, dup := seen[step.StepIndex]; dup {
			return
		}
		seen[step.StepIndex] = struct{}{}
		usage.InputTokens += derefInt(step.InputTokens)
		usage.OutputTokens += derefInt(step.OutputTokens)
		usage.CacheReadTokens += derefInt(step.CacheReadTokens)
		usage.APICallCount++
	})
	if usage.APICallCount == 0 {
		return nil, nil //nolint:nilnil // no token data in range; callers fall back
	}
	return usage, nil
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
