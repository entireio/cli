package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Line indices of the rows in testdata/attribution_session.jsonl. The fixture
// is synthetic (no real content) and modelled on the Claude Code 2.1.x row
// shapes; the malformed row at index 4 is what keeps these indices honest.
const (
	fixtureLinePrompt   = 0
	fixtureLineM1RowA   = 1
	fixtureLineM1RowB   = 2
	fixtureLineResults1 = 3
	fixtureLineGarbage  = 4
	fixtureLineM2       = 5
	fixtureLineLateRef  = 6
	fixtureLineM3       = 7
	fixtureLineM3Result = 8
	fixtureLineM4       = 9
	fixtureLineM4Result = 10
)

func fixtureTime(t *testing.T, sec int) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("2026-08-27T10:00:%02d.000Z", sec))
	if err != nil {
		t.Fatalf("bad fixture second %d: %v", sec, err)
	}
	return ts
}

func readAttributionFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "attribution_session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func attributeFixture(t *testing.T, startLine int, subagentsDir string) *types.Attribution {
	t.Helper()
	got, err := (&ClaudeCodeAgent{}).AttributeTokens(readAttributionFixture(t), startLine, subagentsDir)
	if err != nil {
		t.Fatalf("AttributeTokens(startLine=%d): %v", startLine, err)
	}
	if got == nil {
		t.Fatalf("AttributeTokens(startLine=%d) returned nil Attribution", startLine)
	}
	return got
}

