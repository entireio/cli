package grok

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// fixture returns a golden hook payload captured from a real Grok 1.0.5
// session (see AGENT.md).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func parse(t *testing.T, hookName, fixtureName string) *agent.Event {
	t.Helper()
	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), hookName, bytes.NewReader(fixture(t, fixtureName)))
	if err != nil {
		t.Fatalf("ParseHookEvent(%s): %v", hookName, err)
	}
	return ev
}

func TestParseHookEvent_EventTypeMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hook     string
		fixture  string
		wantType agent.EventType
	}{
		{"session start", HookNameSessionStart, "session_start.json", agent.SessionStart},
		{"prompt submit", HookNameUserPromptSubmit, "user_prompt_submit.json", agent.TurnStart},
		{"turn stop", HookNameStop, "stop_turn.json", agent.TurnEnd},
		{"session end", HookNameSessionEnd, "session_end.json", agent.SessionEnd},
		{"tool use", HookNamePostToolUse, "post_tool_use.json", agent.ToolUse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := parse(t, tt.hook, tt.fixture)
			if ev == nil {
				t.Fatalf("%s: got nil event, want %v", tt.hook, tt.wantType)
			}
			if ev.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", ev.Type, tt.wantType)
			}
			if ev.SessionID != "01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e" {
				t.Errorf("SessionID = %q", ev.SessionID)
			}
		})
	}
}

// TestParseHookEvent_TeardownStopIsNotATurn locks in the discriminator between
// Grok's two Stop invocations. Both fixtures are real payloads from a single
// session: the turn-scoped one and the teardown one that follows SessionEnd.
// Without this split every Grok session mints a duplicate, contentless
// checkpoint.
func TestParseHookEvent_TeardownStopIsNotATurn(t *testing.T) {
	t.Parallel()

	if ev := parse(t, HookNameStop, "stop_turn.json"); ev == nil || ev.Type != agent.TurnEnd {
		t.Fatalf("turn stop: got %v, want TurnEnd", ev)
	}
	if ev := parse(t, HookNameStop, "stop_teardown.json"); ev != nil {
		t.Errorf("teardown stop: got %v, want nil (no lifecycle action)", ev.Type)
	}
}

func TestIsTeardownStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		reason   string
		promptID string
		want     bool
	}{
		{"real turn", "end_turn", "p-1", false},
		{"shutdown", "shutdown", "", true},
		{"channel closed", "channel_closed", "", true},
		{"missing prompt id", "end_turn", "", true},
		{"shutdown with prompt id", "shutdown", "p-1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTeardownStop(tt.reason, tt.promptID); got != tt.want {
				t.Errorf("isTeardownStop(%q, %q) = %v, want %v", tt.reason, tt.promptID, got, tt.want)
			}
		})
	}
}

// TestParseHookEvent_CancelledAndFailedTurnsEndTheTurn guards the mapping that
// is easiest to omit: Grok fires StopCancelled/StopFailure INSTEAD of Stop, so
// without these an interrupted or rate-limited turn is never checkpointed.
func TestParseHookEvent_CancelledAndFailedTurnsEndTheTurn(t *testing.T) {
	t.Parallel()

	cancelled := []byte(`{"hookEventName":"stop_cancelled","sessionId":"s1","promptId":"p1",
		"transcriptPath":"/tmp/updates.jsonl","reason":"user_interrupt","cancelledBy":"user","cancelTrigger":"ctrl_c"}`)
	failed := []byte(`{"hookEventName":"stop_failure","sessionId":"s1","promptId":"p1",
		"transcriptPath":"/tmp/updates.jsonl","error":"rate_limit"}`)

	g := &GrokAgent{}
	for _, tc := range []struct {
		hook string
		body []byte
	}{
		{HookNameStopCancelled, cancelled},
		{HookNameStopFailure, failed},
	} {
		ev, err := g.ParseHookEvent(context.Background(), tc.hook, bytes.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.hook, err)
		}
		if ev == nil || ev.Type != agent.TurnEnd {
			t.Errorf("%s: got %v, want TurnEnd", tc.hook, ev)
		}
	}
}

