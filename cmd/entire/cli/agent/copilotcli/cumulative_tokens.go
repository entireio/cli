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
// The session.shutdown aggregates (one per process, cumulative across
// resumes, summed per segment) are a second lower bound that also covers
// calls made before Copilot wrote usage records. The session-wide total is
// the per-field max of the two; whatever exceeds the children's records is
// attributed to the main agent, since the aggregates cannot be split.
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
			if usage = agent.UsageOrNil(usage); usage != nil {
				shutdowns = append(shutdowns, usage)
			}
		}
	}
	var children map[string]*agent.TokenUsage
	var recorded, childTotal *agent.TokenUsage
	for agentID, byCall := range calls {
		sum := agent.SumTokenUsageByKey(byCall)
		recorded = types.AddTokenUsage(recorded, sum)
		if agentID != "" {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[agentID] = sum
			childTotal = types.AddTokenUsage(childTotal, sum)
		}
	}
	session := agent.MaxTokenUsage(recorded, agent.SumCumulativeSegments(shutdowns))
	return types.SubtractTokenUsage(session, childTotal), children, nil
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
