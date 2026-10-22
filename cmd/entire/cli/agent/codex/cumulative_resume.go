package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var _ agent.ResumableCumulativeTokenSource = (*CodexAgent)(nil)

// codexCursorVersion changes whenever codexCursor's meaning does.
const codexCursorVersion = 1

// codexCursor holds cumulativeRolloutUsage's running state, so a resumed call
// reads only the lines appended since.
type codexCursor struct {
	agent.TranscriptCursor

	Version int `json:"v"`
	// Lines counts the non-blank lines consumed: a record's position.
	Lines int `json:"lines"`
	// Seen holds every response_id already counted, so a replayed record is not
	// counted again.
	Seen map[string]bool `json:"seen,omitempty"`
	// Covered sums the records written before the last token_count; Tail holds
	// the records after it, keyed by response_id.
	Covered *agent.TokenUsage            `json:"covered,omitempty"`
	Tail    map[string]*agent.TokenUsage `json:"tail,omitempty"`
	// Banked, Last and Calls are SumCumulativeSegments' state over the
	// token_count totals; Last is the most recent total.
	Banked *agent.TokenUsage `json:"banked,omitempty"`
	Last   *agent.TokenUsage `json:"last,omitempty"`
	Calls  int               `json:"calls,omitempty"`
}

// ResumeCumulativeTokenUsage returns what CumulativeTokenUsage returns, reading
// only the lines appended since cursor. Children still come from the inventory.
func (c *CodexAgent) ResumeCumulativeTokenUsage(ctx context.Context, in agent.CumulativeTokenInput, cursor json.RawMessage) (agent.CumulativeObservation, error) {
	var cur codexCursor
	known := len(cursor) > 0 && json.Unmarshal(cursor, &cur) == nil && cur.Version == codexCursorVersion
	restarted := known && !cur.Resumes(in.TranscriptData)
	if !known || restarted {
		cur = codexCursor{Version: codexCursorVersion}
	}
	end := agent.CompleteLinesEnd(in.TranscriptData, cur.Offset)
	for _, raw := range splitJSONL(in.TranscriptData[cur.Offset:end]) {
		cur.add(raw)
		cur.Lines++
	}
	cur.TranscriptCursor = agent.NewTranscriptCursor(in.TranscriptData, end)

	next, err := json.Marshal(cur)
	if err != nil {
		return agent.CumulativeObservation{}, fmt.Errorf("failed to encode token cursor: %w", err)
	}
	return agent.CumulativeObservation{Total: cur.total(), Children: c.cumulativeChildren(ctx, in), Cursor: next, Restarted: restarted}, nil
}

func (cur *codexCursor) add(raw []byte) {
	if !bytes.Contains(raw, []byte(rolloutLineTypeTokenUsageRecord)) && !bytes.Contains(raw, []byte(eventMsgTypeTokenCount)) {
		return
	}
	var line rolloutLine
	if json.Unmarshal(raw, &line) != nil {
		return
	}
	switch line.Type {
	case rolloutLineTypeTokenUsageRecord:
		var record struct {
			ResponseID string        `json:"response_id"`
			Usage      *rolloutUsage `json:"usage"`
		}
		if json.Unmarshal(line.Payload, &record) != nil || record.Usage == nil {
			return
		}
		key := record.ResponseID
		if key == "" {
			key = "line:" + strconv.Itoa(cur.Lines)
		}
		if cur.Seen[key] {
			return // a replayed record keeps its first place and usage
		}
		if cur.Seen == nil {
			cur.Seen = make(map[string]bool)
		}
		cur.Seen[key] = true
		if cur.Tail == nil {
			cur.Tail = make(map[string]*agent.TokenUsage)
		}
		cur.Tail[key] = record.Usage.tokenUsage(1)
	case rolloutLineTypeEventMsg:
		var event struct {
			Type string `json:"type"`
			Info *struct {
				TotalTokenUsage *rolloutUsage `json:"total_token_usage"`
			} `json:"info"`
		}
		if json.Unmarshal(line.Payload, &event) != nil || event.Type != eventMsgTypeTokenCount ||
			event.Info == nil || event.Info.TotalTokenUsage == nil {
			return
		}
		snapshot := event.Info.TotalTokenUsage.tokenUsage(0)
		if cur.Last == nil || *snapshot != *cur.Last {
			cur.Calls++ // a replayed total is not a new call
		}
		if cur.Last != nil && tokenTotal(snapshot) < tokenTotal(cur.Last) {
			// A lower total is a restarted counter: bank the last segment.
			cur.Banked = types.AddTokenUsage(cur.Banked, cur.Last)
		}
		cur.Last = snapshot
		// Every record so far was written before this total.
		cur.Covered = types.AddTokenUsage(cur.Covered, agent.SumTokenUsageByKey(cur.Tail))
		cur.Tail = nil
	}
}

func (cur *codexCursor) total() *agent.TokenUsage {
	segments := types.AddTokenUsage(cur.Banked, cur.Last)
	if segments != nil {
		segments.APICallCount = cur.Calls
	}
	return agent.UsageOrNil(types.AddTokenUsage(agent.MaxTokenUsage(cur.Covered, segments), agent.SumTokenUsageByKey(cur.Tail)))
}

func tokenTotal(u *agent.TokenUsage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}
