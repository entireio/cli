package geminicli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Message indices in testdata/attribution_session.json — the coordinate
// AttributeTokens' startLine and CallUsage.Line share with
// CalculateTokenUsage's fromOffset. The fixture is synthetic, modelled on the
// Gemini CLI session shape in transcript/compact/testdata/gemini_full.jsonl
// (2026-08-28): every message carries a timestamp, every gemini message a
// model and (usually) a six-key tokens block, and each toolCall holds its own
// result as a functionResponse array.
const (
	fixtureMsgUser0   = 0
	fixtureMsgGemini1 = 1 // run_shell_command + read_file; cached, thoughts and tool > 0
	fixtureMsgInfo2   = 2 // info message: never a call
	fixtureMsgGemini3 = 3 // text only
	fixtureMsgUser4   = 4
	fixtureMsgGemini5 = 5 // glob + a file_path-keyed read_file; no tokens block
	fixtureMsgGemini6 = 6 // text only, a different model
	fixtureMsgCount   = 7
)

// messageTypeInfo is Gemini's system-notice message type ("Model switched to
// …"); AttributeTokens never treats it as a call.
const messageTypeInfo = "info"

const (
	fixtureModelPro   = "gemini-2.5-pro"
	fixtureModelFlash = "gemini-2.5-flash"

	fixtureUser0At   = "2026-03-18T21:05:13.497Z"
	fixtureGemini1At = "2026-03-18T21:05:20.932Z"
	fixtureGemini3At = "2026-03-18T21:05:47.368Z"
	fixtureGemini5At = "2026-03-18T21:06:01.208Z"
	fixtureGemini6At = "2026-03-18T21:08:23.669Z"

	// Compact JSON of each toolCall's `result` array, exactly as the fixture
	// writes it minus indentation; Bytes is its length.
	fixtureShellResult = `[{"functionResponse":{"id":"run_shell_command-1773867920932-1","name":"run_shell_command","response":{"output":"ok\tgithub.com/x/src\t0.012s"}}}]`
	fixtureReadResult  = `[{"functionResponse":{"id":"read_file-1773867920932-2","name":"read_file","response":{"output":"package src\n"}}}]`
	fixtureGlobResult  = `[{"functionResponse":{"id":"glob-1773867961208-3","name":"glob","response":{"output":"Found 1 file(s) matching \"**/*.go\":\nsrc/a.go"}}}]`
	fixtureReadBResult = `[{"functionResponse":{"id":"read_file-1773867961208-4","name":"read_file","response":{"output":"package src // b\n"}}}]`
)

// Per-call usage from the CalculateTokenUsage identities: input − cached +
// tool, output + thoughts, thoughts, cached.
var (
	fixtureCall1Usage = types.TokenUsage{InputTokens: 500, OutputTokens: 100, ThinkingTokens: 60, CacheReadTokens: 800, APICallCount: 1}
	fixtureCall3Usage = types.TokenUsage{InputTokens: 100, OutputTokens: 100, ThinkingTokens: 20, CacheReadTokens: 1400, APICallCount: 1}
	fixtureCall6Usage = types.TokenUsage{InputTokens: 2050, OutputTokens: 30, APICallCount: 1}

	fixtureShellRef = types.ToolUseRef{ID: "run_shell_command-1773867920932-1", Tool: "run_shell_command", Detail: "go test ./..."}
	// fixtureReadRef is the older `absolute_path` spelling (fallback);
	// fixtureReadBRef the `file_path` key current Gemini CLI writes.
	fixtureReadRef  = types.ToolUseRef{ID: "read_file-1773867920932-2", Tool: "read_file", Detail: "src/a.go"}
	fixtureReadBRef = types.ToolUseRef{ID: "read_file-1773867961208-4", Tool: "read_file", Detail: "src/b.go"}
	fixtureGlobRef  = types.ToolUseRef{ID: "glob-1773867961208-3", Tool: "glob"}

	// Consumed by the gemini message AFTER the one that emitted them.
	fixtureMsg1Results = []types.ToolResultRef{
		{ToolUse: fixtureShellRef, Bytes: len(fixtureShellResult)},
		{ToolUse: fixtureReadRef, Bytes: len(fixtureReadResult)},
	}
	fixtureMsg5Results = []types.ToolResultRef{
		{ToolUse: fixtureGlobRef, Bytes: len(fixtureGlobResult)},
		{ToolUse: fixtureReadBRef, Bytes: len(fixtureReadBResult)},
	}
)

func readAttributionFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "attribution_session.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func attributeFixture(t *testing.T, startLine int) *types.Attribution {
	t.Helper()
	got, err := (&GeminiCLIAgent{}).AttributeTokens(readAttributionFixture(t), startLine, "")
	if err != nil {
		t.Fatalf("AttributeTokens(startLine=%d): %v", startLine, err)
	}
	if got == nil {
		t.Fatalf("AttributeTokens(startLine=%d) returned nil Attribution", startLine)
	}
	return got
}

func fixtureTime(t *testing.T, ts string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("parse fixture timestamp %q: %v", ts, err)
	}
	return at
}

func assertRefs(t *testing.T, name string, got, want []types.ToolUseRef) {
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

func assertConsumed(t *testing.T, name string, got, want []types.ToolResultRef) {
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

// TestAttributionFixtureLayout pins the message types and the token-block
// presence every index assertion below depends on.
func TestAttributionFixtureLayout(t *testing.T) {
	t.Parallel()

	session, err := parseAttributionSession(readAttributionFixture(t))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(session.Messages) != fixtureMsgCount {
		t.Fatalf("fixture has %d messages, want %d", len(session.Messages), fixtureMsgCount)
	}
	wantTypes := map[int]string{
		fixtureMsgUser0: MessageTypeUser, fixtureMsgGemini1: MessageTypeGemini, fixtureMsgInfo2: messageTypeInfo,
		fixtureMsgGemini3: MessageTypeGemini, fixtureMsgUser4: MessageTypeUser,
		fixtureMsgGemini5: MessageTypeGemini, fixtureMsgGemini6: MessageTypeGemini,
	}
	for i, want := range wantTypes {
		if got := session.Messages[i].Type; got != want {
			t.Errorf("message %d type = %q, want %q", i, got, want)
		}
	}
	if session.Messages[fixtureMsgGemini5].Tokens != nil {
		t.Errorf("message %d should record no tokens block", fixtureMsgGemini5)
	}
	if session.Messages[fixtureMsgGemini1].Tokens == nil || session.Messages[fixtureMsgGemini1].Tokens.Cached == 0 ||
		session.Messages[fixtureMsgGemini1].Tokens.Thoughts == 0 || session.Messages[fixtureMsgGemini1].Tokens.Tool == 0 {
		t.Errorf("message %d tokens = %+v, want cached, thoughts and tool all > 0 so every identity term is exercised", fixtureMsgGemini1, session.Messages[fixtureMsgGemini1].Tokens)
	}
	if got := len(session.Messages[fixtureMsgGemini1].ToolCalls); got != 2 {
		t.Errorf("message %d has %d toolCalls, want 2", fixtureMsgGemini1, got)
	}
	if _, ok := session.Messages[fixtureMsgGemini1].ToolCalls[1].Args[argAbsolutePath]; !ok {
		t.Errorf("message %d read_file args = %v, want the older %q key so the fallback is exercised", fixtureMsgGemini1, session.Messages[fixtureMsgGemini1].ToolCalls[1].Args, argAbsolutePath)
	}
	if got := len(session.Messages[fixtureMsgGemini5].ToolCalls); got != 2 {
		t.Fatalf("message %d has %d toolCalls, want 2", fixtureMsgGemini5, got)
	}
	if _, ok := session.Messages[fixtureMsgGemini5].ToolCalls[1].Args["file_path"]; !ok {
		t.Errorf("message %d read_file args = %v, want the \"file_path\" key current Gemini CLI writes", fixtureMsgGemini5, session.Messages[fixtureMsgGemini5].ToolCalls[1].Args)
	}
}

func TestAttributeTokens_OneCallPerGeminiMessage(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4 (the info message is not a call): %+v", len(got.Calls), got.Calls)
	}
	wantLines := []int{fixtureMsgGemini1, fixtureMsgGemini3, fixtureMsgGemini5, fixtureMsgGemini6}
	wantAt := []string{fixtureGemini1At, fixtureGemini3At, fixtureGemini5At, fixtureGemini6At}
	wantModel := []string{fixtureModelPro, fixtureModelPro, fixtureModelPro, fixtureModelFlash}
	for i, call := range got.Calls {
		if call.Line != wantLines[i] {
			t.Errorf("Calls[%d].Line = %d, want message index %d", i, call.Line, wantLines[i])
		}
		if want := fixtureTime(t, wantAt[i]); !call.At.Equal(want) {
			t.Errorf("Calls[%d].At = %v, want %v (the message timestamp)", i, call.At, want)
		}
		if call.Model != wantModel[i] {
			t.Errorf("Calls[%d].Model = %q, want the message's model %q", i, call.Model, wantModel[i])
		}
		if call.Effort != "" || call.ActiveSkill != "" {
			t.Errorf("Calls[%d] Effort/ActiveSkill = %q/%q, want empty (Gemini records neither)", i, call.Effort, call.ActiveSkill)
		}
	}
}

func TestAttributeTokens_UsagePerCall(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	// Each class of message 1 asserted on its own so a broken identity names
	// the term: input 1200 − cached 800 + tool 100; output 40 + thoughts 60.
	u := got.Calls[0].Usage
	if got.Calls[0].UsageUnknown {
		t.Errorf("Calls[0].UsageUnknown = true, want measured usage")
	}
	if u.InputTokens != fixtureCall1Usage.InputTokens {
		t.Errorf("Calls[0].InputTokens = %d, want %d (input − cached + tool)", u.InputTokens, fixtureCall1Usage.InputTokens)
	}
	if u.OutputTokens != fixtureCall1Usage.OutputTokens {
		t.Errorf("Calls[0].OutputTokens = %d, want %d (output + thoughts)", u.OutputTokens, fixtureCall1Usage.OutputTokens)
	}
	if u.ThinkingTokens != fixtureCall1Usage.ThinkingTokens {
		t.Errorf("Calls[0].ThinkingTokens = %d, want %d (thoughts)", u.ThinkingTokens, fixtureCall1Usage.ThinkingTokens)
	}
	if u.CacheReadTokens != fixtureCall1Usage.CacheReadTokens {
		t.Errorf("Calls[0].CacheReadTokens = %d, want %d (cached)", u.CacheReadTokens, fixtureCall1Usage.CacheReadTokens)
	}
	if u.CacheCreationTokens != 0 {
		t.Errorf("Calls[0].CacheCreationTokens = %d, want 0 (Gemini records no cache writes)", u.CacheCreationTokens)
	}
	if u.APICallCount != 1 {
		t.Errorf("Calls[0].APICallCount = %d, want 1", u.APICallCount)
	}
	if u != fixtureCall1Usage {
		t.Errorf("Calls[0].Usage = %+v, want %+v", u, fixtureCall1Usage)
	}

	if got.Calls[1].Usage != fixtureCall3Usage || got.Calls[1].UsageUnknown {
		t.Errorf("Calls[1].Usage = %+v (unknown=%v), want %+v", got.Calls[1].Usage, got.Calls[1].UsageUnknown, fixtureCall3Usage)
	}
	if !got.Calls[2].UsageUnknown || got.Calls[2].Usage != (types.TokenUsage{}) {
		t.Errorf("Calls[2] = %+v, want UsageUnknown with zero Usage (no tokens block)", got.Calls[2])
	}
	if got.Calls[3].Usage != fixtureCall6Usage || got.Calls[3].UsageUnknown {
		t.Errorf("Calls[3].Usage = %+v (unknown=%v), want %+v (tool tokens are fresh input)", got.Calls[3].Usage, got.Calls[3].UsageUnknown, fixtureCall6Usage)
	}
}

// TestAttributeTokens_SumsMatchCalculateTokenUsage guards the shared
// arithmetic: the per-call usages add up to the session total for slices
// starting at a user message, a gemini message, and the tokenless message.
func TestAttributeTokens_SumsMatchCalculateTokenUsage(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	ag := &GeminiCLIAgent{}
	for _, start := range []int{0, fixtureMsgGemini3, fixtureMsgGemini5} {
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

// TestAttributeTokens_EmittedRefs: run_shell_command reduces to the command
// head, read_file to its path under either the current `file_path` key or
// the older `absolute_path` fallback, glob to "" (its pattern is user
// content).
func TestAttributeTokens_EmittedRefs(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	assertRefs(t, "Calls[0].Emitted", got.Calls[0].Emitted, []types.ToolUseRef{fixtureShellRef, fixtureReadRef})
	if len(got.Calls[1].Emitted) != 0 {
		t.Errorf("Calls[1].Emitted = %+v, want none (text only)", got.Calls[1].Emitted)
	}
	assertRefs(t, "Calls[2].Emitted", got.Calls[2].Emitted, []types.ToolUseRef{fixtureGlobRef, fixtureReadBRef})
	if len(got.Calls[3].Emitted) != 0 {
		t.Errorf("Calls[3].Emitted = %+v, want none (text only)", got.Calls[3].Emitted)
	}
	for i, call := range got.Calls {
		for j, ref := range call.Emitted {
			if ref.SkillName != "" || ref.SubagentType != "" || ref.Model != "" {
				t.Errorf("Calls[%d].Emitted[%d] = %+v, want no skill/subagent/model (activate_skill and delegate_to_agent are not labelled yet)", i, j, ref)
			}
		}
	}
}

// TestAttributeTokens_ResultsConsumedByNextCall: a toolCall's result lives on
// the emitting message, so it is charged to the NEXT gemini message — across
// an intervening info or user message, and from a call that recorded no
// usage. Bytes is the compacted `result` JSON, not the indented file bytes.
func TestAttributeTokens_ResultsConsumedByNextCall(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if len(got.Calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(got.Calls))
	}
	if len(got.Calls[0].Consumed) != 0 {
		t.Errorf("Calls[0].Consumed = %+v, want none (nothing precedes it)", got.Calls[0].Consumed)
	}
	assertConsumed(t, "Calls[1].Consumed", got.Calls[1].Consumed, fixtureMsg1Results)
	if len(got.Calls[2].Consumed) != 0 {
		t.Errorf("Calls[2].Consumed = %+v, want none (message %d emitted no tools)", got.Calls[2].Consumed, fixtureMsgGemini3)
	}
	assertConsumed(t, "Calls[3].Consumed", got.Calls[3].Consumed, fixtureMsg5Results)
}

func TestAttributeTokens_StartLineIsMessageIndex(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureMsgGemini3)
	if len(got.Calls) != 3 {
		t.Fatalf("got %d calls from message %d, want 3: %+v", len(got.Calls), fixtureMsgGemini3, got.Calls)
	}
	if got.Calls[0].Line != fixtureMsgGemini3 {
		t.Errorf("Calls[0].Line = %d, want %d", got.Calls[0].Line, fixtureMsgGemini3)
	}
	// The pre-slice message's results are still charged to the first call
	// that read them, fully labelled.
	assertConsumed(t, "Calls[0].Consumed", got.Calls[0].Consumed, fixtureMsg1Results)
	if want := fixtureTime(t, fixtureGemini3At); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want message %d's timestamp", got.Start, fixtureMsgGemini3)
	}
	if want := fixtureTime(t, fixtureGemini6At); !got.End.Equal(want) {
		t.Errorf("End = %v, want message %d's timestamp", got.End, fixtureMsgGemini6)
	}
}

