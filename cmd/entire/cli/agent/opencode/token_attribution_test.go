package opencode

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Message indices in testdata/attribution_export.json — the coordinate
// AttributeTokens' startLine and CallUsage.Line share with
// CalculateTokenUsage's fromOffset. The fixture is synthetic, modelled on
// `opencode export` output from OpenCode 1.3 (2026-08-28): assistant info
// carries top-level modelID/providerID/cost, tool parts hold their result in
// state.output, and the task part's state.metadata names the child session
// and the model it ran on.
const (
	fixtureMsgUser0      = 0
	fixtureMsgAssistant1 = 1 // bash + read; reasoning OUTSIDE output
	fixtureMsgUser2      = 2
	fixtureMsgAssistant3 = 3 // task; reasoning INSIDE output
	fixtureMsgAssistant4 = 4 // skill; no tokens block
	fixtureMsgUser5      = 5
	fixtureMsgAssistant6 = 6 // text only
	fixtureMsgCount      = 7
)

// Fixture timestamps are fixtureBaseMillis plus a per-message offset.
const (
	fixtureBaseMillis          int64 = 1773867525000
	fixtureUser0Created              = fixtureBaseMillis + 15
	fixtureAssistant1Created         = fixtureBaseMillis + 23
	fixtureAssistant3Created         = fixtureBaseMillis + 20010
	fixtureAssistant4Created         = fixtureBaseMillis + 45010
	fixtureAssistant6Created         = fixtureBaseMillis + 60005
	fixtureAssistant6Completed       = fixtureBaseMillis + 61000
)

const (
	fixtureModel        = "claude-sonnet-4-5"
	fixtureSubagentType = "explore"
	fixtureSkill        = "artifact-design"
	fixtureCostSum      = 0.0123 + 0.0045 + 0.0021
)

// Per-call usage: message 1's total covers reasoning as a separate term so
// BilledOutput adds it (300+100); message 3's does not (reasoning already
// inside its 250).
var (
	fixtureCall1Usage = types.TokenUsage{InputTokens: 1200, OutputTokens: 400, ThinkingTokens: 100, CacheReadTokens: 4000, CacheCreationTokens: 800, APICallCount: 1}
	fixtureCall3Usage = types.TokenUsage{InputTokens: 50, OutputTokens: 250, ThinkingTokens: 80, CacheReadTokens: 6000, APICallCount: 1}
	fixtureCall6Usage = types.TokenUsage{InputTokens: 10, OutputTokens: 120, CacheReadTokens: 7000, CacheCreationTokens: 100, APICallCount: 1}

	fixtureBashRef  = types.ToolUseRef{ID: "call_bash_1", Tool: "bash", Detail: "go test ./..."}
	fixtureReadRef  = types.ToolUseRef{ID: "call_read_1", Tool: "read", Detail: "src/a.go"}
	fixtureTaskRef  = types.ToolUseRef{ID: "call_task_1", Tool: "task", Detail: fixtureSubagentType, SubagentType: fixtureSubagentType, Model: "claude-haiku-4-5"}
	fixtureSkillRef = types.ToolUseRef{ID: "call_skill_1", Tool: "skill", Detail: fixtureSkill, SkillName: fixtureSkill}

	// Consumed by the assistant message AFTER the one that emitted them.
	fixtureMsg1Results = []types.ToolResultRef{{ToolUse: fixtureBashRef, Bytes: 5000}, {ToolUse: fixtureReadRef, Bytes: 300}}
	fixtureMsg3Results = []types.ToolResultRef{{ToolUse: fixtureTaskRef, Bytes: 40}}
	fixtureMsg4Results = []types.ToolResultRef{{ToolUse: fixtureSkillRef, Bytes: 120}}
)

func readAttributionFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "attribution_export.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func attributeFixture(t *testing.T, startLine int) *types.Attribution {
	t.Helper()
	got, err := (&OpenCodeAgent{}).AttributeTokens(readAttributionFixture(t), startLine, "")
	if err != nil {
		t.Fatalf("AttributeTokens(startLine=%d): %v", startLine, err)
	}
	if got == nil {
		t.Fatalf("AttributeTokens(startLine=%d) returned nil Attribution", startLine)
	}
	return got
}

func fixtureTime(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
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

// TestAttributionFixtureLayout pins the message roles and the token-block
// presence every index assertion below depends on.
func TestAttributionFixtureLayout(t *testing.T) {
	t.Parallel()

	session, err := ParseExportSession(readAttributionFixture(t))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(session.Messages) != fixtureMsgCount {
		t.Fatalf("fixture has %d messages, want %d", len(session.Messages), fixtureMsgCount)
	}
	wantRoles := map[int]string{
		fixtureMsgUser0: roleUser, fixtureMsgAssistant1: roleAssistant, fixtureMsgUser2: roleUser,
		fixtureMsgAssistant3: roleAssistant, fixtureMsgAssistant4: roleAssistant,
		fixtureMsgUser5: roleUser, fixtureMsgAssistant6: roleAssistant,
	}
	for i, want := range wantRoles {
		if got := session.Messages[i].Info.Role; got != want {
			t.Errorf("message %d role = %q, want %q", i, got, want)
		}
	}
	if session.Messages[fixtureMsgAssistant4].Info.Tokens != nil {
		t.Errorf("message %d should record no tokens block", fixtureMsgAssistant4)
	}
	if got := session.Messages[fixtureMsgAssistant1].Info.ProviderID; got != "anthropic" {
		t.Errorf("message %d providerID = %q, want it decoded from the top-level key", fixtureMsgAssistant1, got)
	}
	task := session.Messages[fixtureMsgAssistant3].Parts[1]
	if task.State == nil || task.State.Metadata == nil || task.State.Metadata.SessionID != "ses_child_1" {
		t.Errorf("task part metadata = %+v, want sessionId decoded", task.State)
	}
}

func TestAttributeTokens_OneCallPerAssistantMessage(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4: %+v", len(got.Calls), got.Calls)
	}
	wantLines := []int{fixtureMsgAssistant1, fixtureMsgAssistant3, fixtureMsgAssistant4, fixtureMsgAssistant6}
	wantAt := []int64{fixtureAssistant1Created, fixtureAssistant3Created, fixtureAssistant4Created, fixtureAssistant6Created}
	for i, call := range got.Calls {
		if call.Line != wantLines[i] {
			t.Errorf("Calls[%d].Line = %d, want message index %d", i, call.Line, wantLines[i])
		}
		if !call.At.Equal(fixtureTime(wantAt[i])) {
			t.Errorf("Calls[%d].At = %v, want %v (time.created as epoch millis)", i, call.At, fixtureTime(wantAt[i]))
		}
		if call.Model != fixtureModel {
			t.Errorf("Calls[%d].Model = %q, want the bare modelID %q", i, call.Model, fixtureModel)
		}
		if call.Effort != "" || call.ActiveSkill != "" {
			t.Errorf("Calls[%d] Effort/ActiveSkill = %q/%q, want empty (OpenCode records neither)", i, call.Effort, call.ActiveSkill)
		}
	}
}

func TestAttributeTokens_UsagePerCall(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	if got.Calls[0].Usage != fixtureCall1Usage || got.Calls[0].UsageUnknown {
		t.Errorf("Calls[0].Usage = %+v (unknown=%v), want %+v (reasoning outside output is billed)", got.Calls[0].Usage, got.Calls[0].UsageUnknown, fixtureCall1Usage)
	}
	if got.Calls[1].Usage != fixtureCall3Usage || got.Calls[1].UsageUnknown {
		t.Errorf("Calls[1].Usage = %+v (unknown=%v), want %+v (reasoning inside output is not added)", got.Calls[1].Usage, got.Calls[1].UsageUnknown, fixtureCall3Usage)
	}
	if !got.Calls[2].UsageUnknown || got.Calls[2].Usage != (types.TokenUsage{}) {
		t.Errorf("Calls[2] = %+v, want UsageUnknown with zero Usage (no tokens block)", got.Calls[2])
	}
	if got.Calls[3].Usage != fixtureCall6Usage || got.Calls[3].UsageUnknown {
		t.Errorf("Calls[3].Usage = %+v (unknown=%v), want %+v", got.Calls[3].Usage, got.Calls[3].UsageUnknown, fixtureCall6Usage)
	}
}

