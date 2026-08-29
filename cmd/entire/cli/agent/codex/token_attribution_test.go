package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// 0-based line indices of the rows in testdata/attribution_rollout.jsonl —
// the coordinate AttributeTokens and CalculateTokenUsage share (see the
// AttributeTokens doc). The fixture is synthetic, modelled on Codex CLI
// 0.130–0.149 rollout shapes; the malformed row at index 10 keeps the indices
// honest. Each row's timestamp second equals its index.
const (
	fixtureLineSessionMeta = 0
	fixtureLineTurnHigh    = 1
	fixtureLineC1Call      = 2
	fixtureLineC1Output    = 3
	fixtureLineT1          = 4
	fixtureLineT1Dup       = 5
	fixtureLineC2Call      = 6
	fixtureLineC2Output    = 7
	fixtureLineC3Call      = 8
	fixtureLineT2          = 9
	fixtureLineGarbage     = 10
	fixtureLineTurnMedium  = 11
	fixtureLineC4Call      = 12
	fixtureLineT3          = 13
	fixtureLineT3Dup       = 14
	fixtureLineCount       = 15
)

const (
	fixtureModel       = "gpt-5.5"
	fixtureEffortHigh  = "high"
	fixtureToolExec    = "exec_command"
	toolNameShell      = "shell"
	fixtureDetailTest  = "go test ./..."
	fixtureDetailPatch = "src/a.go"
)

// Cumulative totals recorded by the fixture's three distinct token_count
// events, and the usage each call therefore has (the delta from the previous
// distinct total, fresh input = Δinput − Δcached).
var (
	fixtureCall1Usage = types.TokenUsage{InputTokens: 4000, CacheCreationTokens: 1500, CacheReadTokens: 8000, OutputTokens: 700, APICallCount: 1, ThinkingTokens: 300}
	fixtureCall2Usage = types.TokenUsage{InputTokens: 6000, CacheCreationTokens: 1000, CacheReadTokens: 12000, OutputTokens: 800, APICallCount: 1, ThinkingTokens: 300}
	fixtureCall3Usage = types.TokenUsage{InputTokens: 2000, CacheCreationTokens: 0, CacheReadTokens: 30000, OutputTokens: 700, APICallCount: 1, ThinkingTokens: 300}

	fixtureC1Ref = types.ToolUseRef{ID: "c1", Tool: fixtureToolExec, Detail: fixtureDetailTest}
	fixtureC2Ref = types.ToolUseRef{ID: "c2", Tool: "apply_patch", Detail: fixtureDetailPatch}
	fixtureC3Ref = types.ToolUseRef{ID: "c3", Tool: "spawn_agent", Detail: "explorer", SubagentType: "explorer"}
	fixtureC4Ref = types.ToolUseRef{ID: "c4", Tool: fixtureToolExec, Detail: "git log"}
)

func fixtureTime(t *testing.T, sec int) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("2026-08-28T10:00:%02d.000Z", sec))
	if err != nil {
		t.Fatalf("bad fixture second %d: %v", sec, err)
	}
	return ts
}

func readAttributionFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "attribution_rollout.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func attributeFixture(t *testing.T, startLine int, subagentsDir string) *types.Attribution {
	t.Helper()
	got, err := (&CodexAgent{}).AttributeTokens(readAttributionFixture(t), startLine, subagentsDir)
	if err != nil {
		t.Fatalf("AttributeTokens(startLine=%d): %v", startLine, err)
	}
	if got == nil {
		t.Fatalf("AttributeTokens(startLine=%d) returned nil Attribution", startLine)
	}
	return got
}

