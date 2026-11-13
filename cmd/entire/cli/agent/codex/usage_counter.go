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
	"slices"

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
	key, preferred, cacheable := childFingerprint(in, u.childRolloutRoots(in.Home))
	if cacheable && key == u.ChildKey {
		return maps.Clone(u.ChildTotals)
	}
	extraction, ok := agent.ExtractWithSubagentInventory(ctx, u.agent, nil, 0, in.Subagents, in.Home)
	if !ok {
		return nil
	}
	var children map[string]*agent.TokenUsage
	for index, child := range extraction.Children {
		if child.AgentID != "" && !readAsFingerprinted(child, preferred, index) {
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

// readAsFingerprinted reports whether the child at index came back with a
// total read from preferred[index], the path childFingerprint expects it to
// be read from. A failed or canceled read, a child found by scanning, and a
// read that fell back past a preferred file it couldn't read (a transient
// failure leaves the file's size and time alone) are read again next time,
// not cached.
func readAsFingerprinted(child agent.SubagentAnalysis, preferred []string, index int) bool {
	return child.TokenUsage != nil && index < len(preferred) && child.ResolvedPath == preferred[index]
}

// childRolloutRoots are the trusted roots a child's rollout can be read
// from: the agent's own, and the stores beneath the session's recorded home
// when that home is trusted. A child's paths are looked at only inside them.
func (u *usageCounter) childRolloutRoots(home string) []string {
	roots := slices.Clone(u.agent.rolloutRoots())
	if home == "" {
		return roots
	}
	trusted, err := agent.ResolveTrustedHome(u.agent, home)
	if err != nil {
		return roots
	}
	return append(roots, codexHomeLayout().StoresUnder(trusted)...)
}

// childFingerprint identifies the inventory and, for each child, every path
// its rollout is read from (declared, then resolved, as loadDirectRollout
// tries them): a regular file's size and modification time, or that it is
// missing, so one appearing changes the key. preferred holds each child's
// first path that is a regular file, the one its cached total must come
// from. A child with neither path a regular file is found by scanning, and a
// path that is something else, or outside roots, is left to the inventory's
// own checks, so neither inventory is cached.
func childFingerprint(in agent.CumulativeTokenInput, roots []string) (key string, preferred []string, ok bool) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", in.Home)
	preferred = make([]string, len(in.Subagents))
	for index, ref := range in.Subagents {
		fmt.Fprintf(h, "%s", ref.AgentID)
		for _, path := range []string{ref.DeclaredTranscriptPath, ref.ResolvedTranscriptPath} {
			regular, pathOK := fingerprintPath(h, roots, path)
			if !pathOK {
				return "", nil, false
			}
			if regular && preferred[index] == "" {
				preferred[index] = path
			}
		}
		if preferred[index] == "" {
			return "", nil, false
		}
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), preferred, true
}

// fingerprintPath writes path's part of a child fingerprint to w: its size
// and modification time when it is a regular file (regular), a marker when
// it is empty or missing. ok is false for anything else, including a path
// outside roots.
func fingerprintPath(w io.Writer, roots []string, path string) (regular, ok bool) {
	if path == "" {
		fmt.Fprint(w, "\x00")
		return false, true
	}
	info, err := lstatInRoots(roots, path)
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

// lstatInRoots is Lstat of path through the first of roots that holds it, as
// openScopedRollout opens one. A path outside every root is not looked at.
func lstatInRoots(roots []string, path string) (fs.FileInfo, error) {
	root, rel, err := openRolloutRoot(roots, path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("lstat rollout: %w", err)
	}
	return info, nil
}
