package opencode

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.CumulativeTokenSource = (*OpenCodeAgent)(nil)

// CumulativeTokenUsage sums every assistant message's tokens in the session
// export. OpenCode reports reasoning separately from output, so OutputTokens
// is output + reasoning. A message still in flight carries all-zero tokens
// (OpenCode fills them at step finish, after the step's tools run); it counts
// as a call with no tokens until a later read sees the final values. Child
// sessions are top-level sessions of their own, so there are no children.
func (a *OpenCodeAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	session, err := ParseExportSession(in.TranscriptData)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}
	if session == nil {
		return nil, nil, nil
	}
	var total *agent.TokenUsage
	for _, msg := range session.Messages {
		tokens := msg.Info.Tokens
		if msg.Info.Role != roleAssistant || tokens == nil {
			continue
		}
		if total == nil {
			total = &agent.TokenUsage{}
		}
		total.InputTokens += tokens.Input
		total.OutputTokens += tokens.Output + tokens.Reasoning
		total.CacheReadTokens += tokens.Cache.Read
		total.CacheCreationTokens += tokens.Cache.Write
		total.APICallCount++
	}
	return total, nil, nil
}
