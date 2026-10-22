package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var _ agent.CumulativeTokenSource = (*CodexAgent)(nil)

// rolloutLineTypeTokenUsageRecord is the per-API-call usage line newer Codex
// versions write before the call's event_msg token_count.
const rolloutLineTypeTokenUsageRecord = "token_usage_record" //nolint:gosec // a JSONL line type, not a credential

// rolloutUsage is one Codex usage object: a call's usage on token_usage_record,
// or the running total on token_count. input_tokens includes the cached share
// and, by assumption (unverified: captured runs wrote no cache), the
// cache-written share too; output_tokens includes reasoning.
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

// CumulativeTokenUsage takes the per-field max of two lower bounds: per-call
// token_usage_record lines deduped by response_id (exact, immune to a counter
// restarted on resume), and segment-summed token_count running totals, which
// also cover calls made before Codex began writing records. The max spans only
// the records written before the last token_count; a record after it (Codex
// writes a call's record before that call's token_count) is added on top, so a
// total that predates records never hides a newer call.
//
// Children come from the session's authoritative inventory, keyed by child
// agent ID. They keep the inventory's own rule — the child's last token_count
// snapshot, without the cache-write split — because a forked child copies its
// parent's history and only that analysis knows which lines are its own.
func (c *CodexAgent) CumulativeTokenUsage(ctx context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	return cumulativeRolloutUsage(in.TranscriptData), c.cumulativeChildren(ctx, in), nil
}

// cumulativeChildren returns each inventoried child's running total.
func (c *CodexAgent) cumulativeChildren(ctx context.Context, in agent.CumulativeTokenInput) map[string]*agent.TokenUsage {
	if len(in.Subagents) == 0 {
		return nil
	}
	extraction, ok := agent.ExtractWithSubagentInventory(ctx, c, in.TranscriptData, 0, in.Subagents, in.Home)
	if !ok {
		return nil
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
	return children
}

func cumulativeRolloutUsage(data []byte) *agent.TokenUsage {
	type callUsage struct {
		usage *agent.TokenUsage
		index int // first occurrence, so a replayed record keeps its place
	}
	records := make(map[string]callUsage)
	var snapshots []*agent.TokenUsage
	snapshotCalls := 0
	lastSnapshot := -1
	for index, raw := range splitJSONL(data) {
		if !bytes.Contains(raw, []byte(rolloutLineTypeTokenUsageRecord)) && !bytes.Contains(raw, []byte(eventMsgTypeTokenCount)) {
			continue
		}
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
			if _, replayed := records[key]; replayed {
				continue // a replayed record keeps its first place and usage
			}
			records[key] = callUsage{usage: record.Usage.tokenUsage(1), index: index}
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
			lastSnapshot = index
		}
	}
	segments := agent.SumCumulativeSegments(snapshots)
	if segments != nil {
		segments.APICallCount = snapshotCalls
	}
	covered := make(map[string]*agent.TokenUsage)
	tail := make(map[string]*agent.TokenUsage)
	for key, r := range records {
		if r.index < lastSnapshot {
			covered[key] = r.usage
		} else {
			tail[key] = r.usage
		}
	}
	return agent.UsageOrNil(types.AddTokenUsage(agent.MaxTokenUsage(agent.SumTokenUsageByKey(covered), segments), agent.SumTokenUsageByKey(tail)))
}
