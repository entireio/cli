package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// ensureUserHooks installs the user-level hooks with Entire's record of them
// kept out of the developer's config.
func ensureUserHooks(t *testing.T) (int, string, error) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	install, err := (&ClaudeCodeAgent{}).EnsureUserHooks(context.Background(), nil)
	return install.Changed, install.Path, err
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

func TestEnsureUserHooks_KeepsUserKeys(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	existing := `{"model":"opus","hooks":{"Notification":[{"matcher":"","hooks":[{"type":"command","command":"say hi"}]}]}}`
	if err := os.WriteFile(filepath.Join(userDir, ClaudeSettingsFileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ensureUserHooks(t); err != nil {
		t.Fatalf("EnsureUserHooks() error = %v", err)
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

// The user-level hook version lives in Entire's own record, never in the hook
// commands, which stay byte-identical to the project ones (see
// TestEnsureUserHooks_WritesTheSameHooksEnableWrites); inspect reads it back.
func TestEnsureUserHooks_RecordsTheVersionOutsideTheCommands(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ag := &ClaudeCodeAgent{}

	if _, err := ag.EnsureUserHooks(context.Background(), nil); err != nil {
		t.Fatalf("EnsureUserHooks() error = %v", err)
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
	if got := ag.InspectUserHooks(context.Background()).Version; got != 0 {
		t.Errorf("version after removal = %d, want 0", got)
	}
}

// writeClaudeSettings writes a Claude settings file whose UserPromptSubmit
// runs command (none when empty).
func writeClaudeSettings(t *testing.T, dir, command string) {
	t.Helper()
	content := `{}`
	if command != "" {
		hooks, err := json.Marshal(map[string]any{"hooks": map[string]any{"UserPromptSubmit": []map[string]any{
			{"matcher": "", "hooks": []map[string]any{{"type": "command", "command": command}}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		content = string(hooks)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ClaudeSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Claude Code runs a command in both its user and project settings once only
// when the two are byte-identical, so a prompt can fire Entire twice only
// when both files carry Entire's UserPromptSubmit hook and they differ.
func TestPromptHookMayFireTwice(t *testing.T) {
	current := agent.WrapProductionSilentHookCommand("entire hooks claude-code user-prompt-submit")
	const older = "entire hooks claude-code user-prompt-submit"
	for name, tc := range map[string]struct {
		user, project string
		want          bool
	}{
		"identical commands":        {current, current, false},
		"older project command":     {current, older, true},
		"only the user-level copy":  {current, "", false},
		"only the project copy":     {"", older, false},
		"project hook not Entire's": {current, "echo hi", false},
	} {
		t.Run(name, func(t *testing.T) {
			userDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", userDir)
			repo := t.TempDir()
			testutil.InitRepo(t, repo)
			t.Chdir(repo)
			writeClaudeSettings(t, userDir, tc.user)
			writeClaudeSettings(t, filepath.Join(repo, ".claude"), tc.project)
			if got := (&ClaudeCodeAgent{}).PromptHookMayFireTwice(context.Background()); got != tc.want {
				t.Errorf("PromptHookMayFireTwice() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Lifecycle reaches these checks only through the agent capabilities.
func TestClaudeCodeImplementsLifecycleHookCapabilities(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{}
	if _, ok := agent.AsPromptRepeatReporter(ag); !ok {
		t.Error("Claude Code must implement agent.PromptRepeatReporter")
	}
	if _, ok := agent.AsRequiredHooksReporter(ag); !ok {
		t.Error("Claude Code must implement agent.RequiredHooksReporter")
	}
}
