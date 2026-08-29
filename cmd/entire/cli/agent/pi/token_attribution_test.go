package pi

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Physical line indices in testdata/attribution_session.jsonl — the
// coordinate AttributeTokens' startLine and CallUsage.Line share with
// CalculateTokenUsage's fromOffset. The fixture is synthetic, in the shape
// Pi 0.70 writes (session header, tree entries with id/parentId, pi-ai
// usage with a cost block, message.provider beside message.model,
// thinking_level_change entries carrying thinkingLevel).
const (
	fixtureLineHeader    = 0
	fixtureLineUser1     = 1
	fixtureLineThinkHigh = 2  // thinking_level_change → high
	fixtureLineCall3     = 3  // claude-sonnet-4-5; bash + read toolCalls; cacheWrite + 1h + cost
	fixtureLineResultTC1 = 4  // toolResult tc1, 5000 raw content bytes
	fixtureLineResultTC2 = 5  // toolResult tc2, 300 raw content bytes
	fixtureLineCall6     = 6  // text only, cost 0.002
	fixtureLineMalformed = 7  // truncated JSON: skipped, still counted
	fixtureLineThinkLow  = 8  // thinking_level_change → low
	fixtureLineCall9     = 9  // gpt-5.5, no usage block; bash toolCall tc3
	fixtureLineResultTC3 = 10 // toolResult tc3, 41 raw content bytes
	fixtureLineCall11    = 11 // gpt-5.5, cost 0.01
	fixtureLineAbandoned = 12 // assistant forked off m4: off the active branch
	// fixtureLineUser13 is the user continuing from m8: the leaf. It keeps m9
	// from being the last message entry and thus the leaf — without it the
	// abandoned fork would become the active branch.
	fixtureLineUser13     = 13
	fixtureLineCount      = 14
	fixtureCallCount      = 4
	fixtureSliceCallCount = 3 // calls from fixtureLineResultTC1 onward
)

const (
	fixtureModelSonnet = "claude-sonnet-4-5"
	fixtureModelGPT    = "gpt-5.5"
	fixtureLevelHigh   = "high"
	fixtureLevelLow    = "low"

	fixtureResultTC1Bytes = 5000
	fixtureResultTC2Bytes = 300
	fixtureResultTC3Bytes = 41

	fixtureCost3  = 0.0123
	fixtureCost6  = 0.002
	fixtureCost11 = 0.01
)

var (
	fixtureCall3Usage  = types.TokenUsage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, CacheCreationTokens: 400, CacheCreation1hTokens: 150, APICallCount: 1}
	fixtureCall6Usage  = types.TokenUsage{InputTokens: 6000, OutputTokens: 50, CacheReadTokens: 3400, APICallCount: 1}
	fixtureCall11Usage = types.TokenUsage{InputTokens: 7000, OutputTokens: 80, APICallCount: 1}

	fixtureBashRef  = types.ToolUseRef{ID: "tc1", Tool: "bash", Detail: "go test ./..."}
	fixtureReadRef  = types.ToolUseRef{ID: "tc2", Tool: "read", Detail: "src/a.go"}
	fixtureBash3Ref = types.ToolUseRef{ID: "tc3", Tool: "bash", Detail: "git status"}

	// Consumed by the assistant message AFTER the one that emitted them.
	fixtureCall6Consumed = []types.ToolResultRef{
		{ToolUse: fixtureBashRef, Bytes: fixtureResultTC1Bytes},
		{ToolUse: fixtureReadRef, Bytes: fixtureResultTC2Bytes},
	}
	fixtureCall11Consumed = []types.ToolResultRef{{ToolUse: fixtureBash3Ref, Bytes: fixtureResultTC3Bytes}}
)

func readAttributionFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "attribution_session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func attributeFixture(t *testing.T, startLine int) *types.Attribution {
	t.Helper()
	got, err := (&PiAgent{}).AttributeTokens(readAttributionFixture(t), startLine, "")
	if err != nil {
		t.Fatalf("AttributeTokens(startLine=%d): %v", startLine, err)
	}
	if got == nil {
		t.Fatalf("AttributeTokens(startLine=%d): nil result", startLine)
	}
	return got
}

// fixtureTime is the fixture's timestamp for line i: 10:00:00Z plus i seconds.
func fixtureTime(line int) time.Time {
	return time.Date(2026, 8, 20, 10, 0, line, 0, time.UTC)
}

