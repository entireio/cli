package pijsonl

import (
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
