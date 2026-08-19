package openhands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

func hooksIn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return filepath.Join(dir, configDirName, hooksFileName)
}

func hookTable(t *testing.T, path string) map[string][]hookMatcher {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var hooks map[string][]hookMatcher
	if err := json.Unmarshal(data, &hooks); err != nil {
		t.Fatalf("hooks is not valid JSON: %v", err)
	}
	return hooks
}

func TestInstallHooks_UsesCanonicalSnakeCaseFields(t *testing.T) {
	path := hooksIn(t)

	a := &OpenHandsAgent{}
	added, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if added != 4 {
		t.Errorf("added %d, want 4", added)
	}

	hooks := hookTable(t, path)
	for _, field := range []string{"session_start", "user_prompt_submit", "stop", "session_end"} {
		entries, ok := hooks[field]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("no hook registered under %q", field)
			continue
		}
		if entries[0].Matcher != matchAll {
			t.Errorf("%s: matcher = %q, want %q", field, entries[0].Matcher, matchAll)
		}
		cmd := entries[0].Hooks[0]
		if cmd.Type != "command" {
			t.Errorf("%s: type = %q", field, cmd.Type)
		}
		if !strings.Contains(cmd.Command, "entire hooks openhands ") {
			t.Errorf("%s: command %q does not invoke the entire hook", field, cmd.Command)
		}
	}
}