func assertSame[T comparable](t *testing.T, name string, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d items %+v, want %d %+v", name, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %+v, want %+v", name, i, got[i], want[i])
		}
	}
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestAttributionFixtureLayout(t *testing.T) {
	t.Parallel()
	data := readAttributionFixture(t)
	if n := pijsonl.CountLines(data); n != fixtureLineCount {
		t.Fatalf("fixture has %d lines, want %d — the line constants are stale", n, fixtureLineCount)
	}
	got := attributeFixture(t, 0)
	if len(got.Calls) != fixtureCallCount {
		t.Fatalf("got %d calls, want %d: %+v", len(got.Calls), fixtureCallCount, got.Calls)
	}
	lines := make([]int, 0, len(got.Calls))
	for _, c := range got.Calls {
		lines = append(lines, c.Line)
	}
	assertSame(t, "Line", lines, []int{fixtureLineCall3, fixtureLineCall6, fixtureLineCall9, fixtureLineCall11})
}

func TestAttributeTokens_CallFields(t *testing.T) {
	t.Parallel()
	got := attributeFixture(t, 0)
	if len(got.Calls) != fixtureCallCount {
		t.Fatalf("got %d calls", len(got.Calls))
	}

	c3 := got.Calls[0]
	if c3.Usage != fixtureCall3Usage || c3.UsageUnknown {
		t.Errorf("call3 Usage = %+v (unknown %v), want %+v", c3.Usage, c3.UsageUnknown, fixtureCall3Usage)
	}
	if c3.Model != fixtureModelSonnet {
		t.Errorf("call3 Model = %q, want bare id %q (provider is not prefixed)", c3.Model, fixtureModelSonnet)
	}
	if c3.Effort != fixtureLevelHigh {
		t.Errorf("call3 Effort = %q, want %q", c3.Effort, fixtureLevelHigh)
	}
	if !c3.At.Equal(fixtureTime(fixtureLineCall3)) {
		t.Errorf("call3 At = %v, want %v", c3.At, fixtureTime(fixtureLineCall3))
	}
	assertSame(t, "call3 Emitted", c3.Emitted, []types.ToolUseRef{fixtureBashRef, fixtureReadRef})
	if len(c3.Consumed) != 0 {
		t.Errorf("call3 Consumed = %+v, want none (nothing precedes it)", c3.Consumed)
	}
	if c3.ActiveSkill != "" {
		t.Errorf("call3 ActiveSkill = %q, want empty (Pi stamps none)", c3.ActiveSkill)
	}

	c6 := got.Calls[1]
	if c6.Usage != fixtureCall6Usage || c6.UsageUnknown {
		t.Errorf("call6 Usage = %+v, want %+v", c6.Usage, fixtureCall6Usage)
	}
	if c6.Effort != fixtureLevelHigh {
		t.Errorf("call6 Effort = %q, want %q (level still in force)", c6.Effort, fixtureLevelHigh)
	}
	assertSame(t, "call6 Consumed", c6.Consumed, fixtureCall6Consumed)
	if len(c6.Emitted) != 0 {
		t.Errorf("call6 Emitted = %+v, want none", c6.Emitted)
	}

	c9 := got.Calls[2]
	if !c9.UsageUnknown || c9.Usage != (types.TokenUsage{}) {
		t.Errorf("call9: UsageUnknown %v Usage %+v, want unknown and zero", c9.UsageUnknown, c9.Usage)
	}
	if c9.Effort != fixtureLevelLow {
		t.Errorf("call9 Effort = %q, want %q", c9.Effort, fixtureLevelLow)
	}
	if c9.Model != fixtureModelGPT {
		t.Errorf("call9 Model = %q, want %q", c9.Model, fixtureModelGPT)
	}
	assertSame(t, "call9 Emitted", c9.Emitted, []types.ToolUseRef{fixtureBash3Ref})
	if len(c9.Consumed) != 0 {
		t.Errorf("call9 Consumed = %+v, want none", c9.Consumed)
	}

	c11 := got.Calls[3]
	if c11.Usage != fixtureCall11Usage || c11.UsageUnknown {
		t.Errorf("call11 Usage = %+v, want %+v", c11.Usage, fixtureCall11Usage)
	}
	if c11.Model != fixtureModelGPT || c11.Effort != fixtureLevelLow {
		t.Errorf("call11 Model/Effort = %q/%q, want %q/%q", c11.Model, c11.Effort, fixtureModelGPT, fixtureLevelLow)
	}
	// The call without a usage block still labels the result it caused.
	assertSame(t, "call11 Consumed", c11.Consumed, fixtureCall11Consumed)
}

