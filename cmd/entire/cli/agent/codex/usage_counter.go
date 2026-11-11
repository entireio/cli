package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UsageCounterSource = (*CodexAgent)(nil)

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

func (u rolloutUsage) tokenUsage(calls int) agent.TokenUsage {
	return agent.TokenUsage{
		InputTokens:         agent.NonCacheInput(u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens),
		CacheReadTokens:     u.CachedInputTokens,
		CacheCreationTokens: u.CacheWriteInputTokens,
		OutputTokens:        u.OutputTokens,
		APICallCount:        calls,
	}
}

// NewUsageCounter combines two lower bounds on the rollout's usage: per-call
// token_usage_record lines, counted once per response_id (a replayed record
// keeps its first copy; immune to a counter restarted on resume), and
// token_count running totals summed per segment, which also cover calls made
// before Codex began writing records (see KeyedUsage.CoveredBy).
//
// Children come from the session's authoritative inventory, keyed by child
// agent ID, with the inventory's own rule (the child's last token_count
// snapshot): a forked child copies its parent's history, and only that
// analysis knows which lines are its own.
func (c *CodexAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{agent: c, Records: agent.KeyedUsage{Policy: agent.KeepFirst, Remember: true}}
}

type usageCounter struct {
	agent *CodexAgent

	Records   agent.KeyedUsage   `json:"records"`
	Snapshots agent.RunningTotal `json:"totals"`
	// Calls counts distinct consecutive token_count totals: a replayed total
	// is not a new call.
	Calls int `json:"calls,omitempty"`
	// ChildKey fingerprints the inventoried children's files when ChildTotals
	// was read: an unchanged inventory reuses it instead of reading every
	// child rollout again.
	ChildKey    string                       `json:"child_key,omitempty"`
	ChildTotals map[string]*agent.TokenUsage `json:"child_totals,omitempty"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{{Path: in.TranscriptPath}}
}

func (u *usageCounter) Add(_ string, data []byte) {
	agent.ForEachLine(data, func(raw []byte) {
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
			u.Records.Add("", record.ResponseID, record.Usage.tokenUsage(1))
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
			if last := u.Snapshots.Last; last == nil || snapshot != *last {
				u.Calls++
			}
			u.Snapshots.Add(&snapshot)
			u.Records.Mark()
		}
	})
}

func (u *usageCounter) Reset(string) {
	u.Records.Reset()
	u.Snapshots, u.Calls = agent.RunningTotal{}, 0
}

func (u *usageCounter) Totals(ctx context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	segments := u.Snapshots.Value()
	if segments != nil {
		segments.APICallCount = u.Calls
	}
	return agent.UsageOrNil(u.Records.CoveredBy(segments)), u.children(ctx, in)
}

// children returns each inventoried child's running total.
func (u *usageCounter) children(ctx context.Context, in agent.CumulativeTokenInput) map[string]*agent.TokenUsage {
	if len(in.Subagents) == 0 {
		return nil
	}
	key, cacheable := childFingerprint(in)
	if cacheable && key == u.ChildKey {
		return maps.Clone(u.ChildTotals)
	}
	extraction, ok := agent.ExtractWithSubagentInventory(ctx, u.agent, nil, 0, in.Subagents, in.Home)
	if !ok {
		return nil
	}
	var children map[string]*agent.TokenUsage
	for index, child := range extraction.Children {
		if child.AgentID != "" && (child.TokenUsage == nil || !readFromOwnPath(in.Subagents, index, child.ResolvedPath)) {
			// A child read without a total (a failed or canceled read) or
			// found by scanning is read again next time, not cached.
			cacheable = false
		}
		if child.AgentID == "" || child.TokenUsage == nil {
			continue
		}
		if children == nil {
			children = make(map[string]*agent.TokenUsage)
		}
		children[child.AgentID] = child.TokenUsage
	}
	if cacheable && ctx.Err() == nil {
		u.ChildKey, u.ChildTotals = key, maps.Clone(children)
	} else {
		u.ChildKey, u.ChildTotals = "", nil
	}
	return children
}

// readFromOwnPath reports whether the child at index was read from one of
// its reference's own paths, which childFingerprint covers.
func readFromOwnPath(refs []agent.SubagentReference, index int, path string) bool {
	if index >= len(refs) || path == "" {
		return false
	}
	return path == refs[index].DeclaredTranscriptPath || path == refs[index].ResolvedTranscriptPath
}

// childFingerprint identifies the inventory and, for each child, every path
// its rollout is read from (declared, then resolved, as loadDirectRollout
// tries them): a regular file's size and modification time, or that it is
// missing, so one appearing changes the key. A child with neither path a
// regular file is found by scanning, and a path that is something else is
// left to the inventory's own checks, so neither inventory is cached.
func childFingerprint(in agent.CumulativeTokenInput) (string, bool) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", in.Home)
	for _, ref := range in.Subagents {
		fmt.Fprintf(h, "%s", ref.AgentID)
		found := false
		for _, path := range []string{ref.DeclaredTranscriptPath, ref.ResolvedTranscriptPath} {
			regular, ok := fingerprintPath(h, path)
			if !ok {
				return "", false
			}
			found = found || regular
		}
		if !found {
			return "", false
		}
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), true
}

// fingerprintPath writes path's part of a child fingerprint to w: its size
// and modification time when it is a regular file (regular), a marker when
// it is empty or missing. ok is false for anything else.
func fingerprintPath(w io.Writer, path string) (regular, ok bool) {
	if path == "" {
		fmt.Fprint(w, "\x00")
		return false, true
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		fmt.Fprintf(w, "\x00%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())
		return true, true
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(w, "\x00%s\x00missing", path)
		return false, true
	default:
		return false, false
	}
}