// TestAttributionFixtureLayout pins the fixture's physical layout that every
// other test's line-index assertion depends on: the uuid at each row, the
// malformed row in the middle, and that streamed rows A/B share message.id.
func TestAttributionFixtureLayout(t *testing.T) {
	t.Parallel()

	lines := strings.Split(strings.TrimSuffix(string(readAttributionFixture(t)), "\n"), "\n")
	if len(lines) != fixtureLineM4Result+1 {
		t.Fatalf("fixture has %d lines, want %d", len(lines), fixtureLineM4Result+1)
	}
	wantUUIDs := map[int]string{
		fixtureLinePrompt: "u-0", fixtureLineM1RowA: "a-1a", fixtureLineM1RowB: "a-1b",
		fixtureLineResults1: "u-3", fixtureLineM2: "a-5", fixtureLineLateRef: "u-6",
		fixtureLineM3: "a-7", fixtureLineM3Result: "u-8", fixtureLineM4: "a-9", fixtureLineM4Result: "u-10",
	}
	for line, want := range wantUUIDs {
		var row struct {
			UUID    string `json:"uuid"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[line]), &row); err != nil {
			t.Fatalf("line %d does not parse: %v", line, err)
		}
		if row.UUID != want {
			t.Errorf("line %d uuid = %q, want %q", line, row.UUID, want)
		}
		if (line == fixtureLineM1RowA || line == fixtureLineM1RowB) && row.Message.ID != "msg_m1" {
			t.Errorf("line %d message.id = %q, want msg_m1 (streamed rows share it)", line, row.Message.ID)
		}
	}
	if json.Valid([]byte(lines[fixtureLineGarbage])) {
		t.Errorf("line %d parses as JSON, want the malformed row", fixtureLineGarbage)
	}
}

func TestAttributeTokens_CallsFromLineZero(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4: %+v", len(got.Calls), got.Calls)
	}
	wantLines := []int{fixtureLineM1RowA, fixtureLineM2, fixtureLineM3, fixtureLineM4}
	for i, want := range wantLines {
		if got.Calls[i].Line != want {
			t.Errorf("Calls[%d].Line = %d, want %d (malformed row must still count)", i, got.Calls[i].Line, want)
		}
	}
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none when subagentsDir is empty", got.Subagents)
	}
	if got.AgentReportedCost != 0 {
		t.Errorf("AgentReportedCost = %v, want 0 (Claude Code reports none)", got.AgentReportedCost)
	}
	// Start/End are the first and last timestamps seen in the slice — any
	// row, not only calls (types.Attribution doc).
	if want := fixtureTime(t, fixtureLinePrompt); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", got.Start, want)
	}
	if want := fixtureTime(t, fixtureLineM4Result); !got.End.Equal(want) {
		t.Errorf("End = %v, want %v", got.End, want)
	}
}

// TestAttributeTokens_StreamedMessageIsOneCall pins the dedupe rule: two rows
// sharing message.id are one call, usage comes from the row with the highest
// output_tokens (all fields from that row), Emitted is the union of tool_use
// blocks, and Line/At come from the first row.
func TestAttributeTokens_StreamedMessageIsOneCall(t *testing.T) {
	t.Parallel()

	m1 := attributeFixture(t, 0, "").Calls[0]
	if m1.UsageUnknown {
		t.Fatalf("m1.UsageUnknown = true, want usage recorded")
	}
	wantUsage := types.TokenUsage{
		InputTokens:           10,
		CacheCreationTokens:   200,
		CacheReadTokens:       1000,
		OutputTokens:          45,
		APICallCount:          1,
		ThinkingTokens:        12,
		CacheCreation1hTokens: 50,
	}
	if m1.Usage != wantUsage {
		t.Errorf("m1.Usage = %+v, want %+v (row B has the higher output_tokens)", m1.Usage, wantUsage)
	}
	if m1.Model != "claude-fable-5" {
		t.Errorf("m1.Model = %q, want claude-fable-5", m1.Model)
	}
	if m1.Effort != "high" {
		t.Errorf("m1.Effort = %q, want high", m1.Effort)
	}
	if m1.ActiveSkill != "systematic-debugging" {
		t.Errorf("m1.ActiveSkill = %q, want systematic-debugging", m1.ActiveSkill)
	}
	if want := fixtureTime(t, fixtureLineM1RowA); !m1.At.Equal(want) {
		t.Errorf("m1.At = %v, want %v (first row)", m1.At, want)
	}
	if len(m1.Consumed) != 0 {
		t.Errorf("m1.Consumed = %+v, want none (the prompt is not a tool result)", m1.Consumed)
	}

	wantEmitted := []types.ToolUseRef{
		{ID: "toolu_b1", Tool: "Bash", Detail: "go test ./cmd/entire/..."},
		{ID: "toolu_b2", Tool: "Bash", Detail: "git log"},
	}
	if len(m1.Emitted) != len(wantEmitted) {
		t.Fatalf("m1.Emitted = %+v, want %+v", m1.Emitted, wantEmitted)
	}
	for i, want := range wantEmitted {
		if m1.Emitted[i] != want {
			t.Errorf("m1.Emitted[%d] = %+v, want %+v", i, m1.Emitted[i], want)
		}
	}
}

// TestAttributeTokens_ConsumedResultsResolveToEmitters pins that the results
// between two calls are the later call's Consumed, labelled through the
// emitting tool_use and sized by the raw JSON bytes of the content field.
func TestAttributeTokens_ConsumedResultsResolveToEmitters(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	m2 := got.Calls[1]
	if m2.Line != fixtureLineM2 || m2.Model != "claude-fable-5" || m2.ActiveSkill != "" {
		t.Errorf("m2 = line %d model %q skill %q, want line %d, claude-fable-5, no skill", m2.Line, m2.Model, m2.ActiveSkill, fixtureLineM2)
	}
	if len(m2.Emitted) != 0 {
		t.Errorf("m2.Emitted = %+v, want none (text only)", m2.Emitted)
	}
	wantConsumed := []types.ToolResultRef{
		{ToolUse: types.ToolUseRef{ID: "toolu_b1", Tool: "Bash", Detail: "go test ./cmd/entire/..."}, Bytes: 12000},
		{ToolUse: types.ToolUseRef{ID: "toolu_b2", Tool: "Bash", Detail: "git log"}, Bytes: 300},
	}
	if len(m2.Consumed) != len(wantConsumed) {
		t.Fatalf("m2.Consumed = %+v, want %+v", m2.Consumed, wantConsumed)
	}
	for i, want := range wantConsumed {
		if m2.Consumed[i] != want {
			t.Errorf("m2.Consumed[%d] = %+v, want %+v", i, m2.Consumed[i], want)
		}
	}

	// The late reference at line 6 is new input to m3.
	m3 := got.Calls[2]
	if len(m3.Consumed) != 1 || m3.Consumed[0].ToolUse.ID != "toolu_b1" || m3.Consumed[0].Bytes != len(`"late reference"`) {
		t.Errorf("m3.Consumed = %+v, want the late toolu_b1 reference (16 bytes)", m3.Consumed)
	}
}

// TestAttributeTokens_AgentAndSkillEmits pins the subagent tool name: Claude
// Code's tool is "Agent" (hooks.go), which is what the fixture uses.
func TestAttributeTokens_AgentAndSkillEmits(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	m3, m4 := got.Calls[2], got.Calls[3]

	wantTask := types.ToolUseRef{ID: "toolu_t1", Tool: "Agent", Detail: "Explore (haiku)", SubagentType: "Explore", Model: "haiku"}
	if len(m3.Emitted) != 1 || m3.Emitted[0] != wantTask {
		t.Errorf("m3.Emitted = %+v, want [%+v]", m3.Emitted, wantTask)
	}
	if m3.Line != fixtureLineM3 {
		t.Errorf("m3.Line = %d, want %d", m3.Line, fixtureLineM3)
	}

	wantSkill := types.ToolUseRef{ID: "toolu_s1", Tool: "Skill", Detail: "artifact-design", SkillName: "artifact-design"}
	if len(m4.Emitted) != 1 || m4.Emitted[0] != wantSkill {
		t.Errorf("m4.Emitted = %+v, want [%+v]", m4.Emitted, wantSkill)
	}
	if m4.Line != fixtureLineM4 {
		t.Errorf("m4.Line = %d, want %d", m4.Line, fixtureLineM4)
	}
	// m4 records no usage: it still counts as a call, flagged rather than zero.
	if !m4.UsageUnknown {
		t.Errorf("m4.UsageUnknown = false, want true (fixture row has no usage)")
	}
	if m4.Usage != (types.TokenUsage{}) {
		t.Errorf("m4.Usage = %+v, want zero when unknown", m4.Usage)
	}
	// The Agent result (line 8) is consumed by m4, labelled with the Agent emit.
	if len(m4.Consumed) != 1 || m4.Consumed[0].ToolUse != wantTask {
		t.Errorf("m4.Consumed = %+v, want the Agent result labelled %+v", m4.Consumed, wantTask)
	}
}

// TestAttributeTokens_LegacyTaskNameAndDedupedEmits pins two rules on one
// inline transcript: the older "Task" tool name (and any casing) still fills
// SubagentType/Model, and a tool_use block repeated across streamed rows of
// one message is emitted once.
func TestAttributeTokens_LegacyTaskNameAndDedupedEmits(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","timestamp":"2026-08-27T11:00:00Z","message":{"id":"msg_1","model":"claude-fable-5","content":[{"type":"tool_use","id":"toolu_t","name":"Task","input":{"subagent_type":"Explore","model":"haiku"}}],"usage":{"input_tokens":1,"output_tokens":2}}}
{"type":"assistant","uuid":"a2","timestamp":"2026-08-27T11:00:01Z","message":{"id":"msg_1","model":"claude-fable-5","content":[{"type":"tool_use","id":"toolu_t","name":"Task","input":{"subagent_type":"Explore","model":"haiku"}},{"type":"tool_use","id":"toolu_k","name":"skill","input":{"skill":"design"}}],"usage":{"input_tokens":1,"output_tokens":9}}}
`
	got, err := (&ClaudeCodeAgent{}).AttributeTokens([]byte(data), 0, "")
	if err != nil || len(got.Calls) != 1 {
		t.Fatalf("got %+v, %v; want one call", got, err)
	}
	want := []types.ToolUseRef{
		{ID: "toolu_t", Tool: "Task", Detail: "Explore (haiku)", SubagentType: "Explore", Model: "haiku"},
		{ID: "toolu_k", Tool: "skill", Detail: "design", SkillName: "design"},
	}
	if len(got.Calls[0].Emitted) != len(want) {
		t.Fatalf("Emitted = %+v, want %+v (repeated block emitted once)", got.Calls[0].Emitted, want)
	}
	for i := range want {
		if got.Calls[0].Emitted[i] != want[i] {
			t.Errorf("Emitted[%d] = %+v, want %+v", i, got.Calls[0].Emitted[i], want[i])
		}
	}
}

// TestAttributeTokens_StartLineOnResultRow pins a slice that opens on a user
// row: its results are new input to the first call in the slice, and Start is
// that row's timestamp.
func TestAttributeTokens_StartLineOnResultRow(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureLineLateRef, "")
	if len(got.Calls) != 2 || got.Calls[0].Line != fixtureLineM3 {
		t.Fatalf("calls from line %d = %+v, want m3 (line %d) then m4", fixtureLineLateRef, got.Calls, fixtureLineM3)
	}
	want := types.ToolResultRef{ToolUse: types.ToolUseRef{ID: "toolu_b1", Tool: "Bash", Detail: "go test ./cmd/entire/..."}, Bytes: 16}
	if len(got.Calls[0].Consumed) != 1 || got.Calls[0].Consumed[0] != want {
		t.Errorf("Calls[0].Consumed = %+v, want [%+v]", got.Calls[0].Consumed, want)
	}
	if wantStart := fixtureTime(t, fixtureLineLateRef); !got.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", got.Start, wantStart)
	}
}