func TestAttributeTokens_AbandonedBranchIgnored(t *testing.T) {
	t.Parallel()
	got := attributeFixture(t, 0)
	for _, c := range got.Calls {
		if c.Line == fixtureLineAbandoned {
			t.Fatalf("abandoned-branch assistant at line %d became a call: %+v", fixtureLineAbandoned, c)
		}
		for _, ref := range c.Emitted {
			if ref.ID == "tc9" {
				t.Errorf("abandoned-branch toolCall tc9 emitted by call at line %d", c.Line)
			}
		}
	}
	// Its timestamp (10:00:12) must not widen End; the active leaf is the user
	// message at line 13, and Start is the first active entry (the header is
	// off the branch in a tree-shaped transcript).
	if !got.Start.Equal(fixtureTime(fixtureLineUser1)) {
		t.Errorf("Start = %v, want %v", got.Start, fixtureTime(fixtureLineUser1))
	}
	if !got.End.Equal(fixtureTime(fixtureLineUser13)) {
		t.Errorf("End = %v, want %v", got.End, fixtureTime(fixtureLineUser13))
	}
	// Its $2 cost must not leak into the sum either.
	assertClose(t, "AgentReportedCost", got.AgentReportedCost, fixtureCost3+fixtureCost6+fixtureCost11)
}

func TestAttributeTokens_SumsMatchCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	data := readAttributionFixture(t)
	for _, startLine := range []int{fixtureLineHeader, fixtureLineResultTC1, fixtureLineThinkLow, fixtureLineCall9} {
		want, err := (&PiAgent{}).CalculateTokenUsage(data, startLine)
		if err != nil {
			t.Fatalf("CalculateTokenUsage(%d): %v", startLine, err)
		}
		got := attributeFixture(t, startLine)
		var sum types.TokenUsage
		for _, c := range got.Calls {
			if c.UsageUnknown {
				continue
			}
			sum.InputTokens += c.Usage.InputTokens
			sum.OutputTokens += c.Usage.OutputTokens
			sum.CacheReadTokens += c.Usage.CacheReadTokens
			sum.CacheCreationTokens += c.Usage.CacheCreationTokens
			sum.CacheCreation1hTokens += c.Usage.CacheCreation1hTokens
			sum.ThinkingTokens += c.Usage.ThinkingTokens
			sum.APICallCount += c.Usage.APICallCount
		}
		if sum != *want {
			t.Errorf("startLine %d: Σ calls = %+v, CalculateTokenUsage = %+v", startLine, sum, *want)
		}
	}
}

func TestAttributeTokens_StartLineSlice(t *testing.T) {
	t.Parallel()
	got := attributeFixture(t, fixtureLineResultTC1)
	if len(got.Calls) != fixtureSliceCallCount {
		t.Fatalf("got %d calls, want %d: %+v", len(got.Calls), fixtureSliceCallCount, got.Calls)
	}
	first := got.Calls[0]
	if first.Line != fixtureLineCall6 {
		t.Errorf("first call Line = %d, want %d", first.Line, fixtureLineCall6)
	}
	// The results at lines 4–5 are in the slice; their toolCalls (line 3) are
	// not, but the labels come from the full transcript.
	assertSame(t, "first Consumed", first.Consumed, fixtureCall6Consumed)
	// The thinking level was set at line 2, before the slice, and still
	// applies.
	if first.Effort != fixtureLevelHigh {
		t.Errorf("first Effort = %q, want %q (tracked from before the slice)", first.Effort, fixtureLevelHigh)
	}
	if !got.Start.Equal(fixtureTime(fixtureLineResultTC1)) {
		t.Errorf("Start = %v, want slice start %v", got.Start, fixtureTime(fixtureLineResultTC1))
	}
	assertClose(t, "AgentReportedCost", got.AgentReportedCost, fixtureCost6+fixtureCost11)
}