// TestAttributeTokens_SumsMatchCalculateTokenUsage guards the shared
// arithmetic: the per-call usages add up to the session total.
func TestAttributeTokens_SumsMatchCalculateTokenUsage(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	ag := &OpenCodeAgent{}
	for _, start := range []int{0, fixtureMsgAssistant3, fixtureMsgUser5} {
		total, err := ag.CalculateTokenUsage(data, start)
		if err != nil {
			t.Fatalf("CalculateTokenUsage(%d): %v", start, err)
		}
		got, err := ag.AttributeTokens(data, start, "")
		if err != nil {
			t.Fatalf("AttributeTokens(%d): %v", start, err)
		}
		var sum types.TokenUsage
		for _, c := range got.Calls {
			if c.UsageUnknown {
				continue
			}
			sum.InputTokens += c.Usage.InputTokens
			sum.OutputTokens += c.Usage.OutputTokens
			sum.ThinkingTokens += c.Usage.ThinkingTokens
			sum.CacheReadTokens += c.Usage.CacheReadTokens
			sum.CacheCreationTokens += c.Usage.CacheCreationTokens
			sum.APICallCount += c.Usage.APICallCount
		}
		if sum != *total {
			t.Errorf("startLine %d: Σ calls = %+v, CalculateTokenUsage = %+v", start, sum, *total)
		}
	}
}

func TestAttributeTokens_EmittedRefs(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	assertSame(t, "Calls[0].Emitted", got.Calls[0].Emitted, []types.ToolUseRef{fixtureBashRef, fixtureReadRef})
	assertSame(t, "Calls[1].Emitted", got.Calls[1].Emitted, []types.ToolUseRef{fixtureTaskRef})
	assertSame(t, "Calls[2].Emitted", got.Calls[2].Emitted, []types.ToolUseRef{fixtureSkillRef})
	if len(got.Calls[3].Emitted) != 0 {
		t.Errorf("Calls[3].Emitted = %+v, want none (text only)", got.Calls[3].Emitted)
	}
}

// TestAttributeTokens_ResultsConsumedByNextCall: a tool's result lives on the
// emitting part, so it is charged to the NEXT assistant message — across an
// intervening user message, and from a call that recorded no usage.
func TestAttributeTokens_ResultsConsumedByNextCall(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	if len(got.Calls[0].Consumed) != 0 {
		t.Errorf("Calls[0].Consumed = %+v, want none (nothing precedes it)", got.Calls[0].Consumed)
	}
	assertSame(t, "Calls[1].Consumed", got.Calls[1].Consumed, fixtureMsg1Results)
	assertSame(t, "Calls[2].Consumed", got.Calls[2].Consumed, fixtureMsg3Results)
	assertSame(t, "Calls[3].Consumed", got.Calls[3].Consumed, fixtureMsg4Results)
}

func TestAttributeTokens_StartLineIsMessageIndex(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureMsgAssistant3)
	if len(got.Calls) != 3 {
		t.Fatalf("got %d calls from message %d, want 3: %+v", len(got.Calls), fixtureMsgAssistant3, got.Calls)
	}
	if got.Calls[0].Line != fixtureMsgAssistant3 {
		t.Errorf("Calls[0].Line = %d, want %d", got.Calls[0].Line, fixtureMsgAssistant3)
	}
	// The pre-slice message's results are still charged to the first call
	// that read them, fully labelled.
	assertSame(t, "Calls[0].Consumed", got.Calls[0].Consumed, fixtureMsg1Results)
	if !got.Start.Equal(fixtureTime(fixtureAssistant3Created)) {
		t.Errorf("Start = %v, want message %d's created time", got.Start, fixtureMsgAssistant3)
	}
	if !got.End.Equal(fixtureTime(fixtureAssistant6Completed)) {
		t.Errorf("End = %v, want message %d's completed time", got.End, fixtureMsgAssistant6)
	}
	if math.Abs(got.AgentReportedCost-(0.0045+0.0021)) > 1e-9 {
		t.Errorf("AgentReportedCost = %v, want the slice's cost only", got.AgentReportedCost)
	}
}