// TestAttributeTokens_SliceKeepsFullTranscriptLabels pins the label-map rule:
// calls before startLine are not emitted, but a result inside the slice that
// refers to a pre-slice tool_use is still labelled.
func TestAttributeTokens_SliceKeepsFullTranscriptLabels(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureLineM2, "")
	if len(got.Calls) != 3 {
		t.Fatalf("got %d calls from line %d, want 3: %+v", len(got.Calls), fixtureLineM2, got.Calls)
	}
	if got.Calls[0].Line != fixtureLineM2 {
		t.Errorf("Calls[0].Line = %d, want %d (Line stays in startLine's coordinate)", got.Calls[0].Line, fixtureLineM2)
	}
	// The results at line 3 precede the slice and were consumed by m2 before
	// the slice began: they must not be re-attributed.
	if len(got.Calls[0].Consumed) != 0 {
		t.Errorf("Calls[0].Consumed = %+v, want none (results before startLine are not new input in the slice)", got.Calls[0].Consumed)
	}
	m3 := got.Calls[1]
	if len(m3.Consumed) != 1 {
		t.Fatalf("m3.Consumed = %+v, want the late toolu_b1 reference", m3.Consumed)
	}
	if ref := m3.Consumed[0].ToolUse; ref.Tool != "Bash" || ref.Detail != "go test ./cmd/entire/..." {
		t.Errorf("pre-slice result label = %+v, want Bash / go test ./cmd/entire/... from the full transcript", ref)
	}
	if want := fixtureTime(t, fixtureLineM2); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want %v (first timestamp in the slice)", got.Start, want)
	}
	if want := fixtureTime(t, fixtureLineM4Result); !got.End.Equal(want) {
		t.Errorf("End = %v, want %v", got.End, want)
	}
}

