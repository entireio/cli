package pijsonl

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResolveActiveBranch_LinearChain(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"session","id":"s1"}
{"type":"model_change","id":"mc1","parentId":null}
{"type":"message","id":"m1","parentId":"mc1"}
{"type":"message","id":"m2","parentId":"m1"}
{"type":"message","id":"m3","parentId":"m2"}
`)
	active := ResolveActiveBranch(data)
	for _, id := range []string{"m3", "m2", "m1", "mc1"} {
		if !active[id] {
			t.Errorf("expected %q in active set", id)
		}
	}
}

func TestResolveActiveBranch_FlatReturnsNil(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"session","id":"s1"}
{"type":"message","id":"m1"}
{"type":"message","id":"m2"}
`)
	if ResolveActiveBranch(data) != nil {
		t.Error("expected nil for flat transcript (no parentId references)")
	}
}

func TestResolveActiveBranch_TwoBranchesPicksLast(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"message","id":"a","parentId":"root"}
{"type":"message","id":"root","parentId":null}
{"type":"message","id":"b","parentId":"a"}
{"type":"message","id":"c","parentId":"a"}
`)
	active := ResolveActiveBranch(data)
	if !active["c"] || !active["a"] {
		t.Errorf("expected c+a in active, got %v", active)
	}
	if active["b"] {
		t.Error("b (abandoned) should not be in active set")
	}
}

func TestResolveActiveBranch_CycleProtection(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"message","id":"a","parentId":"b"}
{"type":"message","id":"b","parentId":"a"}
`)
	active := ResolveActiveBranch(data)
	if !active["a"] || !active["b"] {
		t.Errorf("active = %v, want both a and b (cycle terminates)", active)
	}
}

