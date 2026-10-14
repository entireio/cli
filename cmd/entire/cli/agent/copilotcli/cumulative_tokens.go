package copilotcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var _ agent.CumulativeTokenSource = (*CopilotCLIAgent)(nil)

const eventTypeUsageRecord = "session.usage_record"

// CumulativeTokenUsage sums the per-call session.usage_record events, deduped
// by apiCallId. Their inputTokens include the cache-read and cache-write
// shares, which are split out. A record carrying a subagent's agentId (set on
// every event a subagent emits) is reported as that child's usage.
//
// Transcripts without usage records fall back to the session.shutdown
// aggregates: one per process, cumulative across resumes, so they are summed
// per segment. That fallback cannot split out subagents.
func (c *CopilotCLIAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	events, err := parseEventsFromBytes(in.TranscriptData)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	calls := make(map[string]map[string]*agent.TokenUsage) // agent ID ("" = main) -> call ID -> usage
	var shutdowns []*agent.TokenUsage
	for index, event := range events {
		switch event.Type {
		case eventTypeUsageRecord:
			var data struct {
				Usage *struct {
					modelUsage

					APICallID string `json:"apiCallId"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Data, &data) != nil || data.Usage == nil {
				continue
			}
			key := data.Usage.APICallID
			if key == "" {
				key = "event:" + strconv.Itoa(index)
			}
			if calls[event.AgentID] == nil {
				calls[event.AgentID] = make(map[string]*agent.TokenUsage)
			}
			calls[event.AgentID][key] = data.Usage.tokenUsage(1)
		case eventTypeSessionShutdown:
			var data sessionShutdownData
			if json.Unmarshal(event.Data, &data) != nil || len(data.ModelMetrics) == 0 {
				continue
			}
			var usage *agent.TokenUsage
			for _, m := range data.ModelMetrics {
				usage = types.AddTokenUsage(usage, m.Usage.tokenUsage(m.Requests.Count))
			}
			shutdowns = append(shutdowns, usage)
		}
	}
	if len(calls) == 0 {
		return agent.SumCumulativeSegments(shutdowns), nil, nil
	}
	var children map[string]*agent.TokenUsage
	for agentID, byCall := range calls {
		if agentID != "" {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[agentID] = agent.SumTokenUsageByKey(byCall)
		}
	}
	return agent.SumTokenUsageByKey(calls[""]), children, nil
}

// tokenUsage converts Copilot's cache-inclusive usage counts.
func (u modelUsage) tokenUsage(calls int) *agent.TokenUsage {
	return &agent.TokenUsage{
		InputTokens:         agent.NonCacheInput(u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens),
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheWriteTokens,
		OutputTokens:        u.OutputTokens,
		APICallCount:        calls,
	}
}