// TestAttributeTokens_CallsIndependentOfStartLine pins window independence:
// a call is the same whole struct whatever startLine admits it — Line, usage,
// Emitted and, in particular, Consumed — so consecutive slices charge each
// result exactly once and never shift it to a different call.
func TestAttributeTokens_CallsIndependentOfStartLine(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	full := attributeFixture(t, 0)
	byLine := make(map[int]types.CallUsage, len(full.Calls))
	for _, call := range full.Calls {
		byLine[call.Line] = call
	}
	for start := 1; start < fixtureMsgCount; start++ {
		got, err := (&OpenCodeAgent{}).AttributeTokens(data, start, "")
		if err != nil {
			t.Fatalf("AttributeTokens(startLine=%d): %v", start, err)
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

func TestAttributeTokens_StartEndAndCost(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if !got.Start.Equal(fixtureTime(fixtureUser0Created)) {
		t.Errorf("Start = %v, want the first user message's created time", got.Start)
	}
	if !got.End.Equal(fixtureTime(fixtureAssistant6Completed)) {
		t.Errorf("End = %v, want the last message's completed time", got.End)
	}
	if math.Abs(got.AgentReportedCost-fixtureCostSum) > 1e-9 {
		t.Errorf("AgentReportedCost = %v, want %v (Σ info.cost)", got.AgentReportedCost, fixtureCostSum)
	}
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none", got.Subagents)
	}
}

func TestAttributeTokens_StartLinePastEnd(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureMsgCount)
	if len(got.Calls) != 0 || !got.Start.IsZero() || !got.End.IsZero() || got.AgentReportedCost != 0 {
		t.Errorf("startLine past the end = %+v, want an empty Attribution", got)
	}
}

func TestAttributeTokens_EmptyTranscript(t *testing.T) {
	t.Parallel()

	got, err := (&OpenCodeAgent{}).AttributeTokens(nil, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got.Calls) != 0 {
		t.Errorf("empty transcript = %+v, want a non-nil empty Attribution", got)
	}
}

func TestAttributeTokens_MalformedExportIsError(t *testing.T) {
	t.Parallel()

	got, err := (&OpenCodeAgent{}).AttributeTokens([]byte(`{"info":{},"messages":[`), 0, "")
	if err == nil {
		t.Fatalf("malformed export returned %+v, want an error", got)
	}
}

// TestAttributeTokens_ToolPartWithoutState: a tool part that never ran keeps
// its id and name, has an empty Detail, and is a zero-byte result.
func TestAttributeTokens_ToolPartWithoutState(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"s"},"messages":[
		{"info":{"id":"m1","role":"assistant","time":{"created":1},"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0},"total":2}},
		 "parts":[{"id":"prt_1","type":"tool","tool":"bash"}]},
		{"info":{"id":"m2","role":"assistant","time":{"created":2},"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0},"total":2}},
		 "parts":[]}]}`)
	got, err := (&OpenCodeAgent{}).AttributeTokens(data, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := types.ToolUseRef{ID: "prt_1", Tool: "bash"}
	if len(got.Calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(got.Calls))
	}
	assertSame(t, "Calls[0].Emitted", got.Calls[0].Emitted, []types.ToolUseRef{want})
	assertSame(t, "Calls[1].Consumed", got.Calls[1].Consumed, []types.ToolResultRef{{ToolUse: want}})
}