func TestForEachActiveMessage(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"session","id":"s1"}
{"type":"message","id":"m1","parentId":null,"message":{"role":"user"}}
{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant"}}
`)
	var got []string
	if err := ForEachActiveMessage(data, 0, func(e Entry) {
		got = append(got, e.ID+":"+e.Message.Role)
	}); err != nil {
		t.Fatal(err)
	}
	// The session header (type != "message") is filtered out.
	want := []string{"m1:user", "m2:assistant"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestForEachActiveMessage_SkipsAbandonedBranchAndHonoursOffset(t *testing.T) {
	t.Parallel()
	// Two branches off m1: m2 (abandoned) and m3 (active, last). Offset 1 skips
	// the session header line; active-branch resolution still runs on full data.
	data := []byte(`{"type":"session","id":"s1"}
{"type":"message","id":"m1","parentId":null,"message":{"role":"user"}}
{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant"}}
{"type":"message","id":"m3","parentId":"m1","message":{"role":"assistant"}}
`)
	var got []string
	if err := ForEachActiveMessage(data, 1, func(e Entry) {
		got = append(got, e.ID)
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"m1", "m3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v (m2 abandoned, header skipped by offset)", got, want)
	}
}

func TestForEachActiveMessage_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	called := false
	if err := ForEachActiveMessage(nil, 0, func(Entry) { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("fn should not be called for empty data")
	}
}

func TestSkipLines(t *testing.T) {
	t.Parallel()
	data := []byte("a\nb\nc\nd\n")
	if got := string(SkipLines(data, 0)); got != "a\nb\nc\nd\n" {
		t.Errorf("0: got %q", got)
	}
	if got := string(SkipLines(data, 2)); got != "c\nd\n" {
		t.Errorf("2: got %q", got)
	}
	// At end of fully-terminated data, SkipLines returns the empty tail
	// (not nil). nil is reserved for "data ran out mid-line" — see below.
	if got := SkipLines(data, 4); len(got) != 0 {
		t.Errorf("4 (exhaust): got %q, expected empty tail", got)
	}
	// With unterminated final line, asking for more lines than exist must
	// return nil so callers can detect the underflow.
	unterminated := []byte("a\nb")
	if got := SkipLines(unterminated, 5); got != nil {
		t.Errorf("unterminated past end: expected nil, got %q", got)
	}
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"":         0,
		"a\n":      1,
		"a\nb\n":   2,
		"a\nb":     2, // unterminated final line counted
		"a\n\nb\n": 3, // blank line counted (offset semantics)
		"\n":       1,
	}
	for in, want := range cases {
		if got := CountLines([]byte(in)); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNewScanner_HandlesLargeLines(t *testing.T) {
	t.Parallel()
	// A 5MB JSONL line — well over the legacy 1MB cap, well under the new
	// 10MB cap. Verifies the scanner doesn't choke on file-content-bearing
	// toolCall arguments.
	big := `{"type":"message","id":"x","message":{"role":"user","content":"` +
		strings.Repeat("a", 5*1024*1024) + `"}}` + "\n"
	scanner := NewScanner([]byte(big))
	if !scanner.Scan() {
		t.Fatalf("scanner failed on large line: %v", scanner.Err())
	}
	if len(scanner.Bytes()) < 5*1024*1024 {
		t.Errorf("got truncated line: %d bytes", len(scanner.Bytes()))
	}
}

func TestDecodeStringContent(t *testing.T) {
	t.Parallel()
	if got := DecodeStringContent([]byte(`"hello"`)); got != "hello" {
		t.Errorf("string content: got %q", got)
	}
	if got := DecodeStringContent([]byte(`[{"type":"text","text":"hi"}]`)); got != "" {
		t.Errorf("array content should return empty: got %q", got)
	}
	if got := DecodeStringContent(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
}

func TestForEachActiveEntry_YieldsEveryTypeWithPhysicalLine(t *testing.T) {
	t.Parallel()
	// Tree-shaped: the header (line 0) is off the branch; line 3 is malformed
	// and line 4 blank — both still count toward the line index. The
	// thinking_level_change entry (line 2) is on the branch and carries its
	// level.
	data := []byte(`{"type":"session","version":3,"id":"s1","timestamp":"2026-08-01T00:00:00Z"}
{"type":"message","id":"m1","parentId":null,"message":{"role":"user"}}
{"type":"thinking_level_change","id":"tl1","parentId":"m1","thinkingLevel":"high"}
{"type":"message","id":"m2",
` + "\n" + `{"type":"message","id":"m2","parentId":"tl1","message":{"role":"assistant"}}
`)
	var got []string
	if err := ForEachActiveEntry(data, 0, func(line int, e Entry) {
		got = append(got, fmt.Sprintf("%d:%s:%s", line, e.Type, e.ThinkingLevel))
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"1:message:", "2:thinking_level_change:high", "5:message:"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestForEachActiveEntry_OffsetKeepsPhysicalLine(t *testing.T) {
	t.Parallel()
	// Flat transcript (no parentId): every entry is on the branch, including
	// the header. Starting at line 2 must report line 2, not 0.
	data := []byte(`{"type":"session","id":"s1"}
{"type":"message","id":"m1","message":{"role":"user"}}
{"type":"thinking_level_change","id":"tl1","thinkingLevel":"low"}
{"type":"message","id":"m2","message":{"role":"assistant"}}
`)
	var got []string
	if err := ForEachActiveEntry(data, 2, func(line int, e Entry) {
		got = append(got, fmt.Sprintf("%d:%s", line, e.ID))
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"2:tl1", "3:m2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	// A negative offset behaves like 0 (SkipLines semantics).
	first := -1
	if err := ForEachActiveEntry(data, -3, func(line int, _ Entry) {
		if first < 0 {
			first = line
		}
	}); err != nil {
		t.Fatal(err)
	}
	if first != 0 {
		t.Errorf("negative offset: first line = %d, want 0", first)
	}
}

func TestForEachActiveEntry_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	called := false
	if err := ForEachActiveEntry(nil, 0, func(int, Entry) { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("fn should not be called for empty data")
	}
}

func TestEntry_DecodesProviderAndCost(t *testing.T) {
	t.Parallel()
	// Real Pi 0.70 shape: message.provider beside message.model, and pi-ai's
	// usage.cost block whose total is the agent-reported dollar cost.
	raw := `{"type":"message","id":"m1","message":{"role":"assistant","model":"claude-sonnet-4-5","provider":"anthropic","content":[],"usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":40,"cacheWrite1h":15,"totalTokens":100,"cost":{"input":0.001,"output":0.002,"cacheRead":0.0003,"cacheWrite":0.0004,"total":0.0037}}}}`
	var e Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Message.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", e.Message.Provider)
	}
	if e.Message.Usage == nil || e.Message.Usage.Cost.Total != 0.0037 {
		t.Errorf("Usage.Cost.Total = %+v, want 0.0037", e.Message.Usage)
	}
	if e.Message.Usage.CacheWrite1h != 15 {
		t.Errorf("CacheWrite1h = %d, want 15", e.Message.Usage.CacheWrite1h)
	}
}