// HookConfig sets extra="forbid", so any key OpenHands does not recognise makes
// it reject the whole file. This is why the Goose-style marker comment is absent.
func TestInstallHooks_WritesNoUnrecognisedKeys(t *testing.T) {
	path := hooksIn(t)

	a := &OpenHandsAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// The complete set OpenHands accepts, from openhands/sdk/hooks/config.py.
	allowed := map[string]bool{
		"pre_tool_use": true, "post_tool_use": true, "user_prompt_submit": true,
		"session_start": true, "session_end": true, "stop": true,
	}
	for key := range raw {
		if !allowed[key] {
			t.Errorf("wrote key %q, which OpenHands would reject (extra=forbid)", key)
		}
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	hooksIn(t)

	a := &OpenHandsAgent{}
	ctx := context.Background()
	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	added, err := a.InstallHooks(ctx, false)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if added != 0 {
		t.Errorf("second install added %d, want 0", added)
	}
}

func TestInstallHooks_PreservesUserHooks(t *testing.T) {
	path := hooksIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const existing = `{"pre_tool_use":[{"matcher":"terminal","hooks":[{"type":"command","command":"my-guard.sh"}]}]}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &OpenHandsAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	hooks := hookTable(t, path)
	if len(hooks["pre_tool_use"]) != 1 || hooks["pre_tool_use"][0].Hooks[0].Command != "my-guard.sh" {
		t.Error("the user's own pre_tool_use hook was disturbed")
	}
	if len(hooks["stop"]) == 0 {
		t.Error("Entire's stop hook was not added")
	}
}

// OpenHands still accepts a {"hooks": {...}} wrapper for Claude Code interop.
// Reading one must unwrap it rather than nesting a second level on rewrite.
func TestInstallHooks_UnwrapsLegacyWrapper(t *testing.T) {
	path := hooksIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const wrapped = `{"hooks":{"pre_tool_use":[{"matcher":"*","hooks":[{"type":"command","command":"mine.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(wrapped), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &OpenHandsAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, nested := raw["hooks"]; nested {
		t.Error("rewrote the file with a nested hooks wrapper")
	}
	if _, ok := raw["pre_tool_use"]; !ok {
		t.Error("the user's hook was lost while unwrapping")
	}
}

func TestInstallHooks_DropsStaleEntireHook(t *testing.T) {
	path := hooksIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := `{"stop":[{"matcher":"*","hooks":[{"type":"command","command":"entire hooks openhands turn-end"}]}]}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &OpenHandsAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	count := 0
	for _, m := range hookTable(t, path)["stop"] {
		for _, h := range m.Hooks {
			if agent.IsManagedHookCommand(h.Command) {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("found %d Entire hooks on stop, want exactly 1", count)
	}
}

func TestUninstallHooks_LeavesUserHooks(t *testing.T) {
	path := hooksIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const existing = `{"pre_tool_use":[{"matcher":"*","hooks":[{"type":"command","command":"mine.sh"}]}]}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &OpenHandsAgent{}
	ctx := context.Background()
	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if err := a.UninstallHooks(ctx); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	if a.AreHooksInstalled(ctx) {
		t.Error("hooks still reported installed after uninstall")
	}
	hooks := hookTable(t, path)
	if len(hooks["pre_tool_use"]) != 1 {
		t.Error("uninstall removed the user's own hook")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	hooksIn(t)

	a := &OpenHandsAgent{}
	ctx := context.Background()
	if a.AreHooksInstalled(ctx) {
		t.Error("reported installed in a clean repo")
	}
	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !a.AreHooksInstalled(ctx) {
		t.Error("not reported installed after install")
	}
}

func TestParseHookEvent(t *testing.T) {
	// Not parallel: sets the persistence dir so the reconstructed path is
	// predictable.
	t.Setenv(conversationsEnv, "/conv")

	const id = "04e2eedbe2d64736a1a4436334d9e1e6"
	stdin := `{"event_type":"Stop","session_id":"` + id + `","working_dir":"/repo","message":"do it"}`

	cases := []struct {
		verb     string
		wantType agent.EventType
		wantRef  bool
	}{
		{HookNameSessionStart, agent.SessionStart, false},
		{HookNameTurnStart, agent.TurnStart, true},
		{HookNameTurnEnd, agent.TurnEnd, true},
		{HookNameSessionEnd, agent.SessionEnd, false},
	}

	a := &OpenHandsAgent{}
	for _, tc := range cases {
		ev, err := a.ParseHookEvent(context.Background(), tc.verb, strings.NewReader(stdin))
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if ev.Type != tc.wantType {
			t.Errorf("%s: Type = %v, want %v", tc.verb, ev.Type, tc.wantType)
		}
		if ev.SessionID != id {
			t.Errorf("%s: SessionID = %q", tc.verb, ev.SessionID)
		}
		if tc.wantRef {
			want := filepath.Join("/conv", id, eventsDirName)
			if ev.SessionRef != want {
				t.Errorf("%s: SessionRef = %q, want %q", tc.verb, ev.SessionRef, want)
			}
		} else if ev.SessionRef != "" {
			t.Errorf("%s: unexpected SessionRef %q", tc.verb, ev.SessionRef)
		}
	}
}

// A dashed id from the hook must still resolve to the undashed directory.
func TestParseHookEvent_NormalizesDashedID(t *testing.T) {
	t.Setenv(conversationsEnv, "/conv")

	a := &OpenHandsAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameTurnEnd,
		strings.NewReader(`{"session_id":"04e2eedb-e2d6-4736-a1a4-436334d9e1e6"}`))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	want := filepath.Join("/conv", "04e2eedbe2d64736a1a4436334d9e1e6", eventsDirName)
	if ev.SessionRef != want {
		t.Errorf("SessionRef = %q, want %q", ev.SessionRef, want)
	}
}

func TestParseHookEvent_UnknownVerbAndBadInput(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	ctx := context.Background()

	ev, err := a.ParseHookEvent(ctx, "nope", strings.NewReader(`{}`))
	if err != nil || ev != nil {
		t.Errorf("unknown verb: got (%v, %v), want (nil, nil)", ev, err)
	}
	if _, err := a.ParseHookEvent(ctx, HookNameTurnEnd, strings.NewReader("")); err == nil {
		t.Error("expected an error on empty stdin")
	}
	if _, err := a.ParseHookEvent(ctx, HookNameTurnEnd, strings.NewReader("{bad")); err == nil {
		t.Error("expected an error on malformed stdin")
	}
}

// The conversation id becomes a path component, so traversal must be rejected
// before the join.
func TestParseHookEvent_RejectsTraversalSessionID(t *testing.T) {
	t.Setenv(conversationsEnv, "/conv")

	a := &OpenHandsAgent{}
	if _, err := a.ParseHookEvent(context.Background(), HookNameTurnEnd,
		strings.NewReader(`{"session_id":"../../etc/passwd"}`)); err == nil {
		t.Fatal("expected a traversal session id to be rejected")
	}
	if err := validation.ValidateSessionID(".."); err == nil {
		t.Fatal(`ValidateSessionID("..") must reject to guard the path join`)
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	got := a.GetSupportedHooks()
	want := []string{"session_end", "session_start", "stop", "user_prompt_submit"}
	if len(got) != len(want) {
		t.Fatalf("GetSupportedHooks() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
