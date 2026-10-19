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
// The main agent's session.shutdown aggregates (one per process, cumulative
// across resumes, summed per segment) are a second lower bound on everything
// before the last shutdown; they also cover calls made before Copilot wrote
// usage records. The total is the per-field max of the two over that span,
// plus the records written after the last shutdown, which no aggregate covers
// yet. Whatever exceeds the children's records is attributed to the main
// agent, since the aggregates cannot be split.
func (c *CopilotCLIAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	events, err := parseEventsFromBytes(in.TranscriptData)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	type record struct {
		usage *agent.TokenUsage
		index int // first occurrence, so a replayed record keeps its place
	}
	calls := make(map[string]map[string]record) // agent ID ("" = main) -> call ID -> usage
	var shutdowns []*agent.TokenUsage
	lastShutdown := -1
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
				calls[event.AgentID] = make(map[string]record)
			}
			at := index
			if prev, ok := calls[event.AgentID][key]; ok {
				at = prev.index
			}
			calls[event.AgentID][key] = record{usage: data.Usage.tokenUsage(1), index: at}
		case eventTypeSessionShutdown:
			var data sessionShutdownData
			if event.AgentID != "" || json.Unmarshal(event.Data, &data) != nil || len(data.ModelMetrics) == 0 {
				continue
			}
			var usage *agent.TokenUsage
			for _, m := range data.ModelMetrics {
				usage = types.AddTokenUsage(usage, m.Usage.tokenUsage(m.Requests.Count))
			}
			if usage = agent.UsageOrNil(usage); usage != nil {
				shutdowns = append(shutdowns, usage)
				lastShutdown = index
			}
		}
	}
	var children map[string]*agent.TokenUsage
	var covered, tail, childTotal *agent.TokenUsage
	for agentID, byCall := range calls {
		var sum *agent.TokenUsage
		for _, r := range byCall {
			sum = types.AddTokenUsage(sum, r.usage)
			if r.index < lastShutdown {
				covered = types.AddTokenUsage(covered, r.usage)
			} else {
				tail = types.AddTokenUsage(tail, r.usage)
			}
		}
		if agentID != "" {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[agentID] = sum
			childTotal = types.AddTokenUsage(childTotal, sum)
		}
	}
	session := types.AddTokenUsage(agent.MaxTokenUsage(covered, agent.SumCumulativeSegments(shutdowns)), tail)
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
