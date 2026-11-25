package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// ensureUserHooks installs the user-level hooks with the side-file stamp
// kept out of the developer's config.
func ensureUserHooks(t *testing.T) (int, string, error) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	return (&ClaudeCodeAgent{}).EnsureUserHooks(context.Background(), nil)
}

func readUserClaudeSettings(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ClaudeSettingsFileName))
	if err != nil {
		t.Fatalf("read user settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse user settings: %v", err)
	}
	return raw
}

// The hooks land in the user file Claude Code reads for every project, and the
// command strings are byte-identical to what `entire enable` writes into a
// project — Claude Code deduplicates identical commands, which is what keeps a
// repository that also has project hooks from firing each one twice.
func TestEnsureUserHooks_WritesTheSameHooksEnableWrites(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	repo := t.TempDir()
	t.Chdir(repo)

	n, path, err := ensureUserHooks(t)
	if err != nil {
		t.Fatalf("EnsureUserHooks() error = %v", err)
	}
	if n == 0 {
		t.Fatal("EnsureUserHooks() installed 0 hooks into an empty user config")
	}
	if want := filepath.Join(userDir, ClaudeSettingsFileName); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := (&ClaudeCodeAgent{}).InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	user := readUserClaudeSettings(t, userDir)
	project := readUserClaudeSettings(t, filepath.Join(repo, ".claude"))
	if string(user["hooks"]) != string(project["hooks"]) {
		t.Errorf("user hooks differ from project hooks\nuser:    %s\nproject: %s", user["hooks"], project["hooks"])
	}
}

func TestEnsureUserHooks_IsIdempotentAndKeepsUserKeys(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	existing := `{"model":"opus","hooks":{"Notification":[{"matcher":"","hooks":[{"type":"command","command":"say hi"}]}]}}`
	if err := os.WriteFile(filepath.Join(userDir, ClaudeSettingsFileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ensureUserHooks(t); err != nil {
		t.Fatalf("first EnsureUserHooks() error = %v", err)
	}
	n, _, err := ensureUserHooks(t)
	if err != nil {
		t.Fatalf("second EnsureUserHooks() error = %v", err)
	}
	if n != 0 {
		t.Errorf("second EnsureUserHooks() installed %d hooks, want 0", n)
	}

	raw := readUserClaudeSettings(t, userDir)
	if string(raw["model"]) != `"opus"` {
		t.Errorf("model = %s, want the user's value kept", raw["model"])
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooks["Notification"]; !ok {
		t.Error("user's Notification hook was dropped")
	}
	if _, ok := hooks["Stop"]; !ok {
		t.Error("Entire's Stop hook is missing")
	}
}

// A malformed user file is the user's to fix; overwriting it would destroy
// whatever else they keep there.
func TestEnsureUserHooks_RefusesAMalformedFile(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	settingsPath := filepath.Join(userDir, ClaudeSettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ensureUserHooks(t); err == nil {
		t.Fatal("EnsureUserHooks() error = nil, want a parse error")
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{not json` {
		t.Errorf("malformed file was rewritten to %q", data)
	}
}

// RemoveUserHooks takes out exactly Entire's hooks and keeps everything the
// user put in the file, including their own hooks.
func TestRemoveUserHooks_RemovesOnlyEntireHooks(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	existing := `{"model":"opus","hooks":{"Notification":[{"matcher":"","hooks":[{"type":"command","command":"say hi"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"say bye"}]}]}}`
	if err := os.WriteFile(filepath.Join(userDir, ClaudeSettingsFileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureUserHooks(t); err != nil {
		t.Fatal(err)
	}

	removed, path, err := (&ClaudeCodeAgent{}).RemoveUserHooks(context.Background())
	if err != nil {
		t.Fatalf("RemoveUserHooks() error = %v", err)
	}
	if want := filepath.Join(userDir, ClaudeSettingsFileName); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if !removed {
		t.Error("RemoveUserHooks() reported nothing removed")
	}
	raw := readUserClaudeSettings(t, userDir)
	if string(raw["model"]) != `"opus"` {
		t.Errorf("model = %s, want the user's value kept", raw["model"])
	}
	var hooks map[string][]ClaudeHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	for hookType, matchers := range hooks {
		if hasEntireHook(matchers) {
			t.Errorf("%s still has an Entire hook", hookType)
		}
	}
	if len(hooks["Notification"]) != 1 || len(hooks["Stop"]) != 1 {
		t.Errorf("the user's own hooks were not kept: %s", raw["hooks"])
	}
}

// Nothing to remove writes nothing: no file is created, and a file without
// Entire's hooks is left byte for byte.
func TestRemoveUserHooks_NothingToRemoveWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	if removed, _, err := (&ClaudeCodeAgent{}).RemoveUserHooks(context.Background()); err != nil || removed {
		t.Fatalf("RemoveUserHooks() with no config dir = %v, %v", removed, err)
	}

	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	const existing = `{ "model": "opus" }`
	path := filepath.Join(userDir, ClaudeSettingsFileName)
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := (&ClaudeCodeAgent{}).RemoveUserHooks(context.Background()); err != nil || removed {
		t.Fatalf("RemoveUserHooks() without Entire hooks = %v, %v", removed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("file rewritten to %q", data)
	}
}

// The user-level hook version lives in Entire's side file, never in the hook
// commands, which stay byte-identical to the project ones (see
// TestEnsureUserHooks_WritesTheSameHooksEnableWrites); inspect reads it back.
func TestEnsureUserHooks_StampsTheVersionInASideFile(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ag := &ClaudeCodeAgent{}

	_, _, err := ag.EnsureUserHooks(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnsureUserHooks() error = %v", err)
	}
	if got := agent.ReadUserHookStamp(agent.AgentNameClaudeCode); got != userHooksVersion {
		t.Errorf("stamp = %d, want %d", got, userHooksVersion)
	}
	data, err := os.ReadFile(filepath.Join(userDir, ClaudeSettingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "--scope") || strings.Contains(string(data), "user/v") {
		t.Errorf("user-level commands carry a version:\n%s", data)
	}
	report := ag.InspectUserHooks(context.Background())
	if report.State != agent.UserHooksCurrent || report.Version != userHooksVersion {
		t.Errorf("InspectUserHooks() = %+v, want current at version %d", report, userHooksVersion)
	}

	if _, _, err := ag.RemoveUserHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := agent.ReadUserHookStamp(agent.AgentNameClaudeCode); got != 0 {
		t.Errorf("stamp after removal = %d, want 0", got)
	}
}
