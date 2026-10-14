package codex

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.CumulativeTokenSource = (*CodexAgent)(nil)

// rolloutLineTypeTokenUsageRecord is the per-API-call usage line newer Codex
// versions write before the call's event_msg token_count.
const rolloutLineTypeTokenUsageRecord = "token_usage_record" //nolint:gosec // a JSONL line type, not a credential

// rolloutUsage is one Codex usage object: a call's usage on token_usage_record,
// or the running total on token_count. input_tokens includes the cached and
// cache-written shares; output_tokens includes reasoning.
type rolloutUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
}

func (u rolloutUsage) tokenUsage(calls int) *agent.TokenUsage {
	return &agent.TokenUsage{
		InputTokens:         agent.NonCacheInput(u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens),
		CacheReadTokens:     u.CachedInputTokens,
		CacheCreationTokens: u.CacheWriteInputTokens,
		OutputTokens:        u.OutputTokens,
		APICallCount:        calls,
	}
}

// CumulativeTokenUsage prefers per-call token_usage_record lines, deduped by
// response_id: exact per call, they are immune to a counter restarted on
// resume or a replayed total. Older rollouts without them fall back to
// segment-summed token_count running totals. Children come from the session's
// authoritative inventory, keyed by child agent ID.
func (c *CodexAgent) CumulativeTokenUsage(ctx context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	total := cumulativeRolloutUsage(in.TranscriptData)
	if len(in.Subagents) == 0 {
		return total, nil, nil
	}
	extraction, ok := agent.ExtractWithSubagentInventory(ctx, c, in.TranscriptData, 0, in.Subagents, in.Home)
	if !ok {
		return total, nil, nil
	}
	var children map[string]*agent.TokenUsage
	for _, child := range extraction.Children {
		if child.AgentID == "" || child.TokenUsage == nil {
			continue
		}
		if children == nil {
			children = make(map[string]*agent.TokenUsage)
		}
		children[child.AgentID] = child.TokenUsage
	}
	return total, children, nil
}

func cumulativeRolloutUsage(data []byte) *agent.TokenUsage {
	records := make(map[string]*agent.TokenUsage)
	var snapshots []*agent.TokenUsage
	snapshotCalls := 0
	for index, raw := range splitJSONL(data) {
		var line rolloutLine
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		switch line.Type {
		case rolloutLineTypeTokenUsageRecord:
			var record struct {
				ResponseID string        `json:"response_id"`
				Usage      *rolloutUsage `json:"usage"`
			}
			if json.Unmarshal(line.Payload, &record) != nil || record.Usage == nil {
				continue
			}
			key := record.ResponseID
			if key == "" {
				key = "line:" + strconv.Itoa(index)
			}
			records[key] = record.Usage.tokenUsage(1)
		case rolloutLineTypeEventMsg:
			var event struct {
				Type string `json:"type"`
				Info *struct {
					TotalTokenUsage *rolloutUsage `json:"total_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(line.Payload, &event) != nil || event.Type != eventMsgTypeTokenCount ||
				event.Info == nil || event.Info.TotalTokenUsage == nil {
				continue
			}
			snapshot := event.Info.TotalTokenUsage.tokenUsage(0)
			if len(snapshots) == 0 || *snapshot != *snapshots[len(snapshots)-1] {
				snapshotCalls++ // a replayed total is not a new call
			}
			snapshots = append(snapshots, snapshot)
		}
	}
	if len(records) > 0 {
		return agent.SumTokenUsageByKey(records)
	}
	total := agent.SumCumulativeSegments(snapshots)
	if total != nil {
		total.APICallCount = snapshotCalls
	}
	return total
}
