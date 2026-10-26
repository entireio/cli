package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// MergePolicy says which copy of a repeated per-call record holds its usage.
type MergePolicy uint8

const (
	// KeepFirst keeps the first copy: later copies are replays (Codex).
	KeepFirst MergePolicy = iota
	// KeepLast keeps the latest copy: a record re-written in place (Copilot,
	// Antigravity).
	KeepLast
	// KeepMostOutput keeps the copy with the most output, a streamed
	// message's final state (Claude Code, Factory Droid).
	KeepMostOutput
)

// KeyedUsage sums per-call records once per (child, key); the main agent's
// child is "". A record stays open to later copies while among its child's
// latest Window, then is closed into a sum; with Remember its key is kept
// (hashed) so a later replay is skipped. Policy, Window and Remember are set
// by the counter's constructor; the rest is saved state.
type KeyedUsage struct {
	Policy   MergePolicy `json:"-"`
	Window   int         `json:"-"`
	Remember bool        `json:"-"`

	Closed map[string]*TokenUsage     `json:"closed,omitempty"`
	Open   []keyedRecord              `json:"open,omitempty"`
	Seen   map[string]map[string]bool `json:"seen,omitempty"`
	Marked *TokenUsage                `json:"marked,omitempty"`
}

type keyedRecord struct {
	Child string     `json:"c,omitempty"`
	Key   string     `json:"k"`
	Usage TokenUsage `json:"u"`
}

// Add records one copy of the call key. A record without a key cannot be
// repeated, so it is counted at once.
func (k *KeyedUsage) Add(child, key string, usage TokenUsage) {
	if key == "" {
		k.addClosed(child, usage)
		return
	}
	for i := len(k.Open) - 1; i >= 0; i-- {
		if r := &k.Open[i]; r.Child == child && r.Key == key {
			if k.Policy == KeepLast || (k.Policy == KeepMostOutput && usage.OutputTokens > r.Usage.OutputTokens) {
				r.Usage = usage
			}
			return
		}
	}
	if k.Seen[child][keyHash(key)] {
		return
	}
	k.Open = append(k.Open, keyedRecord{Child: child, Key: key, Usage: usage})
	// The window is per child: files are read one after another, so another
	// file's records must not close a message whose next line is still to come.
	open, oldest := 0, -1
	for i, r := range k.Open {
		if r.Child == child {
			if oldest < 0 {
				oldest = i
			}
			open++
		}
	}
	if open > k.Window {
		k.close(k.Open[oldest])
		k.Open = append(k.Open[:oldest], k.Open[oldest+1:]...)
	}
}

func (k *KeyedUsage) addClosed(child string, usage TokenUsage) {
	if k.Closed == nil {
		k.Closed = make(map[string]*TokenUsage)
	}
	k.Closed[child] = types.AddTokenUsage(k.Closed[child], &usage)
}

func (k *KeyedUsage) close(r keyedRecord) {
	k.addClosed(r.Child, r.Usage)
	if !k.Remember {
		return
	}
	if k.Seen == nil {
		k.Seen = make(map[string]map[string]bool)
	}
	if k.Seen[r.Child] == nil {
		k.Seen[r.Child] = make(map[string]bool)
	}
	k.Seen[r.Child][keyHash(r.Key)] = true
}

// Sum returns child's total, nil when it has no records.
func (k *KeyedUsage) Sum(child string) *TokenUsage {
	total := cloneUsage(k.Closed[child])
	for _, r := range k.Open {
		if r.Child == child {
			total = types.AddTokenUsage(total, &r.Usage)
		}
	}
	return total
}

// Children returns every child's total except the main agent's.
func (k *KeyedUsage) Children() map[string]*TokenUsage {
	var children map[string]*TokenUsage
	add := func(child string) {
		if child == "" || children[child] != nil {
			return
		}
		if children == nil {
			children = make(map[string]*TokenUsage)
		}
		children[child] = k.Sum(child)
	}
	for child := range k.Closed {
		add(child)
	}
	for _, r := range k.Open {
		add(r.Child)
	}
	return children
}

// All returns the total over every child, the main agent included.
func (k *KeyedUsage) All() *TokenUsage {
	var total *TokenUsage
	for _, u := range k.Closed {
		total = types.AddTokenUsage(total, u)
	}
	for _, r := range k.Open {
		total = types.AddTokenUsage(total, &r.Usage)
	}
	return total
}