// TestAttributeTokens_CallsIndependentOfStartLine pins window independence:
// a call is the same whole struct whatever startLine admits it — Line, usage,
// Effort, Emitted and, in particular, Consumed — so consecutive slices charge
// each result exactly once and never shift it to a different call.
func TestAttributeTokens_CallsIndependentOfStartLine(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	full := attributeFixture(t, 0)
	byLine := make(map[int]types.CallUsage, len(full.Calls))
	for _, call := range full.Calls {
		byLine[call.Line] = call
	}
	for start := 1; start < fixtureLineCount; start++ {
		got, err := (&PiAgent{}).AttributeTokens(data, start, "")
		if err != nil {
			t.Fatalf("AttributeTokens(startLine=%d): %v", start, err)
		}
		wantCount := 0
		for line := range byLine {
			if line >= start {
				wantCount++
			}
		}
		if len(got.Calls) != wantCount {
			t.Errorf("startLine %d: got %d calls, want the %d offset-0 calls at Line >= %d", start, len(got.Calls), wantCount, start)
		}
		for _, call := range got.Calls {
			if call.Line < start {
				t.Errorf("startLine %d: call at Line %d precedes the slice", start, call.Line)
			}
			want, ok := byLine[call.Line]
			if !ok {
				t.Errorf("startLine %d: call at Line %d is not a call from 0", start, call.Line)
				continue
			}
			if !reflect.DeepEqual(call, want) {
				t.Errorf("startLine %d: call at Line %d = %+v, want the same call from 0: %+v", start, call.Line, call, want)
			}
		}
	}
}

func TestAttributeTokens_StartLineAfterLastCall(t *testing.T) {
	t.Parallel()
	// Only the abandoned entry and the trailing user message remain: no
	// calls, but the user message still bounds Start/End.
	got := attributeFixture(t, fixtureLineAbandoned)
	if len(got.Calls) != 0 {
		t.Errorf("got %d calls, want 0", len(got.Calls))
	}
	if !got.Start.Equal(fixtureTime(fixtureLineUser13)) || !got.End.Equal(fixtureTime(fixtureLineUser13)) {
		t.Errorf("Start/End = %v/%v, want both %v", got.Start, got.End, fixtureTime(fixtureLineUser13))
	}
	if got.AgentReportedCost != 0 {
		t.Errorf("AgentReportedCost = %v, want 0", got.AgentReportedCost)
	}
	past := attributeFixture(t, fixtureLineCount+10)
	if len(past.Calls) != 0 || !past.Start.IsZero() {
		t.Errorf("past end: %+v, want empty", past)
	}
}

func TestAttributeTokens_EmptyAndGarbage(t *testing.T) {
	t.Parallel()
	for name, data := range map[string][]byte{
		"empty":   nil,
		"garbage": []byte("not json\n{\"type\":\n\n"),
	} {
		got, err := (&PiAgent{}).AttributeTokens(data, 0, "")
		if err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
		if got == nil || len(got.Calls) != 0 || len(got.Subagents) != 0 || !got.Start.IsZero() || got.AgentReportedCost != 0 {
			t.Errorf("%s: got %+v, want empty attribution", name, got)
		}
	}
}

func TestAttributeTokens_SubagentsDirIgnored(t *testing.T) {
	t.Parallel()
	got, err := (&PiAgent{}).AttributeTokens(readAttributionFixture(t), 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none (Pi has no subagent transcripts)", got.Subagents)
	}
}

func TestAttributeTokens_UnlabelledResultKeepsID(t *testing.T) {
	t.Parallel()
	// A toolResult whose toolCall appears nowhere (flat transcript, no tree)
	// still counts its bytes against the next call, with only the id known.
	// The result after the last call (m3) is consumed by nothing.
	data := []byte(`{"type":"session","id":"s"}
{"type":"message","id":"m1","timestamp":"2026-08-01T00:00:01Z","message":{"role":"toolResult","toolCallId":"orphan","toolName":"bash","content":"12345"}}
{"type":"message","id":"m2","timestamp":"2026-08-01T00:00:02Z","message":{"role":"assistant","model":"gpt-5.5","content":"ok","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","timestamp":"2026-08-01T00:00:03Z","message":{"role":"toolResult","toolCallId":"trailing","toolName":"bash","content":"after the last call"}}
`)
	got, err := (&PiAgent{}).AttributeTokens(data, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(got.Calls))
	}
	want := []types.ToolResultRef{{ToolUse: types.ToolUseRef{ID: "orphan"}, Bytes: len(`"12345"`)}}
	assertSame(t, "Consumed (trailing result attributed to nothing)", got.Calls[0].Consumed, want)
	if got.Calls[0].Effort != "" {
		t.Errorf("Effort = %q, want empty before any thinking_level_change", got.Calls[0].Effort)
	}
}
