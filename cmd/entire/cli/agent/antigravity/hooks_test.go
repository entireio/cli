package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHooks_FreshRepo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	n, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if n != 3 {
		t.Errorf("installed %d hooks, want 3", n)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".agents", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f HooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	cfg, ok := f["entire"]
	if !ok {
		t.Fatal("missing 'entire' hook entry")
	}
	if len(cfg.PreToolUse) != 1 || len(cfg.PreInvocation) != 1 || len(cfg.Stop) != 1 {
		t.Errorf("event coverage incomplete: %+v", cfg)
	}
	// PostToolUse/PostInvocation are deliberately not installed: they have no
	// lifecycle mapping and would spawn a no-op subprocess per tool call.
	if len(cfg.PostToolUse) != 0 || len(cfg.PostInvocation) != 0 {
		t.Errorf("no-op post hooks must not be installed: %+v", cfg)
	}
	if cfg.PreToolUse[0].Matcher != "*" {
		t.Errorf("PreToolUse matcher = %q, want %q", cfg.PreToolUse[0].Matcher, "*")
	}
	// Stop runs PrepareTranscript + SaveStep (a shadow-branch checkpoint
	// write); agy's default hook timeout is 30s, which a large repo can
	// exceed — agy would kill the hook mid-checkpoint with no trace. The
	// installed handler must carry an explicit generous timeout.
	if cfg.Stop[0].Timeout != stopHookTimeoutSeconds {
		t.Errorf("Stop timeout = %d, want %d", cfg.Stop[0].Timeout, stopHookTimeoutSeconds)
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}

	// First install
	n, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks: %v", err)
	}
	if n != 3 {
		t.Errorf("first install: installed %d hooks, want 3", n)
	}

	// Second install — idempotent, should return 0
	n, err = a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if n != 0 {
		t.Errorf("second install: installed %d hooks, want 0 (idempotent)", n)
	}
}

func TestInstallHooks_PreservesForeignHooks(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	// Pre-seed .agents/hooks.json with a foreign entry
	agentsDir := filepath.Join(tmpDir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := HooksFile{
		"safety-gate": {
			PreToolUse: []ToolHandler{
				{Matcher: "*", Hooks: []HookCommand{{Type: "command", Command: "safety-gate check"}}},
			},
		},
	}
	foreignBytes, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "hooks.json"), foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	a := &AntigravityAgent{}
	n, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if n != 3 {
		t.Errorf("installed %d hooks, want 3", n)
	}

	data, err := os.ReadFile(filepath.Join(agentsDir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f HooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}

	// Foreign entry must survive
	if _, ok := f["safety-gate"]; !ok {
		t.Error("foreign 'safety-gate' hook entry was removed")
	}

	// Entire entry must also exist
	if _, ok := f["entire"]; !ok {
		t.Error("missing 'entire' hook entry after install")
	}
}

func TestUninstallHooks_LeavesForeignHooks(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}

	// Install entire hooks first
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Pre-seed a foreign entry alongside the entire one
	agentsDir := filepath.Join(tmpDir, ".agents")
	data, err := os.ReadFile(filepath.Join(agentsDir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f HooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f["safety-gate"] = HookConfig{
		PreToolUse: []ToolHandler{
			{Matcher: "*", Hooks: []HookCommand{{Type: "command", Command: "safety-gate check"}}},
		},
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "hooks.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}

	// Uninstall
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}

	// Read back
	data, err = os.ReadFile(filepath.Join(agentsDir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after HooksFile
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("parse hooks.json after uninstall: %v", err)
	}

	// Foreign must survive
	if _, ok := after["safety-gate"]; !ok {
		t.Error("foreign 'safety-gate' entry was removed by UninstallHooks")
	}

	// Entire entry must be gone
	if _, ok := after["entire"]; ok {
		t.Error("'entire' hook entry still present after UninstallHooks")
	}
}

func TestInstallHooks_LocalDevWritesQuotedSubshell(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), true, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".agents", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f HooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	cmd := f["entire"].PreToolUse[0].Hooks[0].Command
	// The subshell must be quoted so paths with spaces don't break shell word-splitting.
	if !strings.Contains(cmd, `"$(git rev-parse --show-toplevel)"`) {
		t.Errorf("localDev command missing quoted subshell: %q", cmd)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}

	if a.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = true before install, want false")
	}

	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	if !a.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = false after install, want true")
	}
}

