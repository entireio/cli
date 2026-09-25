package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

func TestInstallHooks_FreshRepo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	n, err := a.InstallHooks(context.Background(), false)
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
	n, err := a.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks: %v", err)
	}
	if n != 3 {
		t.Errorf("first install: installed %d hooks, want 3", n)
	}

	// Second install — idempotent, should return 0
	n, err = a.InstallHooks(context.Background(), false)
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
	n, err := a.InstallHooks(context.Background(), false)
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
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
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

func TestAreHooksInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}

	if installed, err := a.AreHooksInstalled(context.Background()); err != nil || installed {
		t.Errorf("AreHooksInstalled() = (%v, %v) before install, want (false, nil)", installed, err)
	}

	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	if installed, err := a.AreHooksInstalled(context.Background()); err != nil || !installed {
		t.Errorf("AreHooksInstalled() = (%v, %v) after install, want (true, nil)", installed, err)
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
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
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

	if installed, err := a.AreHooksInstalled(context.Background()); err != nil || !installed {
		t.Errorf("AreHooksInstalled() = (%v, %v) with a malformed foreign entry present, want (true, nil)", installed, err)
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
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
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

	n, err := a.InstallHooks(context.Background(), false)
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
	if _, err := a.InstallHooks(context.Background(), true); err != nil {
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
	putFakeAgyOnPath(t)

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
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
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("second InstallHooks: %v", err)
	}
	if !TitleTeeInstalled() {
		t.Error("idempotent InstallHooks must repair the missing title tee")
	}
}

// TestHooks_RefuseSymlinkedAgentsDir pins the three operations that used to
// resolve .agents through a checked-in symlink with bare os.ReadFile /
// os.MkdirAll / a joined-path write. A repository shipping
// `.agents -> /somewhere/else` got hooks.json created and rewritten outside the
// worktree from InstallHooks, deleted-from outside it from UninstallHooks, and
// AreHooksInstalled read the far end as its own answer. It is reachable
// without the user naming antigravity: DetectPresence is AreHooksInstalled.
func TestHooks_RefuseSymlinkedAgentsDir(t *testing.T) {
	outside := t.TempDir()
	worktree := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(worktree, ".agents")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The link's far end already holds an entire entry, so a followed read has
	// something to report and a followed write has something to destroy.
	planted := filepath.Join(outside, AgentsHooksFileName)
	plantedBody := `{"entire":{"stop":[{"type":"command","command":"entire hooks antigravity stop"}]}}`
	if err := os.WriteFile(planted, []byte(plantedBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(worktree)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}

	if _, err := a.InstallHooks(context.Background(), false); !errors.Is(err, osroot.ErrSymlinkedPath) {
		t.Errorf("InstallHooks err = %v, want osroot.ErrSymlinkedPath", err)
	}

	// An unreadable answer is not "no hooks": a caller deciding whether hooks
	// can be left alone must not be told the far end's content is ours.
	installed, err := a.AreHooksInstalled(context.Background())
	if !errors.Is(err, osroot.ErrSymlinkedPath) {
		t.Errorf("AreHooksInstalled err = %v, want osroot.ErrSymlinkedPath", err)
	}
	if installed {
		t.Error("AreHooksInstalled followed the link and claimed the planted entry")
	}

	if err := a.UninstallHooks(context.Background()); !errors.Is(err, osroot.ErrSymlinkedPath) {
		t.Errorf("UninstallHooks err = %v, want osroot.ErrSymlinkedPath", err)
	}

	got, err := os.ReadFile(planted)
	if err != nil {
		t.Fatalf("the file at the link's far end must survive untouched: %v", err)
	}
	if string(got) != plantedBody {
		t.Errorf("hooks.json outside the worktree was rewritten:\n%s", got)
	}
}

// TestHooks_RefuseSymlinkedHooksFile: the leaf is refused too. os.Root blocks a
// link that escapes the worktree but follows one pointing elsewhere inside it,
// and accepting the leaf would let `.agents/hooks.json -> ../victim.json`
// redirect both the merge read and the subsequent write.
func TestHooks_RefuseSymlinkedHooksFile(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(worktree, "victim.json")
	if err := os.WriteFile(victim, []byte(`{"mine":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "victim.json"), filepath.Join(worktree, ".agents", AgentsHooksFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(worktree)
	t.Setenv(configDirEnv, t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false); !errors.Is(err, osroot.ErrSymlinkedPath) {
		t.Errorf("InstallHooks err = %v, want osroot.ErrSymlinkedPath", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"mine":true}` {
		t.Errorf("the link's target was rewritten:\n%s", got)
	}
	info, err := os.Lstat(filepath.Join(worktree, ".agents", AgentsHooksFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the user's symlink was replaced by a regular file")
	}
}

// agy runs hook commands through cmd.exe /C on Windows, so the installed
// command must be the bare direct-shell wrapper there: no `sh -c` (cmd.exe
// tears its redirects apart) and no nested `cmd.exe /d /s /c "…"` block (cmd.exe
// /C takes the quoted block as one program name). Confirmed on Windows 11 with
// agy 1.2.7 in trail 444.
func TestBuildEntireHookConfig_WindowsHostUsesDirectCmdWrapper(t *testing.T) {
	t.Parallel()

	cfg := buildEntireHookConfigForHost(true)
	commands := []string{
		cfg.PreToolUse[0].Hooks[0].Command,
		cfg.PreInvocation[0].Command,
		cfg.Stop[0].Command,
	}
	for _, cmd := range commands {
		if strings.HasPrefix(cmd, "sh -c") {
			t.Errorf("Windows host got the sh wrapper: %s", cmd)
		}
		if strings.HasPrefix(cmd, "cmd.exe") {
			t.Errorf("Windows host got the nested cmd.exe wrapper, which agy's own cmd.exe /C rejects: %s", cmd)
		}
		if !strings.HasPrefix(cmd, "where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks antigravity ") {
			t.Errorf("unexpected Windows hook command shape: %s", cmd)
		}
		if !agent.IsManagedHookCommand(cmd) {
			t.Errorf("uninstall and drift detection must still recognise the Windows command as Entire's: %s", cmd)
		}
	}

	posix := buildEntireHookConfigForHost(false)
	if !strings.HasPrefix(posix.Stop[0].Command, "sh -c ") {
		t.Errorf("POSIX host must keep the sh wrapper, got %s", posix.Stop[0].Command)
	}
}

// A hooks.json that never carried an Entire entry is the user's file: uninstall
// must not rewrite it (re-indented, keys reordered) for no change of ours.
func TestUninstallHooks_LeavesAForeignOnlyFileUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv(configDirEnv, t.TempDir())

	hooksPath := filepath.Join(dir, ".agents", AgentsHooksFileName)
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		t.Fatal(err)
	}
	// Deliberately odd formatting: four-space indent, keys out of sorted order.
	original := "{\n    \"zeta\": {\"Stop\": [{\"type\": \"command\", \"command\": \"echo z\"}]},\n    \"alpha\": {\"PreInvocation\": [{\"type\": \"command\", \"command\": \"echo a\"}]}\n}\n"
	if err := os.WriteFile(hooksPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&AntigravityAgent{}).UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("foreign-only hooks.json was rewritten:\n%s", got)
	}
}

// HooksEntryMatchesHost is doctor's zero-cost replacement for the probe: it
// must call a fresh install current, a foreign-shaped entry stale, and a
// user-disabled entry current (install leaves it alone too).
func TestHooksEntryMatchesHost(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv(configDirEnv, t.TempDir())
	a := &AntigravityAgent{}
	ctx := context.Background()

	installed, current, err := a.HooksEntryMatchesHost(ctx)
	if err != nil || installed || current {
		t.Fatalf("no file: installed=%v current=%v err=%v; want false,false,nil", installed, current, err)
	}

	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatal(err)
	}
	installed, current, err = a.HooksEntryMatchesHost(ctx)
	if err != nil || !installed || !current {
		t.Fatalf("fresh install: installed=%v current=%v err=%v; want true,true,nil", installed, current, err)
	}

	hooksPath := filepath.Join(dir, ".agents", AgentsHooksFileName)
	stale := `{"entire":{"PreInvocation":[{"type":"command","command":"sh -c 'exec entire hooks antigravity pre-invocation'"}]}}`
	if err := os.WriteFile(hooksPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, current, err = a.HooksEntryMatchesHost(ctx)
	if err != nil || !installed || current {
		t.Fatalf("foreign shape: installed=%v current=%v err=%v; want true,false,nil", installed, current, err)
	}

	disabled := `{"entire":{"enabled":false,"PreInvocation":[{"type":"command","command":"echo off"}]}}`
	if err := os.WriteFile(hooksPath, []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, current, err = a.HooksEntryMatchesHost(ctx)
	if err != nil || !installed || !current {
		t.Fatalf("user-disabled: installed=%v current=%v err=%v; want true,true,nil", installed, current, err)
	}
}

// "enabled": false is a deliberate opt-out that InstallHooks already honours.
// It has to be readable on its own, separately from installed-ness: the entry
// stays genuinely present for agent detection and `entire agent list`, and only
// `entire doctor` wants to go quiet about it.
func TestHooksDisabled_SeparatesOptOutFromPresence(t *testing.T) {
	for name, tc := range map[string]struct {
		entry        string
		wantDisabled bool
	}{
		"explicitly disabled": {`{"enabled":false,"Stop":[{"type":"command","command":"entire hooks antigravity stop"}]}`, true},
		"explicitly enabled":  {`{"enabled":true,"Stop":[{"type":"command","command":"entire hooks antigravity stop"}]}`, false},
		"enabled unset":       {`{"Stop":[{"type":"command","command":"entire hooks antigravity stop"}]}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			// No t.Parallel — uses t.Chdir and t.Setenv
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv(configDirEnv, t.TempDir())

			agentsDir := filepath.Join(tmpDir, ".agents")
			if err := os.MkdirAll(agentsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			hooks := `{"entire":` + tc.entry + `}`
			if err := os.WriteFile(filepath.Join(agentsDir, AgentsHooksFileName), []byte(hooks), 0o600); err != nil {
				t.Fatal(err)
			}

			a := &AntigravityAgent{}
			disabled, err := a.HooksDisabled(context.Background())
			if err != nil {
				t.Fatalf("HooksDisabled: %v", err)
			}
			if disabled != tc.wantDisabled {
				t.Errorf("HooksDisabled() = %v, want %v", disabled, tc.wantDisabled)
			}

			// Present either way: detection and `entire agent list` describe
			// what is on disk, so the opt-out must not make the entry vanish.
			installed, err := a.AreHooksInstalled(context.Background())
			if err != nil {
				t.Fatalf("AreHooksInstalled: %v", err)
			}
			if !installed {
				t.Error("AreHooksInstalled() = false; a disabled entry is still present")
			}
		})
	}
}

// putFakeAgyOnPath makes InstallHooks see an installed agy: the global title
// slot is only claimed on machines where `agy` resolves on PATH, so a test that
// expects the tee to be installed has to stand one up. The file is never run.
func putFakeAgyOnPath(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	name := antigravityBinaryName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The title slot lives in agy's machine-global settings.json, so a repo-local
// `entire agent add antigravity` must not claim it on a machine that has never
// run agy. `entire doctor` gates its matching check on the same lookup.
func TestInstallHooks_SkipsTitleTeeWhenAgyIsAbsent(t *testing.T) {
	// No t.Parallel — uses t.Chdir and t.Setenv
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	cfgDir := t.TempDir()
	t.Setenv(configDirEnv, cfgDir)
	// An empty PATH is the "agy was never installed here" machine.
	t.Setenv("PATH", t.TempDir())

	a := &AntigravityAgent{}
	if _, err := a.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Repo hooks still installed: the global slot is a separate concern.
	installed, err := a.AreHooksInstalled(context.Background())
	if err != nil {
		t.Fatalf("AreHooksInstalled: %v", err)
	}
	if !installed {
		t.Error("repo hooks must install even when agy is absent")
	}

	if TitleTeeInstalled() {
		t.Error("the machine-global title slot was claimed on a machine with no agy")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, agySettingsFileName)); err == nil {
		t.Error("agy's global settings.json was created on a machine that never ran agy")
	}
}
