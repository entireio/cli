package opencode

import (
	"context"
	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UsageCounterSource = (*OpenCodeAgent)(nil)

// NewUsageCounter sums every assistant message's tokens in the session
// export, which OpenCode rewrites rather than appends, so it is read whole.
// OutputTokens includes reasoning once (see OutputWithReasoning). A message
// still in flight carries all-zero tokens (OpenCode fills them at step
// finish, after the step's tools run); it counts as a call with no tokens
// until a later read sees the final values. Child sessions are top-level
// sessions of their own, so there are no children.
func (a *OpenCodeAgent) NewUsageCounter() agent.UsageCounter { return &usageCounter{} }

type usageCounter struct {
	Total *agent.TokenUsage `json:"total,omitempty"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{{Path: in.TranscriptPath, Whole: true}}
}

func (u *usageCounter) Add(_ string, data []byte) {
	session, err := ParseExportSession(data)
	if err != nil || session == nil {
		return
	}
	for _, msg := range session.Messages {
		tokens := msg.Info.Tokens
		if msg.Info.Role != roleAssistant || tokens == nil {
			continue
		}
		if u.Total == nil {
			u.Total = &agent.TokenUsage{}
		}
		u.Total.InputTokens += tokens.Input
		u.Total.OutputTokens += tokens.OutputWithReasoning()
		u.Total.CacheReadTokens += tokens.Cache.Read
		u.Total.CacheCreationTokens += tokens.Cache.Write
		u.Total.APICallCount++
	}
}

func (u *usageCounter) Reset(string) { u.Total = nil }

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	return u.Total, nil
}