func TestAttributeTokens_StartEndSpanEveryMessageType(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, 0)
	if want := fixtureTime(t, fixtureUser0At); !got.Start.Equal(want) {
		t.Errorf("Start = %v, want the first user message's timestamp", got.Start)
	}
	if want := fixtureTime(t, fixtureGemini6At); !got.End.Equal(want) {
		t.Errorf("End = %v, want the last message's timestamp", got.End)
	}
	if len(got.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none (Gemini writes no child transcripts)", got.Subagents)
	}
	if got.AgentReportedCost != 0 {
		t.Errorf("AgentReportedCost = %v, want 0 (Gemini records no cost)", got.AgentReportedCost)
	}
}

// TestAttributeTokens_SubagentsDirIgnored: a subagentsDir changes nothing —
// Gemini writes no child transcripts and delegate_to_agent is not yet
// recognised.
func TestAttributeTokens_SubagentsDirIgnored(t *testing.T) {
	t.Parallel()

	got, err := (&GeminiCLIAgent{}).AttributeTokens(readAttributionFixture(t), 0, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Subagents) != 0 || len(got.Calls) != 4 {
		t.Errorf("with subagentsDir: %d subagents, %d calls; want 0 and 4", len(got.Subagents), len(got.Calls))
	}
}

// TestAttributeTokens_CallsIndependentOfStartLine: a call is the same whole
// struct whatever startLine admits it — Line, usage, Emitted and, in
// particular, Consumed — so consecutive slices charge each result exactly
// once and never shift it to a different call; and a slice holds exactly the
// offset-0 calls at or after it.
func TestAttributeTokens_CallsIndependentOfStartLine(t *testing.T) {
	t.Parallel()

	data := readAttributionFixture(t)
	full := attributeFixture(t, 0)
	byLine := make(map[int]types.CallUsage, len(full.Calls))
	for _, call := range full.Calls {
		byLine[call.Line] = call
	}
	for start := 1; start < fixtureMsgCount; start++ {
		got, err := (&GeminiCLIAgent{}).AttributeTokens(data, start, "")
		if err != nil {
			t.Fatalf("AttributeTokens(startLine=%d): %v", start, err)
		}
		wantCount := 0
		for _, call := range full.Calls {
			if call.Line >= start {
				wantCount++
			}
		}
		if len(got.Calls) != wantCount {
			t.Errorf("startLine %d: got %d calls, want %d (the offset-0 calls with Line >= %d)", start, len(got.Calls), wantCount, start)
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

func TestAttributeTokens_StartLinePastEnd(t *testing.T) {
	t.Parallel()

	got := attributeFixture(t, fixtureMsgCount)
	if len(got.Calls) != 0 || !got.Start.IsZero() || !got.End.IsZero() {
		t.Errorf("startLine past the end = %+v, want an empty Attribution", got)
	}
}

func TestAttributeTokens_EmptyTranscript(t *testing.T) {
	t.Parallel()

	for _, data := range [][]byte{nil, []byte("  \n")} {
		got, err := (&GeminiCLIAgent{}).AttributeTokens(data, 0, "")
		if err != nil {
			t.Fatalf("AttributeTokens(%q): unexpected error: %v", data, err)
		}
		if got == nil || len(got.Calls) != 0 {
			t.Errorf("AttributeTokens(%q) = %+v, want a non-nil empty Attribution", data, got)
		}
	}
}

func TestAttributeTokens_MalformedTranscriptIsError(t *testing.T) {
	t.Parallel()

	got, err := (&GeminiCLIAgent{}).AttributeTokens([]byte(`{"sessionId":"s","messages":[`), 0, "")
	if err == nil {
		t.Fatalf("malformed transcript returned %+v, want an error", got)
	}
}

// TestAttributeTokens_ToolCallWithoutResult pins the defensive path: every
// real toolCall carries a result (cancelled and errored ones included), but
// one without keeps its id and name and is a zero-byte result for the next
// call; a missing or unparsable timestamp leaves At zero.
func TestAttributeTokens_ToolCallWithoutResult(t *testing.T) {
	t.Parallel()

	data := []byte(`{"sessionId":"s","messages":[
		{"id":"m1","type":"gemini","timestamp":"not-a-time","tokens":{"input":1,"output":1,"cached":0,"thoughts":0,"tool":0,"total":2},
		 "toolCalls":[{"id":"tc1","name":"run_shell_command","args":{"command":"go build ./..."},"status":"cancelled"}]},
		{"id":"m2","type":"gemini","tokens":{"input":1,"output":1,"cached":0,"thoughts":0,"tool":0,"total":2}}]}`)
	got, err := (&GeminiCLIAgent{}).AttributeTokens(data, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(got.Calls))
	}
	want := types.ToolUseRef{ID: "tc1", Tool: "run_shell_command", Detail: "go build ./..."}
	assertRefs(t, "Calls[0].Emitted", got.Calls[0].Emitted, []types.ToolUseRef{want})
	assertConsumed(t, "Calls[1].Consumed", got.Calls[1].Consumed, []types.ToolResultRef{{ToolUse: want}})
	if !got.Calls[0].At.IsZero() || !got.Calls[1].At.IsZero() {
		t.Errorf("At = %v / %v, want zero for an unparsable and a missing timestamp", got.Calls[0].At, got.Calls[1].At)
	}
	if got.Calls[0].Model != "" {
		t.Errorf("Calls[0].Model = %q, want \"\" when the message records none", got.Calls[0].Model)
	}
	if !got.Start.IsZero() || !got.End.IsZero() {
		t.Errorf("Start/End = %v/%v, want zero with no parsable timestamp", got.Start, got.End)
	}
}
