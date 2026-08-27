package qwencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func settingsIn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return filepath.Join(dir, configDirName, settingsFileName)
}

func readSettings(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("settings is not valid JSON: %v", err)
	}
	return raw
}

func hookTable(t *testing.T, path string) map[string][]hookMatcherEntry {
	t.Helper()
	raw := readSettings(t, path)
	var hooks map[string][]hookMatcherEntry
	if err := json.Unmarshal(raw[hooksKey], &hooks); err != nil {
		t.Fatalf("hooks is not valid: %v", err)
	}
	return hooks
}

func TestInstallHooks_RegistersFourEvents(t *testing.T) {
	path := settingsIn(t)

	a := &QwenCodeAgent{}
	added, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if added != 4 {
		t.Errorf("added %d hooks, want 4", added)
	}

	hooks := hookTable(t, path)
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		entries, ok := hooks[event]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("no hook registered for %q", event)
			continue
		}
		cmd := entries[0].Hooks[0]
		if cmd.Type != "command" {
			t.Errorf("%s: type = %q, want command", event, cmd.Type)
		}
		if !strings.Contains(cmd.Command, "entire hooks qwen-code ") {
			t.Errorf("%s: command %q does not invoke the entire hook", event, cmd.Command)
		}
	}
}

// The settings file belongs to the user. Installing must not disturb unrelated
// settings or hooks they wrote themselves.
func TestInstallHooks_PreservesForeignSettingsAndHooks(t *testing.T) {
	path := settingsIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const existing = `{
  "theme": "dark",
  "mcpServers": {"x": {"command": "y"}},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "my-own-linter"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-guard"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &QwenCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	raw := readSettings(t, path)
	if _, ok := raw["theme"]; !ok {
		t.Error("unrelated setting 'theme' was dropped")
	}
	if _, ok := raw["mcpServers"]; !ok {
		t.Error("unrelated setting 'mcpServers' was dropped")
	}

	hooks := hookTable(t, path)
	// The user's own Stop hook must survive alongside Entire's.
	var sawUserHook, sawEntireHook bool
	for _, e := range hooks["Stop"] {
		for _, h := range e.Hooks {
			if h.Command == "my-own-linter" {
				sawUserHook = true
			}
			if strings.Contains(h.Command, "entire hooks qwen-code turn-end") {
				sawEntireHook = true
			}
		}
	}
	if !sawUserHook {
		t.Error("the user's own Stop hook was removed")
	}
	if !sawEntireHook {
		t.Error("Entire's Stop hook was not added")
	}
	if len(hooks["PreToolUse"]) != 1 {
		t.Error("the user's PreToolUse hook was disturbed")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	settingsIn(t)

	a := &QwenCodeAgent{}
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

// An Entire hook written by an older version must be replaced, not joined, or
// two Entire hooks fire on every turn.
func TestInstallHooks_DropsStaleEntireHook(t *testing.T) {
	path := settingsIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A bare (unwrapped) command is a shape an older Entire wrote.
	stale := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"entire hooks qwen-code turn-end"}]}]}}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &QwenCodeAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	entries := hookTable(t, path)["Stop"]
	entireHooks := 0
	for _, e := range entries {
		for _, h := range e.Hooks {
			if agent.IsManagedHookCommand(h.Command) {
				entireHooks++
			}
		}
	}
	if entireHooks != 1 {
		t.Errorf("found %d Entire hooks on Stop, want exactly 1 (stale one must be replaced)", entireHooks)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	settingsIn(t)

	a := &QwenCodeAgent{}
	ctx := context.Background()
	if a.AreHooksInstalled(ctx) {
		t.Error("hooks reported installed in a clean repo")
	}
	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !a.AreHooksInstalled(ctx) {
		t.Error("hooks not reported installed after install")
	}
}

func TestUninstallHooks_LeavesUserHooksAndSettings(t *testing.T) {
	path := settingsIn(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const existing = `{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-own-linter"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &QwenCodeAgent{}
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
	raw := readSettings(t, path)
	if _, ok := raw["theme"]; !ok {
		t.Error("uninstall dropped an unrelated setting")
	}
	hooks := hookTable(t, path)
	var sawUserHook bool
	for _, e := range hooks["Stop"] {
		for _, h := range e.Hooks {
			if h.Command == "my-own-linter" {
				sawUserHook = true
			}
		}
	}
	if !sawUserHook {
		t.Error("uninstall removed the user's own hook")
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	got := a.GetSupportedHooks()
	want := []string{"SessionEnd", "SessionStart", "Stop", "UserPromptSubmit"}
	if len(got) != len(want) {
		t.Fatalf("GetSupportedHooks() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseHookEvent(t *testing.T) {
	t.Parallel()

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
	const stdin = `{"session_id":"abc-123","transcript_path":"/tmp/abc.jsonl","prompt":"hi","cwd":"/repo"}`

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()

			a := &QwenCodeAgent{}
			ev, err := a.ParseHookEvent(context.Background(), tc.verb, strings.NewReader(stdin))
			if err != nil {
				t.Fatalf("ParseHookEvent: %v", err)
			}
			if ev == nil {
				t.Fatal("nil event")
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type = %v, want %v", ev.Type, tc.wantType)
			}
			if ev.SessionID != "abc-123" {
				t.Errorf("SessionID = %q", ev.SessionID)
			}
			// The transcript path comes straight from the payload; Entire never
			// reconstructs it for Qwen.
			if tc.wantRef && ev.SessionRef != "/tmp/abc.jsonl" {
				t.Errorf("SessionRef = %q, want the payload's transcript_path", ev.SessionRef)
			}
			if !tc.wantRef && ev.SessionRef != "" {
				t.Errorf("unexpected SessionRef %q", ev.SessionRef)
			}
		})
	}
}

func TestParseHookEvent_UnknownVerbAndBadInput(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
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