// Mark records that a running total now covers every record so far.
func (k *KeyedUsage) Mark() { k.Marked = k.All() }

// CoveredBy combines the records with a running total of the same calls: the
// per-field max of the two over what the last Mark covered (each is a lower
// bound there: the total also saw calls made before records were written,
// the records survive a counter restart), plus the records since, which no
// total covers yet.
func (k *KeyedUsage) CoveredBy(total *TokenUsage) *TokenUsage {
	return types.AddTokenUsage(MaxTokenUsage(k.Marked, total), types.SubtractTokenUsage(k.All(), k.Marked))
}

// CloseChild closes child's open records: its file was read whole, so no
// later line can extend them.
func (k *KeyedUsage) CloseChild(child string) {
	open := k.Open[:0]
	for _, r := range k.Open {
		if r.Child == child {
			k.addClosed(child, r.Usage)
		} else {
			open = append(open, r)
		}
	}
	k.Open = open
}

// ResetChild forgets child's records, for a file read again from its start.
func (k *KeyedUsage) ResetChild(child string) {
	delete(k.Closed, child)
	delete(k.Seen, child)
	open := k.Open[:0]
	for _, r := range k.Open {
		if r.Child != child {
			open = append(open, r)
		}
	}
	k.Open = open
}

// Reset forgets every record.
func (k *KeyedUsage) Reset() {
	k.Closed, k.Open, k.Seen, k.Marked = nil, nil, nil, nil
}

// keyHash shortens a record key for the seen set.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// RunningTotal sums running-total snapshots from a source that restarts its
// counter (a resumed process). A snapshot below its predecessor starts a new
// segment and the previous segment's last snapshot is banked; an equal or
// larger one continues the segment, so a replayed total is not counted twice.
//
// LIMITATION: a restart is visible only when the new segment's first snapshot
// is below the old segment's last. Where an agent also writes per-call
// records, combine the two with KeyedUsage.CoveredBy.
type RunningTotal struct {
	Banked *TokenUsage `json:"banked,omitempty"`
	Last   *TokenUsage `json:"last,omitempty"`
}

// Add records the next snapshot.
func (r *RunningTotal) Add(snapshot *TokenUsage) {
	if snapshot == nil {
		return
	}
	if r.Last != nil && TokenTotal(snapshot) < TokenTotal(r.Last) {
		r.Banked = types.AddTokenUsage(r.Banked, r.Last)
	}
	r.Last = snapshot
}

// Value returns the total over every segment, nil before the first snapshot.
func (r *RunningTotal) Value() *TokenUsage {
	return types.AddTokenUsage(r.Banked, r.Last)
}

// MaxTokenUsage returns the per-field maximum of a and b, ignoring subagent
// usage: two lower bounds on the same running total, so usage only one of
// them saw is kept. nil when both are.
func MaxTokenUsage(a, b *TokenUsage) *TokenUsage {
	if a == nil || b == nil {
		return types.AddTokenUsage(a, b)
	}
	return &TokenUsage{
		InputTokens:         max(a.InputTokens, b.InputTokens),
		CacheCreationTokens: max(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:     max(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:        max(a.OutputTokens, b.OutputTokens),
		APICallCount:        max(a.APICallCount, b.APICallCount),
	}
}

// UsageOrNil maps usage with no tokens and no calls to nil, "no data". Agents
// write all-zero placeholders (Droid's settings.json before its first update,
// an idle Copilot shutdown).
func UsageOrNil(u *TokenUsage) *TokenUsage {
	if u == nil || (TokenTotal(u) == 0 && u.APICallCount == 0 && u.SubagentTokens == nil) {
		return nil
	}
	return u
}

// TokenTotal returns u's token count over every field, 0 for nil.
func TokenTotal(u *TokenUsage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

func cloneUsage(u *TokenUsage) *TokenUsage {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

// NonCacheInput returns the fresh-input share of a cache-inclusive input count,
// floored at zero, for agents that report input with cache reads and writes
// folded in.
func NonCacheInput(inclusiveInput, cacheRead, cacheWrite int) int {
	return max(inclusiveInput-cacheRead-cacheWrite, 0)
}