func TestAttributeTokens_SubagentRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sub := `{"type":"user","uuid":"su-0","timestamp":"2026-08-27T10:00:07.100Z","message":{"role":"user","content":"Synthetic subagent prompt."}}
{"type":"assistant","uuid":"sa-1","timestamp":"2026-08-27T10:00:07.400Z","message":{"id":"msg_sub1","model":"claude-haiku-4-5","role":"assistant","content":[{"type":"text","text":"Looking."}],"usage":{"input_tokens":5,"cache_creation_input_tokens":40,"cache_read_input_tokens":300,"output_tokens":50}}}
{"type":"assistant","uuid":"sa-2","timestamp":"2026-08-27T10:00:07.900Z","message":{"id":"msg_sub2","model":"claude-haiku-4-5","role":"assistant","content":[{"type":"text","text":"Found it."}],"usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":340,"output_tokens":60}}}
`
	if err := os.WriteFile(filepath.Join(dir, "agent-abc.jsonl"), []byte(sub), 0o600); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}

	got := attributeFixture(t, 0, dir)
	if len(got.Subagents) != 1 {
		t.Fatalf("Subagents = %+v, want exactly one", got.Subagents)
	}
	rec := got.Subagents[0]
	if rec.ToolUseID != "toolu_t1" || rec.SubagentType != "Explore" || rec.Model != "claude-haiku-4-5" {
		t.Errorf("record = %+v, want ToolUseID toolu_t1, SubagentType Explore, Model claude-haiku-4-5", rec)
	}
	if rec.Usage == nil {
		t.Fatalf("record.Usage = nil, want the subagent's summed usage")
	}
	wantUsage := types.TokenUsage{
		InputTokens:         12,
		CacheCreationTokens: 40,
		CacheReadTokens:     640,
		OutputTokens:        110,
		APICallCount:        2,
		Model:               "claude-haiku-4-5",
	}
	if *rec.Usage != wantUsage {
		t.Errorf("record.Usage = %+v, want %+v", *rec.Usage, wantUsage)
	}
	wantStart := time.Date(2026, time.August, 27, 10, 0, 7, 100_000_000, time.UTC)
	wantEnd := time.Date(2026, time.August, 27, 10, 0, 7, 900_000_000, time.UTC)
	if !rec.Start.Equal(wantStart) || !rec.End.Equal(wantEnd) {
		t.Errorf("record Start/End = %v/%v, want %v/%v (first/last row timestamps of agent-abc.jsonl)", rec.Start, rec.End, wantStart, wantEnd)
	}

	// The record is discovered from the full transcript even when the slice
	// starts after the Agent call.
	sliced := attributeFixture(t, fixtureLineM4, dir)
	if len(sliced.Subagents) != 1 || sliced.Subagents[0].ToolUseID != "toolu_t1" {
		t.Errorf("sliced Subagents = %+v, want the same record from the full transcript", sliced.Subagents)
	}
}