// TestAreHooksInstalled_ToleratesForeignEntryShapes pins detection tolerance:
// .agents/hooks.json is a shared, user-editable file, and foreign hook entries
// are free-form (agy only requires OUR entry to be well-shaped). A foreign
// entry whose fields don't match Entire's struct types (e.g. a string timeout)
// must not break detection of the entire entry — otherwise install succeeds
// while status/doctor permanently report "not installed".
func TestAreHooksInstalled_ToleratesForeignEntryShapes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Splice in a foreign entry with shapes that don't unmarshal into
	// Entire's handler structs: string timeout, numeric command.
	hooksPath := filepath.Join(tmpDir, ".agents", AgentsHooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["users-own-linter"] = json.RawMessage(`{"PreInvocation":[{"command":42,"timeout":"5s"}],"Stop":"not-a-list"}`)
	merged, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, merged, 0o600); err != nil {
		t.Fatal(err)
	}

	if !a.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = false with a malformed foreign entry present, want true")
	}
}

// TestInstallHooks_RespectsUserDisabledEntry pins that a user-set
// "enabled": false on the entire hooks entry (agy's documented per-entry
// disable knob) is a deliberate choice: a non-force reinstall must leave the
// entry untouched rather than rewriting it and silently re-arming tracking.
func TestInstallHooks_RespectsUserDisabledEntry(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// User disables the entry through agy's documented knob.
	hooksPath := filepath.Join(tmpDir, ".agents", AgentsHooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw["entire"], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["enabled"] = false
	entireBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw["entire"] = entireBytes
	merged, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, merged, 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks after disable: %v", err)
	}
	if n != 0 {
		t.Errorf("InstallHooks rewrote a user-disabled entry (n=%d), want 0", n)
	}

	after, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	var afterRaw map[string]json.RawMessage
	if err := json.Unmarshal(after, &afterRaw); err != nil {
		t.Fatal(err)
	}
	var afterCfg HookConfig
	if err := json.Unmarshal(afterRaw["entire"], &afterCfg); err != nil {
		t.Fatal(err)
	}
	if afterCfg.Enabled == nil || *afterCfg.Enabled {
		t.Error("user-set enabled:false was dropped — hooks silently re-armed")
	}

	// --force is the explicit override: it may rewrite (and re-arm) the entry.
	if _, err := a.InstallHooks(context.Background(), true, false); err != nil {
		t.Fatalf("InstallHooks --force: %v", err)
	}
}

// TestInstallHooks_IdempotentStillRepairsTitleTee guards the regression where
// the repo-hooks idempotency early-return skipped the global title-tee install,
// leaving Antigravity checkpoints without token counts after an upgrade or a
// failed first title install (and making the doctor's "re-run setup" hint a
// no-op). Re-running InstallHooks must repair the missing global slot even when
// the repo's .agents/hooks.json already matches.
func TestInstallHooks_IdempotentStillRepairsTitleTee(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("first InstallHooks: %v", err)
	}
	if !TitleTeeInstalled() {
		t.Fatal("title tee should be installed after the first InstallHooks")
	}

	// Simulate a missing/stale global slot while repo hooks remain correct.
	if err := UninstallTitleTee(); err != nil {
		t.Fatalf("UninstallTitleTee: %v", err)
	}
	if TitleTeeInstalled() {
		t.Fatal("precondition: title tee should be gone before the idempotent re-install")
	}

	// Second install hits the repo-hooks idempotency early-return, but must
	// still re-install the missing global title tee.
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if !TitleTeeInstalled() {
		t.Error("idempotent InstallHooks must repair the missing title tee")
	}
}
