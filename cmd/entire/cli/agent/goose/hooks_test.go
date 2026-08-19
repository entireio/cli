package goose

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// installInTempRepo runs InstallHooks with the repo root pointed at a temp dir.
// hooksPath falls back to the working directory when WorktreeRoot fails, so
// chdir-ing is what scopes the install.
func installInTempRepo(t *testing.T, force bool) (dir, path string, count int) {
	t.Helper()
	dir = t.TempDir()
	t.Chdir(dir)

	a := &GooseAgent{}
	count, err := a.InstallHooks(context.Background(), force)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	return dir, filepath.Join(dir, pluginsDirName, pluginName, hooksFileName), count
}

func TestInstallHooks_WritesDiscoverableConfig(t *testing.T) {
	// Not parallel: uses t.Chdir.
	_, path, count := installInTempRepo(t, false)

	if count != 1 {
		t.Errorf("InstallHooks returned %d, want 1", count)
	}

	// The path is what Goose's plugin discovery scans; getting it wrong means
	// hooks silently never load.
	if !strings.HasSuffix(filepath.ToSlash(path), ".agents/plugins/entire/hooks/hooks.json") {
		t.Errorf("unexpected install path %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks file: %v", err)
	}

	var doc struct {
		Comment string                        `json:"_comment"`
		Hooks   map[string][]hookMatcherEntry `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("hooks file is not valid JSON: %v", err)
	}

	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		entries, ok := doc.Hooks[event]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("no hook registered for Goose event %q", event)
			continue
		}
		cmd := entries[0].Hooks[0]
		if cmd.Type != "command" {
			t.Errorf("%s: type = %q, want command (the only type Goose supports)", event, cmd.Type)
		}
		if !strings.Contains(cmd.Command, "entire hooks goose ") {
			t.Errorf("%s: command %q does not invoke the entire hook", event, cmd.Command)
		}
	}
}

// The hook command must name the `entire` binary resolved through PATH. A
// repo-relative command would execute whatever the checked-out branch contains
// on every agent turn.
func TestInstallHooks_CommandIsNotRepoRelative(t *testing.T) {
	dir, path, _ := installInTempRepo(t, false)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks file: %v", err)
	}
	content := string(data)

	if strings.Contains(content, dir) {
		t.Error("hook command embeds an absolute repo path; it must resolve `entire` through PATH")
	}
	for _, bad := range []string{"./", "../", "$PLUGIN_ROOT", "${PLUGIN_ROOT}"} {
		if strings.Contains(content, bad) {
			t.Errorf("hook command contains repo-relative marker %q", bad)
		}
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir, _, first := installInTempRepo(t, false)
	if first != 1 {
		t.Fatalf("first install returned %d, want 1", first)
	}

	a := &GooseAgent{}
	second, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if second != 0 {
		t.Errorf("second install returned %d, want 0 (no rewrite when current)", second)
	}
	_ = dir
}

func TestInstallHooks_RefusesForeignFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	path := filepath.Join(dir, pluginsDirName, pluginName, hooksFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const foreign = `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-own-thing"}]}]}}`
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	a := &GooseAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err == nil {
		t.Fatal("expected InstallHooks to refuse overwriting a foreign plugin file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != foreign {
		t.Error("foreign file was modified despite the refusal")
	}
}

func TestInstallHooks_ForceOverwritesForeignFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	path := filepath.Join(dir, pluginsDirName, pluginName, hooksFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &GooseAgent{}
	count, err := a.InstallHooks(context.Background(), true)
	if err != nil {
		t.Fatalf("forced InstallHooks: %v", err)
	}
	if count != 1 {
		t.Errorf("forced install returned %d, want 1", count)
	}
	if !a.AreHooksInstalled(context.Background()) {
		t.Error("hooks should be recognised as installed after a forced install")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	a := &GooseAgent{}
	if a.AreHooksInstalled(context.Background()) {
		t.Error("hooks should not be reported as installed in a clean repo")
	}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !a.AreHooksInstalled(context.Background()) {
		t.Error("hooks should be reported as installed after install")
	}
}

func TestUninstallHooks_RemovesOnlyEntirePlugin(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// A sibling plugin that Entire does not own.
	otherPlugin := filepath.Join(dir, pluginsDirName, "someone-else", "hooks")
	if err := os.MkdirAll(otherPlugin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	otherFile := filepath.Join(otherPlugin, "hooks.json")
	if err := os.WriteFile(otherFile, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &GooseAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}

	if a.AreHooksInstalled(context.Background()) {
		t.Error("hooks still reported as installed after uninstall")
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Error("uninstall removed a plugin Entire does not own")
	}
}

func TestCheckHookConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	a := &GooseAgent{}
	ctx := context.Background()

	if got := a.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("clean repo: state = %v, want HooksAbsent", got)
	}

	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("after install: state = %v, want HooksCurrent", got)
	}

	// Simulate a stale committed file: still Entire-owned, but not what we
	// would write today. This is the case AreHooksInstalled cannot detect.
	path := filepath.Join(dir, pluginsDirName, pluginName, hooksFileName)
	stale := "{\n  \"_comment\": \"" + entireMarker + "\",\n  \"hooks\": {}\n}\n"
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksOutdated {
		t.Errorf("stale file: state = %v, want HooksOutdated", got)
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	got := a.GetSupportedHooks()
	want := []string{"SessionEnd", "SessionStart", "Stop", "UserPromptSubmit"}
	if len(got) != len(want) {
		t.Fatalf("GetSupportedHooks() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GetSupportedHooks()[%d] = %q, want %q (sorted)", i, got[i], want[i])
		}
	}
}
