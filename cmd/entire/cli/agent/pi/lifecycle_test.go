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

func TestParseHookEvent_SessionShutdown_NoLifecycleEvent(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	stdin := strings.NewReader(`{"type":"session_shutdown","session_id":"explicit-id"}`)
	ev, err := a.ParseHookEvent(context.Background(), HookNameSessionShutdown, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	// session_shutdown is cleanup-only — see ParseHookEvent for the rationale.
	if ev != nil {
		t.Fatalf("expected nil event from session_shutdown, got %+v", ev)
	}
}

func TestParseHookEvent_SessionShutdown_ClearsCache(t *testing.T) {
	// session_shutdown's only side effect is clearing the cached session ID.
	// Cannot use t.Parallel — t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	ctx := context.Background()
	a := &PiAgent{}

	// Populate the cache via session_start.
	if _, err := a.ParseHookEvent(ctx, HookNameSessionStart, strings.NewReader(
		`{"type":"session_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_cached-id.jsonl"}`)); err != nil {
		t.Fatalf("session_start setup: %v", err)
	}
	if got := readCachedSessionID(ctx); got != "cached-id" {
		t.Fatalf("cache pre-shutdown = %q, want cached-id", got)
	}

	// session_shutdown clears the cache and emits no event.
	ev, err := a.ParseHookEvent(ctx, HookNameSessionShutdown, strings.NewReader(`{"type":"session_shutdown"}`))
	if err != nil {
		t.Fatalf("session_shutdown: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event, got %+v", ev)
	}
	if got := readCachedSessionID(ctx); got != "" {
		t.Errorf("cache should be cleared after session_shutdown, got %q", got)
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

func TestSessionIDCacheRoundtrip(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir mutates process state.
	dir := t.TempDir()
	t.Chdir(dir)

	ctx := context.Background()
	if got := readCachedSessionID(ctx); got != "" {
		t.Errorf("expected empty cache initially, got %q", got)
	}
	cacheSessionID(ctx, "abc-123")
	if got := readCachedSessionID(ctx); got != "abc-123" {
		t.Errorf("readCachedSessionID = %q, want abc-123", got)
	}
	clearCachedSessionID(ctx)
	if got := readCachedSessionID(ctx); got != "" {
		t.Errorf("after clear, got %q", got)
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

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()
	got := (&PiAgent{}).GetSupportedHooks()
	// Note: session_shutdown is cleanup-only, not a HookSessionEnd source —
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
