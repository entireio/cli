package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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

	n, path, err := EnsureUserHooks(context.Background())
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

	if _, _, err := EnsureUserHooks(context.Background()); err != nil {
		t.Fatalf("first EnsureUserHooks() error = %v", err)
	}
	n, _, err := EnsureUserHooks(context.Background())
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

	if _, _, err := EnsureUserHooks(context.Background()); err == nil {
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

// ~/.claude kept in a dotfiles checkout is common; the write must land on the
// link's target rather than replace the link.
func TestEnsureUserHooks_WritesThroughASymlinkedSettingsFile(t *testing.T) {
	userDir := t.TempDir()
	dotfiles := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	target := filepath.Join(dotfiles, "claude-settings.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(userDir, ClaudeSettingsFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := EnsureUserHooks(context.Background()); err != nil {
		t.Fatalf("EnsureUserHooks() error = %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("settings.json symlink was replaced by a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["hooks"]; !ok {
		t.Errorf("symlink target not updated: %s", data)
	}
}

func TestEnsureUserHooks_RefusesADanglingSymlink(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())
	link := filepath.Join(userDir, ClaudeSettingsFileName)
	if err := os.Symlink(filepath.Join(t.TempDir(), "not-there-yet.json"), link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := EnsureUserHooks(context.Background()); err == nil {
		t.Fatal("EnsureUserHooks() succeeded through a dangling symlink")
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("dangling settings.json symlink was replaced by a regular file")
	}
}

func TestEnsureUserHooks_ConcurrentCallsInstallOnce(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Chdir(t.TempDir())

	const callers = 8
	added := make(chan int, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _, err := EnsureUserHooks(context.Background())
			if err != nil {
				t.Errorf("EnsureUserHooks() error = %v", err)
			}
			added <- n
		}()
	}
	wg.Wait()
	close(added)
	installs := 0
	for n := range added {
		if n > 0 {
			installs++
		}
	}
	if installs != 1 {
		t.Errorf("%d callers installed hooks, want exactly 1", installs)
	}
}

// Without a Claude config directory Claude Code is not in use; nothing is
// created for it.
func TestEnsureUserHooks_NoClaudeConfigDirCreatesNothing(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Chdir(t.TempDir())

	n, _, err := EnsureUserHooks(context.Background())
	if !errors.Is(err, ErrClaudeNotInUse) {
		t.Fatalf("EnsureUserHooks() error = %v, want ErrClaudeNotInUse", err)
	}
	if n != 0 {
		t.Errorf("EnsureUserHooks() added %d hooks, want 0", n)
	}
	if _, err := os.Stat(claudeDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Claude config dir was created (stat err = %v)", err)
	}
}