// fixtureOutputBytes is the raw JSON size of the `output` field on the
// fixture row at line — the number ToolResultRef.Bytes reports.
func fixtureOutputBytes(t *testing.T, line int) int {
	t.Helper()
	lines := splitJSONL(readAttributionFixture(t))
	var row struct {
		Payload struct {
			Output json.RawMessage `json:"output"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(lines[line], &row); err != nil {
		t.Fatalf("line %d does not parse: %v", line, err)
	}
	return len(row.Payload.Output)
}

// assertSame fails when got and want differ in length or in any element,
// naming the first differing index.
func assertSame[T comparable](t *testing.T, name string, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v, want %+v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %+v, want %+v", name, i, got[i], want[i])
		}
	}
}

// TestAttributionFixtureLayout pins the physical layout every line-index
// assertion below depends on: the row kind at each index, the malformed row
// in the middle, and that the two duplicate token_count rows repeat the
// totals of the row before them.
func TestAttributionFixtureLayout(t *testing.T) {
	t.Parallel()

	lines := splitJSONL(readAttributionFixture(t))
	if len(lines) != fixtureLineCount {
		t.Fatalf("fixture has %d lines, want %d", len(lines), fixtureLineCount)
	}
	wantKinds := map[int]string{
		fixtureLineSessionMeta: "session_meta", fixtureLineTurnHigh: "turn_context",
		fixtureLineC1Call: "response_item/function_call", fixtureLineC1Output: "response_item/function_call_output",
		fixtureLineT1: "event_msg/token_count", fixtureLineT1Dup: "event_msg/token_count",
		fixtureLineC2Call: "response_item/custom_tool_call", fixtureLineC2Output: "response_item/custom_tool_call_output",
		fixtureLineC3Call: "response_item/function_call", fixtureLineT2: "event_msg/token_count",
		fixtureLineTurnMedium: "turn_context", fixtureLineC4Call: "response_item/function_call",
		fixtureLineT3: "event_msg/token_count", fixtureLineT3Dup: "event_msg/token_count",
	}
	for line, want := range wantKinds {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(lines[line], &row); err != nil {
			t.Fatalf("line %d does not parse: %v", line, err)
		}
		kind := row.Type
		if row.Payload.Type != "" {
			kind += "/" + row.Payload.Type
		}
		if kind != want {
			t.Errorf("line %d is %q, want %q", line, kind, want)
		}
	}
	if json.Valid(lines[fixtureLineGarbage]) {
		t.Errorf("line %d parses as JSON, want the malformed row", fixtureLineGarbage)
	}
	for _, pair := range [][2]int{{fixtureLineT1, fixtureLineT1Dup}, {fixtureLineT3, fixtureLineT3Dup}} {
		a, b := tokenCountTotal(lines[pair[0]]), tokenCountTotal(lines[pair[1]])
		if a == nil || b == nil || *a != *b {
			t.Errorf("lines %d and %d should carry identical totals, got %+v and %+v", pair[0], pair[1], a, b)
		}
	}
}

func TestAttributeTokens_DistinctTotalsAreCalls(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	if len(got.Calls) != 3 {
		t.Fatalf("got %d calls, want 3 (duplicate token_count rows are not calls): %+v", len(got.Calls), got.Calls)
	}
	wantLines := []int{fixtureLineT1, fixtureLineT2, fixtureLineT3}
	wantUsage := []types.TokenUsage{fixtureCall1Usage, fixtureCall2Usage, fixtureCall3Usage}
	for i, call := range got.Calls {
		if call.Line != wantLines[i] {
			t.Errorf("Calls[%d].Line = %d, want %d (malformed row must still count)", i, call.Line, wantLines[i])
		}
		if !call.At.Equal(fixtureTime(t, wantLines[i])) {
			t.Errorf("Calls[%d].At = %v, want the token_count row's timestamp", i, call.At)
		}
		if call.Usage != wantUsage[i] {
			t.Errorf("Calls[%d].Usage = %+v, want %+v", i, call.Usage, wantUsage[i])
		}
		if call.UsageUnknown {
			t.Errorf("Calls[%d].UsageUnknown = true, want false (every token_count records usage)", i)
		}
		if call.Model != fixtureModel {
			t.Errorf("Calls[%d].Model = %q, want %q (bare id from turn_context)", i, call.Model, fixtureModel)
		}
		if call.ActiveSkill != "" {
			t.Errorf("Calls[%d].ActiveSkill = %q, want empty (Codex stamps none)", i, call.ActiveSkill)
		}
	}
	if got.Calls[0].Effort != fixtureEffortHigh || got.Calls[1].Effort != fixtureEffortHigh {
		t.Errorf("calls 1-2 Effort = %q, %q; want %q from the first turn_context", got.Calls[0].Effort, got.Calls[1].Effort, fixtureEffortHigh)
	}
	if got.Calls[2].Effort != "medium" {
		t.Errorf("call 3 Effort = %q, want medium from the later turn_context", got.Calls[2].Effort)
	}
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none", got.Subagents)
	}
	if got.AgentReportedCost != 0 {
		t.Errorf("AgentReportedCost = %v, want 0 (Codex records none)", got.AgentReportedCost)
	}
	if want := fixtureTime(t, fixtureLineSessionMeta); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want %v (first row of the slice)", got.Start, want)
	}
	if want := fixtureTime(t, fixtureLineT3Dup); !got.End.Equal(want) {
		t.Errorf("End = %v, want %v (a duplicate token_count still has a timestamp)", got.End, want)
	}
}

// TestAttributeTokens_EmittedAndConsumed pins the attribution rule: the tool
// calls and tool outputs between two distinct totals belong to the call at
// the LATER total — it emitted those calls and it consumed those outputs.
func TestAttributeTokens_EmittedAndConsumed(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	c1, c2, c3 := got.Calls[0], got.Calls[1], got.Calls[2]

	assertSame(t, "call1.Emitted", c1.Emitted, []types.ToolUseRef{fixtureC1Ref})
	// c1's output is a 4998-character string: 5000 raw bytes pins that Bytes
	// counts the JSON as written, quotes included.
	assertSame(t, "call1.Consumed", c1.Consumed, []types.ToolResultRef{{ToolUse: fixtureC1Ref, Bytes: 5000}})

	assertSame(t, "call2.Emitted", c2.Emitted, []types.ToolUseRef{fixtureC2Ref, fixtureC3Ref})
	assertSame(t, "call2.Consumed", c2.Consumed, []types.ToolResultRef{{ToolUse: fixtureC2Ref, Bytes: fixtureOutputBytes(t, fixtureLineC2Output)}})

	assertSame(t, "call3.Emitted", c3.Emitted, []types.ToolUseRef{fixtureC4Ref})
	if len(c3.Consumed) != 0 {
		t.Errorf("call3.Consumed = %+v, want none (no outputs between T2 and T3)", c3.Consumed)
	}
}

// TestAttributeTokens_StartLineBaseline pins the baseline rule shared with
// CalculateTokenUsage: the last distinct total before startLine is the
// baseline, the duplicate of it inside the slice is not a call, and calls
// before startLine are not emitted.
func TestAttributeTokens_StartLineBaseline(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureLineT1+1, "")
	if len(got.Calls) != 2 {
		t.Fatalf("got %d calls, want 2 (T1 is the baseline, its duplicate is not a call): %+v", len(got.Calls), got.Calls)
	}
	if got.Calls[0].Line != fixtureLineT2 || got.Calls[0].Usage != fixtureCall2Usage {
		t.Errorf("Calls[0] = line %d usage %+v, want line %d usage %+v (T2 − T1)", got.Calls[0].Line, got.Calls[0].Usage, fixtureLineT2, fixtureCall2Usage)
	}
	if got.Calls[1].Line != fixtureLineT3 || got.Calls[1].Usage != fixtureCall3Usage {
		t.Errorf("Calls[1] = line %d usage %+v, want line %d usage %+v (T3 − T2)", got.Calls[1].Line, got.Calls[1].Usage, fixtureLineT3, fixtureCall3Usage)
	}
	// Model/Effort come from a turn_context before the slice: state, not a row.
	if got.Calls[0].Model != fixtureModel || got.Calls[0].Effort != fixtureEffortHigh {
		t.Errorf("Calls[0] model/effort = %q/%q, want %q/%q from the pre-slice turn_context", got.Calls[0].Model, got.Calls[0].Effort, fixtureModel, fixtureEffortHigh)
	}
	if want := fixtureTime(t, fixtureLineT1Dup); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want %v (first row at/after startLine)", got.Start, want)
	}
	if want := fixtureTime(t, fixtureLineT3Dup); !got.End.Equal(want) {
		t.Errorf("End = %v, want %v", got.End, want)
	}
}

// TestAttributeTokens_LabelsResolveAcrossStartLine pins the full-transcript
// rules across a startLine that falls between a function_call and the total
// that closes it: the call at that total still emitted the pre-slice
// function_call (Emitted is a property of the call, not of the window) and
// its output, inside the slice, is labelled through the pre-slice call.
func TestAttributeTokens_LabelsResolveAcrossStartLine(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureLineC1Output, "")
	if len(got.Calls) != 3 {
		t.Fatalf("got %d calls, want 3 (no total precedes the slice, so T1 is a call): %+v", len(got.Calls), got.Calls)
	}
	c1 := got.Calls[0]
	if c1.Usage != fixtureCall1Usage {
		t.Errorf("call1.Usage = %+v, want %+v (no baseline)", c1.Usage, fixtureCall1Usage)
	}
	assertSame(t, "call1.Emitted", c1.Emitted, []types.ToolUseRef{fixtureC1Ref})
	assertSame(t, "call1.Consumed", c1.Consumed, []types.ToolResultRef{{ToolUse: fixtureC1Ref, Bytes: 5000}})
}

func TestAttributeTokens_SubagentsDirYieldsNoRecords(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, t.TempDir())
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none (Codex subagent rollouts are not read; see AttributeTokens doc)", got.Subagents)
	}
	if len(got.Calls) != 3 {
		t.Errorf("got %d calls, want 3 (subagentsDir must not change the calls)", len(got.Calls))
	}
}

func TestAttributeTokens_EmptyAndGarbage(t *testing.T) {
	t.Parallel()

	for name, data := range map[string][]byte{
		"empty":   nil,
		"garbage": []byte("not json\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":null}}\n"),
	} {
		got, err := (&CodexAgent{}).AttributeTokens(data, 0, "")
		if err != nil {
			t.Errorf("%s: err = %v, want nil", name, err)
		}
		if got == nil {
			t.Fatalf("%s: Attribution = nil, want empty", name)
		}
		if len(got.Calls) != 0 || len(got.Subagents) != 0 || !got.Start.IsZero() || !got.End.IsZero() {
			t.Errorf("%s: Attribution = %+v, want empty", name, got)
		}
	}
}

// TestAttributeTokens_ToolRefShapes pins the per-tool reduction on inline
// rows: the legacy `shell` tool's argv array (`bash -lc <script>` unwrapped
// to the script; `git -c …` NOT unwrapped; any other argv joined; a
// non-array `command` yields no Detail), a function_call whose arguments do
// not decode (id and name survive), a non-apply_patch custom tool, an output
// whose call_id no row emitted (kept, id only), and a call before the first
// turn_context (Model/Effort empty).
func TestAttributeTokens_ToolRefShapes(t *testing.T) {
	t.Parallel()

	data := `{"timestamp":"2026-08-28T11:00:00Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"npm run build\"]}","call_id":"s1"}}
{"timestamp":"2026-08-28T11:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\",\"-la\",\"src/\"]}","call_id":"s2"}}
{"timestamp":"2026-08-28T11:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"git\",\"-c\",\"core.pager=cat\",\"log\"]}","call_id":"s2b"}}
{"timestamp":"2026-08-28T11:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"npm run build\"}","call_id":"s2c"}}
{"timestamp":"2026-08-28T11:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"not json","call_id":"s3"}}
{"timestamp":"2026-08-28T11:00:03Z","type":"response_item","payload":{"type":"custom_tool_call","name":"other_tool","input":"*** Update File: not/a/patch.go","call_id":"s4"}}
{"timestamp":"2026-08-28T11:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"orphan","output":"12345"}}
{"timestamp":"2026-08-28T11:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":110}}}}
`
	got, err := (&CodexAgent{}).AttributeTokens([]byte(data), 0, "")
	if err != nil || len(got.Calls) != 1 {
		t.Fatalf("AttributeTokens = %+v, %v; want one call", got, err)
	}
	call := got.Calls[0]
	if call.Model != "" || call.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want empty before any turn_context", call.Model, call.Effort)
	}
	assertSame(t, "Emitted", call.Emitted, []types.ToolUseRef{
		{ID: "s1", Tool: toolNameShell, Detail: "npm run"},
		{ID: "s2", Tool: toolNameShell, Detail: "ls src/"},
		// Not a shell wrapper, so not unwrapped: ToolDetail sees the joined
		// argv and its assignment rule drops the `core.pager=cat` operand.
		{ID: "s2b", Tool: toolNameShell, Detail: "git log"},
		{ID: "s2c", Tool: toolNameShell},
		{ID: "s3", Tool: fixtureToolExec},
		{ID: "s4", Tool: "other_tool"},
	})
	assertSame(t, "Consumed", call.Consumed, []types.ToolResultRef{{ToolUse: types.ToolUseRef{ID: "orphan"}, Bytes: len(`"12345"`)}})
	if want := (types.TokenUsage{InputTokens: 100, OutputTokens: 10, APICallCount: 1}); call.Usage != want {
		t.Errorf("Usage = %+v, want %+v", call.Usage, want)
	}
}

// TestCalculateTokenUsage_CountsDistinctTotals pins the fix to APICallCount:
// duplicate token_count rows are one call, and cache writes are summed.
func TestCalculateTokenUsage_CountsDistinctTotals(t *testing.T) {
	t.Parallel()

	usage, err := (&CodexAgent{}).CalculateTokenUsage(readAttributionFixture(t), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage == nil {
		t.Fatal("CalculateTokenUsage returned nil usage")
	}
	want := types.TokenUsage{InputTokens: 12000, CacheCreationTokens: 2500, CacheReadTokens: 50000, OutputTokens: 2200, APICallCount: 3, ThinkingTokens: 900}
	if *usage != want {
		t.Errorf("CalculateTokenUsage = %+v, want %+v (3 distinct totals, not 5 events)", *usage, want)
	}

	// A duplicate straddling the offset is not a call either: the slice from
	// T3 holds only T3's duplicate, so there is no usage to report.
	usage, err = (&CodexAgent{}).CalculateTokenUsage(readAttributionFixture(t), fixtureLineT3+1)
	if err != nil {
		t.Fatalf("CalculateTokenUsage(after T3): %v", err)
	}
	if usage != nil {
		t.Errorf("CalculateTokenUsage(after T3) = %+v, want nil (the only row is a duplicate of the baseline)", *usage)
	}
}

// TestAttributeTokens_CallsIndependentOfStartLine pins window independence:
// a call is the same whole struct whatever startLine admits it — Line, usage,
// Model/Effort, Emitted and Consumed — so consecutive slices charge each tool
// call and each output exactly once and never shift it to a different call;
// and a slice holds exactly the offset-0 calls at or after it (a duplicate of
// the baseline inside the slice is still not a call).
func TestAttributeTokens_CallsIndependentOfStartLine(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	full := attributeFixture(t, 0, "")
	byLine := make(map[int]types.CallUsage, len(full.Calls))
	for _, call := range full.Calls {
		byLine[call.Line] = call
	}
	for start := 1; start < fixtureLineCount; start++ {
		got, err := (&CodexAgent{}).AttributeTokens(data, start, "")
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
