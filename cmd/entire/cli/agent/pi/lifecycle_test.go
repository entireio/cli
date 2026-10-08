package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"session_start","cwd":"/repo","session_file":"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl"}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameSessionStart, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev.Type != agent.SessionStart {
		t.Errorf("Type = %v", ev.Type)
	}
	if ev.SessionID != "abc-123" {
		t.Errorf("SessionID = %q (want abc-123 extracted from filename)", ev.SessionID)
	}
}

func TestParseHookEvent_BeforeAgentStart(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"before_agent_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl","prompt":"do thing"}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameBeforeAgentStart, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev.Type != agent.TurnStart {
		t.Errorf("Type = %v, want TurnStart", ev.Type)
	}
	if ev.Prompt != "do thing" {
		t.Errorf("Prompt = %q", ev.Prompt)
	}
	if ev.SessionRef != "/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl" {
		t.Errorf("SessionRef = %q", ev.SessionRef)
	}
}

func TestParseHookEvent_DoesNotWriteActiveSessionCache(t *testing.T) {
	tests := []struct {
		name     string
		hookName string
		payload  string
	}{
		{
			name:     "session start",
			hookName: HookNameSessionStart,
			payload:  `{"type":"session_start","session_id":"abc-123"}`,
		},
		{
			name:     "before agent start",
			hookName: HookNameBeforeAgentStart,
			payload:  `{"type":"before_agent_start","session_id":"abc-123","prompt":"do thing"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			if _, err := (&PiAgent{}).ParseHookEvent(
				context.Background(), tt.hookName, strings.NewReader(tt.payload),
			); err != nil {
				t.Fatalf("ParseHookEvent: %v", err)
			}

			cachePath := filepath.Join(dir, ".entire", "tmp", "pi", "pi-active-session")
			if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
				t.Fatalf("active-session cache exists after %s; stat error = %v", tt.hookName, err)
			}
		})
	}
}

func TestParseHookEvent_BeforeAgentStart_WithSkillEvent(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"before_agent_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl","prompt":"expanded skill","skill_events":[{"skill_name":"trigger-analysis","invocation":"/skill:trigger-analysis","timestamp":"2026-05-25T12:34:56Z"}]}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameBeforeAgentStart, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if len(ev.SkillEvents) != 1 {
		t.Fatalf("SkillEvents len = %d, want 1", len(ev.SkillEvents))
	}
	skillEvent := ev.SkillEvents[0]
	if skillEvent.EventType != agent.SkillEventTypePromptInvocation {
		t.Errorf("EventType = %q", skillEvent.EventType)
	}
	if skillEvent.Skill.Name != "trigger-analysis" {
		t.Errorf("Skill.Name = %q", skillEvent.Skill.Name)
	}
	if skillEvent.Source.Signal != agent.SkillSignalPiInputSlashCommand {
		t.Errorf("Source.Signal = %q", skillEvent.Source.Signal)
	}
	if skillEvent.Collapse.Target != agent.SkillCollapseTargetUserMessage || !skillEvent.Collapse.DefaultCollapsed {
		t.Errorf("Collapse = %+v", skillEvent.Collapse)
	}
}

// TestParseHookEvent_BeforeAgentStart_MultipleSkillEvents locks live capture of
// every /skill:<name> in a turn (the path backing all live Pi sessions).
func TestParseHookEvent_BeforeAgentStart_MultipleSkillEvents(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"before_agent_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl","prompt":"expanded","skill_events":[{"skill_name":"review-pr","invocation":"/skill:review-pr","timestamp":"2026-05-25T12:34:56Z"},{"skill_name":"test-auditor","invocation":"/skill:test-auditor","timestamp":"2026-05-25T12:35:10Z"}]}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameBeforeAgentStart, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	got := make(map[string]bool)
	for _, se := range ev.SkillEvents {
		got[se.Skill.Name] = true
		if se.Source.Agent != string(agent.AgentNamePi) {
			t.Errorf("Source.Agent = %q, want pi", se.Source.Agent)
		}
	}
	for _, want := range []string{"review-pr", "test-auditor"} {
		if !got[want] {
			t.Errorf("missing live skill event for %q; got %v", want, got)
		}
	}
}

// TestPiAgent_UsesLiveSkillCaptureNotTranscriptExtraction guards Pi's
// live-capture model: a transcript extractor would double-count at
// condensation (see piSkillEvents).
func TestPiAgent_UsesLiveSkillCaptureNotTranscriptExtraction(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsSkillEventExtractor(NewPiAgent()); ok {
		t.Fatal("PiAgent must not implement SkillEventExtractor: Pi captures skills live; " +
			"a transcript extractor would double-count at condensation (see piSkillEvents)")
	}
}

// TestPiAgent_MustNotImplementToolInvocationScanner keeps Pi out of the
// tool-invocation probe. Pi's transcript has no walker here, so implementing
// the interface would turn "cannot tell" into a fabricated "did not run" —
// indistinguishable in aggregate from a real one (see
// agent.ToolInvocationScanner).
func TestPiAgent_MustNotImplementToolInvocationScanner(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsToolInvocationScanner(NewPiAgent()); ok {
		t.Fatal("PiAgent must not implement ToolInvocationScanner: no walker exists for Pi's " +
			"transcript, and reporting a fabricated negative is worse than reporting unsupported")
	}
}

func TestParseHookEvent_SessionShutdown_NoLifecycleEvent(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"session_shutdown","session_id":"explicit-id"}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameSessionShutdown, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	// session_shutdown is not a lifecycle event — see ParseHookEvent for the rationale.
	if ev != nil {
		t.Fatalf("expected nil event from session_shutdown, got %+v", ev)
	}
}

func TestParseHookEvent_EmptyStdin(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	_, err := a.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))
	if err == nil {
		t.Error("expected error on empty stdin")
	}
}

func TestParseHookEvent_UnknownHookYieldsNoEvent(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	// Anything not in HookNames() is treated as a no-op.
	ev, err := a.ParseHookEvent(context.Background(), "some_unknown_event", strings.NewReader(`{"type":"unknown"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", ev)
	}
}

func TestExtractSessionIDFromPath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"": "",
		"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl": "abc-123",
		"abc-123.jsonl":                             "abc-123",
		"/tmp/no-underscore-here.jsonl":             "no-underscore-here",
		"/path/with/multiple_under_scores_id.jsonl": "id",
	}
	for in, want := range cases {
		if got := extractSessionIDFromPath(in); got != want {
			t.Errorf("extractSessionIDFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCaptureTranscript(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	src := filepath.Join(dir, "src.jsonl")
	body := []byte(`{"type":"session","version":3}` + "\n")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}

	dst := captureTranscript(context.Background(), "abc-123", src)
	if dst == "" {
		t.Fatal("captureTranscript returned empty path")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("captured content mismatch")
	}
	if !strings.HasSuffix(dst, "abc-123.json") {
		t.Errorf("captured path = %q, expected .../abc-123.json", dst)
	}
}

func TestCaptureTranscript_MissingInputs(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	t.Chdir(t.TempDir())
	if got := captureTranscript(context.Background(), "", "/some/path"); got != "" {
		t.Errorf("empty session id should return empty, got %q", got)
	}
	if got := captureTranscript(context.Background(), "abc", ""); got != "" {
		t.Errorf("empty session file should return empty, got %q", got)
	}
}

// TestCaptureTranscript_RejectsTraversalSessionID verifies that captureTranscript
// refuses an unsafe session ID. captureTranscript runs inside ParseHookEvent,
// before the lifecycle dispatcher validates the ID, so it must guard the
// transcript write itself — otherwise a "../"-laden ID escapes the cache dir.
func TestCaptureTranscript_RejectsTraversalSessionID(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	src := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(src, []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A sentinel outside the cache dir that the traversal would target.
	victim := filepath.Join(dir, "victim.json")
	if err := os.WriteFile(victim, []byte("SAFE"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"../victim", "/etc/passwd", "..", "a/b"} {
		if got := captureTranscript(context.Background(), bad, src); got != "" {
			t.Errorf("captureTranscript(%q) = %q, want \"\" (unsafe ID must be refused)", bad, got)
		}
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SAFE" {
		t.Errorf("sentinel was overwritten via traversal: %q", string(got))
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()
	got := (&PiAgent{}).GetSupportedHooks()
	// Note: session_shutdown is not a HookSessionEnd source —
	// see ParseHookEvent's session_shutdown case for why.
	want := []agent.HookType{
		agent.HookSessionStart,
		agent.HookUserPromptSubmit,
		agent.HookStop,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d hooks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHookNamesMatchesParser(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	for _, name := range a.HookNames() {
		// All named hooks must accept a minimal payload without erroring.
		_, err := a.ParseHookEvent(context.Background(), name, strings.NewReader(`{}`))
		if err != nil {
			t.Errorf("ParseHookEvent(%q): %v", name, err)
		}
	}
}

func TestPiAgent_ContextInjector(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}

	// Pi injects model context at TurnStart (before_agent_start).
	if got := a.InjectionEvent(); got != agent.TurnStart {
		t.Errorf("InjectionEvent = %v, want TurnStart", got)
	}

	// Non-empty text renders a newline-terminated {"inject_context":...} line.
	out, err := a.RenderContextInjection(agent.ContextInjection{Text: "use entire trail"})
	if err != nil {
		t.Fatalf("RenderContextInjection: %v", err)
	}
	got := string(out)
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("payload must be newline-terminated, got %q", got)
	}
	if !strings.Contains(got, `"inject_context":"use entire trail"`) {
		t.Errorf("payload missing inject_context envelope: %q", got)
	}

	// Empty text renders nothing.
	out, err = a.RenderContextInjection(agent.ContextInjection{Text: "   "})
	if err != nil {
		t.Fatalf("RenderContextInjection(empty): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty text must render no payload, got %q", string(out))
	}
}

// TestParseHookEvent_SessionlessPayloadIsSkipped pins the guard that keeps a
// sessionless payload from borrowing another Pi process's identity.
//
// A Pi subagent is a nested `pi --no-session` process that auto-discovers the same
// project-local extension, so it fires these hooks carrying no session identity.
// The former single-slot fallback handed the child its parent's ID, and the nested
// turn then overwrote the parent's prompt and turn window.
func TestParseHookEvent_SessionlessPayloadIsSkipped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := &PiAgent{}

	tests := []struct {
		name          string
		hook          string
		stdin         string
		wantSessionID string // "" means: expect no event at all
	}{
		{
			name:  "nested session_start",
			hook:  HookNameSessionStart,
			stdin: `{"type":"session_start","cwd":"/repo"}`,
		},
		{
			name:  "nested before_agent_start",
			hook:  HookNameBeforeAgentStart,
			stdin: `{"type":"before_agent_start","cwd":"/repo","prompt":"Task: create docs/red.md"}`,
		},
		{
			name:  "nested agent_end",
			hook:  HookNameAgentEnd,
			stdin: `{"type":"agent_end","cwd":"/repo"}`,
		},
		{
			// A session file whose basename carries no ID (trailing separator)
			// also yields no session ID. Skipping is the safe reading because this
			// payload cannot identify a session without a repo-global fallback.
			name:  "session file with unparseable name",
			hook:  HookNameBeforeAgentStart,
			stdin: `{"type":"before_agent_start","cwd":"/repo","session_file":"/tmp/x_.jsonl","prompt":"hi"}`,
		},
		{
			// The guard distrusts a missing ID, not the payload: an explicit
			// session_id is still honoured with no session file present.
			name:          "explicit session_id without a session file",
			hook:          HookNameBeforeAgentStart,
			stdin:         `{"type":"before_agent_start","cwd":"/repo","session_id":"explicit-id","prompt":"hi"}`,
			wantSessionID: "explicit-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ev, err := a.ParseHookEvent(ctx, tt.hook, strings.NewReader(tt.stdin))
			if err != nil {
				t.Fatalf("ParseHookEvent(%s): %v", tt.hook, err)
			}

			if tt.wantSessionID == "" {
				if ev != nil {
					t.Fatalf("emitted %s for session %q; want no event",
						ev.Type, ev.SessionID)
				}
				return
			}

			if ev == nil {
				t.Fatalf("emitted no event; want one for session %q", tt.wantSessionID)
			}
			if ev.SessionID != tt.wantSessionID {
				t.Errorf("SessionID = %q, want %q", ev.SessionID, tt.wantSessionID)
			}
		})
	}
}