func TestParseHookEvent_SubagentStopIsFinal(t *testing.T) {
	t.Parallel()

	body := []byte(`{"hookEventName":"subagent_stop","sessionId":"s1","transcriptPath":"/tmp/u.jsonl","subagentType":"worker","phase":"end"}`)
	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), HookNameSubagentStop, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("subagent stop: %v", err)
	}
	if ev == nil || ev.Type != agent.SubagentEnd {
		t.Fatalf("got %v, want SubagentEnd", ev)
	}
	// Grok fires SubagentStop once, at true completion — there is no launch
	// stub to disambiguate from, so Final must be set.
	if !ev.Final {
		t.Error("Final = false, want true (Grok has no separate launch stub)")
	}
}

func TestParseHookEvent_UnknownHookReturnsNil(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), "not-a-real-hook", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev != nil {
		t.Errorf("got %v, want nil", ev.Type)
	}
}

func TestParseHookEvent_MalformedJSONErrors(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	if _, err := g.ParseHookEvent(context.Background(), HookNameStop, bytes.NewReader([]byte(`{not json`))); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

// TestParseHookEvent_TranscriptPathIsUsed pins the field name. Grok spells it
// camelCase; binding snake_case (as the Claude Code payload does) silently
// yields an empty transcript path and a checkpoint with no content.
func TestParseHookEvent_TranscriptPathIsUsed(t *testing.T) {
	t.Parallel()

	ev := parse(t, HookNameStop, "stop_turn.json")
	if ev == nil {
		t.Fatal("nil event")
	}
	if ev.SessionRef == "" {
		t.Fatal("SessionRef is empty; transcriptPath was not parsed")
	}
	if filepath.Base(ev.SessionRef) != transcriptFileName {
		t.Errorf("SessionRef = %q, want it to end in %s", ev.SessionRef, transcriptFileName)
	}
}

func TestParseToolUse_ExtractsAbsolutePath(t *testing.T) {
	t.Parallel()

	ev := parse(t, HookNamePostToolUse, "post_tool_use.json")
	if ev == nil {
		t.Fatal("nil event")
	}
	if len(ev.ModifiedFiles) != 1 {
		t.Fatalf("ModifiedFiles = %v, want exactly one", ev.ModifiedFiles)
	}
	// The result's absolute_path is preferred over toolInput's relative
	// file_path, so the framework does not have to guess a base directory.
	if !filepath.IsAbs(ev.ModifiedFiles[0]) {
		t.Errorf("ModifiedFiles[0] = %q, want an absolute path", ev.ModifiedFiles[0])
	}
	if filepath.Base(ev.ModifiedFiles[0]) != "hello.txt" {
		t.Errorf("ModifiedFiles[0] = %q, want basename hello.txt", ev.ModifiedFiles[0])
	}
}

func TestParseToolUse_NoFileTouchedReturnsNil(t *testing.T) {
	t.Parallel()

	body := []byte(`{"hookEventName":"post_tool_use","sessionId":"s1","toolName":"run_terminal_command","toolInput":{"command":"ls"}}`)
	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), HookNamePostToolUse, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev != nil {
		t.Errorf("got %v, want nil for a tool that touched no file", ev.Type)
	}
}

func TestUnwrapPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips the envelope",
			in:   "<user_query>\nCreate a file named hello.txt.\n</user_query>",
			want: "Create a file named hello.txt.",
		},
		{
			name: "leaves an unwrapped prompt alone",
			in:   "Create a file named hello.txt.",
			want: "Create a file named hello.txt.",
		},
		{
			name: "keeps context outside the envelope",
			in:   "Context line\n<user_query>\nDo the thing.\n</user_query>\nTrailing note",
			want: "Context line\n\nDo the thing.\n\nTrailing note",
		},
		{
			name: "unterminated envelope is left alone",
			in:   "<user_query>\nDo the thing.",
			want: "<user_query>\nDo the thing.",
		},
		{
			name: "empty envelope falls back to the original",
			in:   "<user_query></user_query>",
			want: "<user_query></user_query>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := unwrapPrompt(tt.in); got != tt.want {
				t.Errorf("unwrapPrompt(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseTurnStart_PromptIsUnwrapped ties the unwrapping to the real payload:
// the captured prompt really does arrive inside <user_query>.
func TestParseTurnStart_PromptIsUnwrapped(t *testing.T) {
	t.Parallel()

	ev := parse(t, HookNameUserPromptSubmit, "user_prompt_submit.json")
	if ev == nil {
		t.Fatal("nil event")
	}
	if ev.Prompt == "" {
		t.Fatal("Prompt is empty")
	}
	if got := ev.Prompt; got[0] == '<' {
		t.Errorf("Prompt still wrapped: %q", got)
	}
	if want := "Create a file named hello.txt containing exactly the word hello. Then stop."; ev.Prompt != want {
		t.Errorf("Prompt = %q, want %q", ev.Prompt, want)
	}
}

// TestSessionStart_UsesCWDNotWorkspaceRoot pins which field names the session
// group directory.
//
// Grok sends both, and they differ by a trailing slash: cwd is "/repo",
// workspaceRoot is "/repo/". The group directory is named from the unslashed
// form, so encoding workspaceRoot yields a name ending in %2F that matches no
// session on disk. session_start is the only event without transcriptPath, so
// it is the only place this derivation is used — and the only place it can go
// wrong silently.
func TestSessionStart_UsesCWDNotWorkspaceRoot(t *testing.T) {
	t.Setenv("GROK_HOME", filepath.Join("/tmp", "grokhome"))

	body := []byte(`{"hookEventName":"session_start","sessionId":"01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e",` +
		`"cwd":"/repo/project","workspaceRoot":"/repo/project/","source":"new"}`)

	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), HookNameSessionStart, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("nil event")
	}

	want := g.ResolveSessionFile(
		filepath.Join("/tmp", "grokhome", "sessions", "%2Frepo%2Fproject"),
		"01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e")
	if ev.SessionRef != want {
		t.Errorf("SessionRef = %q\nwant             %q", ev.SessionRef, want)
	}
	// The failure this guards: a group directory ending in an encoded slash.
	group := filepath.Base(filepath.Dir(filepath.Dir(ev.SessionRef)))
	if strings.HasSuffix(group, "%2F") {
		t.Errorf("group %q ends in %%2F; workspaceRoot's trailing slash leaked in", group)
	}
}

// TestSessionStart_FallsBackToWorkspaceRoot covers a payload carrying only
// workspaceRoot, where the trailing slash must still be trimmed.
func TestSessionStart_FallsBackToWorkspaceRoot(t *testing.T) {
	t.Setenv("GROK_HOME", filepath.Join("/tmp", "grokhome"))

	body := []byte(`{"hookEventName":"session_start","sessionId":"01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e",` +
		`"workspaceRoot":"/repo/project/","source":"new"}`)

	g := &GrokAgent{}
	ev, err := g.ParseHookEvent(context.Background(), HookNameSessionStart, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("nil event")
	}
	group := filepath.Base(filepath.Dir(filepath.Dir(ev.SessionRef)))
	if group != "%2Frepo%2Fproject" {
		t.Errorf("group = %q, want %%2Frepo%%2Fproject", group)
	}
}

// TestSessionStart_RejectsUnsafeSessionID guards the path-traversal class of
// bug (GHSA-2h46): session_start is the one hook without transcriptPath, so
// its sessionId becomes a directory component. An ID carrying separators or
// a reserved segment must yield no SessionRef rather than a path that escapes
// the session group.
func TestSessionStart_RejectsUnsafeSessionID(t *testing.T) {
	t.Setenv("GROK_HOME", filepath.Join("/tmp", "grokhome"))

	for _, id := range []string{"../../etc/passwd", "..", "a/b", `a\b`, "C:evil", "-flag", "sess*"} {
		t.Run(id, func(t *testing.T) {
			body := []byte(`{"hookEventName":"session_start","sessionId":` + strconv.Quote(id) + `,` +
				`"cwd":"/repo/project","source":"new"}`)

			g := &GrokAgent{}
			ev, err := g.ParseHookEvent(context.Background(), HookNameSessionStart, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("ParseHookEvent: %v", err)
			}
			if ev == nil {
				t.Fatal("nil event")
			}
			if ev.SessionRef != "" {
				t.Errorf("SessionRef = %q, want empty for unsafe session ID", ev.SessionRef)
			}
		})
	}
}