func TestAttributeTokens_MissingSubagentTranscriptIsSkipped(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, t.TempDir())
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none when agent-abc.jsonl is absent", got.Subagents)
	}
	if len(got.Calls) != 4 {
		t.Errorf("got %d calls, want 4 (a missing subagent file must not affect the main walk)", len(got.Calls))
	}
}

func TestAttributeTokens_EmptyAndGarbage(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	got, err := ag.AttributeTokens(nil, 0, "")
	if err != nil || got == nil || len(got.Calls) != 0 || !got.Start.IsZero() {
		t.Errorf("empty transcript = %+v, %v; want empty Attribution and nil error", got, err)
	}

	// All lines malformed: the error contract follows CalculateTokenUsage —
	// malformed lines are skipped, never fatal, so this is an empty result.
	got, err = ag.AttributeTokens([]byte("not json\nalso not json\n"), 0, "")
	if err != nil || got == nil || len(got.Calls) != 0 {
		t.Errorf("garbage transcript = %+v, %v; want empty Attribution and nil error", got, err)
	}

	// startLine past the end: nothing in the slice.
	got, err = ag.AttributeTokens(readAttributionFixture(t), 100, "")
	if err != nil || got == nil || len(got.Calls) != 0 || !got.End.IsZero() {
		t.Errorf("startLine past end = %+v, %v; want empty Attribution and nil error", got, err)
	}
}

// TestAttributeTokens_UnresolvableResultKeepsIDAndBytes pins the choice for a
// tool_result whose tool_use appears nowhere in the transcript: the ref is
// kept (its bytes did enter the context) with only the ID filled in, so the
// report can label it as an earlier/unknown result rather than dropping it.
func TestAttributeTokens_UnresolvableResultKeepsIDAndBytes(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","timestamp":"2026-08-27T11:00:00Z","message":{"id":"msg_1","model":"claude-fable-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":2}}}
{"type":"user","uuid":"u1","timestamp":"2026-08-27T11:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_missing","content":"abcd"}]}}
{"type":"assistant","uuid":"a2","timestamp":"2026-08-27T11:00:02Z","message":{"id":"msg_2","model":"claude-fable-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":4}}}
`
	got, err := (&ClaudeCodeAgent{}).AttributeTokens([]byte(data), 0, "")
	if err != nil {
		t.Fatalf("AttributeTokens: %v", err)
	}
	if len(got.Calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(got.Calls))
	}
	want := types.ToolResultRef{ToolUse: types.ToolUseRef{ID: "toolu_missing"}, Bytes: len(`"abcd"`)}
	if len(got.Calls[1].Consumed) != 1 || got.Calls[1].Consumed[0] != want {
		t.Errorf("Consumed = %+v, want [%+v]", got.Calls[1].Consumed, want)
	}
}

// TestAttributeTokens_ResultsAfterLastCallAreNotConsumed pins that a result
// no later call has read yet is attributed to nothing (the next call, in a
// later slice, will consume it).
func TestAttributeTokens_ResultsAfterLastCallAreNotConsumed(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0, "")
	for i, call := range got.Calls {
		for _, ref := range call.Consumed {
			if ref.ToolUse.ID == "toolu_s1" {
				t.Errorf("Calls[%d] consumed the trailing Skill result; no call follows it", i)
			}
		}
	}
}
