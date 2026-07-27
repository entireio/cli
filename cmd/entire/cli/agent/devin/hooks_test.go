package devin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHooksTestRepo chdirs into an isolated temp dir so hooksFilePath
// resolves there via the CWD fallback (the dir is outside any git repo).
// Not parallel-safe (t.Chdir).
func setupHooksTestRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	return tmpDir
}

func readHooksFile(t *testing.T, repoDir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoDir, ".devin", HooksFileName))
	if err != nil {
		t.Fatalf("read hooks file: %v", err)
	}
	rawHooks := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &rawHooks); err != nil {
		t.Fatalf("parse hooks file: %v", err)
	}
	return rawHooks
}

func TestInstallHooks_FreshInstall(t *testing.T) {
	repoDir := setupHooksTestRepo(t)
	d := &DevinAgent{}

	count, err := d.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if count != len(managedHookEvents) {
		t.Errorf("count = %d, want %d", count, len(managedHookEvents))
	}

	rawHooks := readHooksFile(t, repoDir)
	for _, event := range []string{hookEventSessionStart, hookEventSessionEnd, hookEventStop, hookEventUserPromptSubmit, hookEventPostToolUse} {
		if _, ok := rawHooks[event]; !ok {
			t.Errorf("hooks file missing event %q", event)
		}
	}

	// The PostToolUse entry must be scoped to file-modification tools.
	var postToolUse []HookMatcher
	if err := json.Unmarshal(rawHooks[hookEventPostToolUse], &postToolUse); err != nil {
		t.Fatalf("parse PostToolUse: %v", err)
	}
	if len(postToolUse) != 1 || postToolUse[0].Matcher != fileModificationToolsMatcher {
		t.Errorf("PostToolUse matcher = %+v, want matcher %q", postToolUse, fileModificationToolsMatcher)
	}

	// Commands must be production-wrapped entire hook invocations.
	var stop []HookMatcher
	if err := json.Unmarshal(rawHooks[hookEventStop], &stop); err != nil {
		t.Fatalf("parse Stop: %v", err)
	}
	if len(stop) != 1 || len(stop[0].Hooks) != 1 {
		t.Fatalf("Stop matchers = %+v", stop)
	}
	if !strings.Contains(stop[0].Hooks[0].Command, "entire hooks devin stop") {
		t.Errorf("Stop command = %q, want it to invoke 'entire hooks devin stop'", stop[0].Hooks[0].Command)
	}

	if !d.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled = false after install")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setupHooksTestRepo(t)
	d := &DevinAgent{}

	if _, err := d.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("first InstallHooks: %v", err)
	}
	count, err := d.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if count != 0 {
		t.Errorf("second install count = %d, want 0", count)
	}
}

func TestInstallHooks_PreservesForeignHooks(t *testing.T) {
	repoDir := setupHooksTestRepo(t)
	d := &DevinAgent{}

	// Pre-existing user hooks: a custom Stop hook and a custom event Entire
	// doesn't manage (PreToolUse).
	existing := `{
  "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "./my-hook.sh"}]}],
  "PreToolUse": [{"matcher": "exec", "hooks": [{"type": "command", "command": "./validate.sh"}]}]
}`
	if err := os.MkdirAll(filepath.Join(repoDir, ".devin"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".devin", HooksFileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := d.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	rawHooks := readHooksFile(t, repoDir)

	var preToolUse []HookMatcher
	if err := json.Unmarshal(rawHooks["PreToolUse"], &preToolUse); err != nil {
		t.Fatalf("parse PreToolUse: %v", err)
	}
	if len(preToolUse) != 1 || preToolUse[0].Hooks[0].Command != "./validate.sh" {
		t.Errorf("foreign PreToolUse hook not preserved: %+v", preToolUse)
	}

	var stop []HookMatcher
	if err := json.Unmarshal(rawHooks[hookEventStop], &stop); err != nil {
		t.Fatalf("parse Stop: %v", err)
	}
	foundForeign, foundEntire := false, false
	for _, m := range stop {
		for _, h := range m.Hooks {
			if h.Command == "./my-hook.sh" {
				foundForeign = true
			}
			if isEntireHook(h.Command) {
				foundEntire = true
			}
		}
	}
	if !foundForeign || !foundEntire {
		t.Errorf("Stop hooks foreign=%v entire=%v, want both true: %+v", foundForeign, foundEntire, stop)
	}
}

func TestUninstallHooks_RemovesOnlyEntireHooks(t *testing.T) {
	repoDir := setupHooksTestRepo(t)
	d := &DevinAgent{}

	existing := `{"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "./my-hook.sh"}]}]}`
	if err := os.MkdirAll(filepath.Join(repoDir, ".devin"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".devin", HooksFileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := d.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if err := d.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}

	if d.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled = true after uninstall")
	}

	rawHooks := readHooksFile(t, repoDir)
	var stop []HookMatcher
	if err := json.Unmarshal(rawHooks[hookEventStop], &stop); err != nil {
		t.Fatalf("parse Stop: %v", err)
	}
	if len(stop) != 1 || stop[0].Hooks[0].Command != "./my-hook.sh" {
		t.Errorf("foreign Stop hook not preserved after uninstall: %+v", stop)
	}
	if _, ok := rawHooks[hookEventSessionStart]; ok {
		t.Error("SessionStart still present after uninstall (should be removed when empty)")
	}
}

func TestUninstallHooks_NoFile(t *testing.T) {
	setupHooksTestRepo(t)
	d := &DevinAgent{}
	if err := d.UninstallHooks(context.Background()); err != nil {
		t.Errorf("UninstallHooks with no file: %v", err)
	}
}

func TestInstallHooks_Force(t *testing.T) {
	setupHooksTestRepo(t)
	d := &DevinAgent{}

	if _, err := d.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	// Force reinstall with localDev=true swaps the command form; without
	// force the old production commands would be left in place alongside.
	count, err := d.InstallHooks(context.Background(), true, true)
	if err != nil {
		t.Fatalf("force InstallHooks: %v", err)
	}
	if count != len(managedHookEvents) {
		t.Errorf("force install count = %d, want %d", count, len(managedHookEvents))
	}

	rawHooks := readHooksFile(t, ".")
	var stop []HookMatcher
	if err := json.Unmarshal(rawHooks[hookEventStop], &stop); err != nil {
		t.Fatalf("parse Stop: %v", err)
	}
	total := 0
	for _, m := range stop {
		total += len(m.Hooks)
	}
	if total != 1 {
		t.Errorf("Stop hook count after force = %d, want 1 (no duplicates)", total)
	}
	if !strings.Contains(stop[0].Hooks[0].Command, "scripts/entire-dev") {
		t.Errorf("Stop command = %q, want local-dev form", stop[0].Hooks[0].Command)
	}
}

func TestAreHooksInstalled_NoFile(t *testing.T) {
	setupHooksTestRepo(t)
	d := &DevinAgent{}
	if d.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled = true with no hooks file")
	}
}
