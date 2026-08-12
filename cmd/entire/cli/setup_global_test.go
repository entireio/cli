package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

func TestRunEnableGlobalMode_OutsideGitRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Chdir(t.TempDir()) // deliberately not a git repository
	t.Cleanup(settings.ClearGlobalModeCache)

	var buf bytes.Buffer
	if err := runEnableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatalf("enable --global must work outside a git repo: %v", err)
	}
	us, err := settings.LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if us.Global == nil || !us.Global.Enabled {
		t.Fatalf("global.enabled not persisted: %+v", us.Global)
	}
	if !strings.Contains(buf.String(), "Global tracking enabled.") {
		t.Fatalf("missing confirmation, got: %q", buf.String())
	}
}

func TestRunEnableGlobalMode_PreservesExcludeLists(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(settings.ClearGlobalModeCache)
	content := `{"global":{"enabled":false,"exclude_paths":["~/oss/**"],"exclude_origins":["github.com/acme/*"]}}`
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runEnableGlobalMode(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	us, err := settings.LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !us.Global.Enabled || len(us.Global.ExcludePaths) != 1 || len(us.Global.ExcludeOrigins) != 1 {
		t.Fatalf("enable --global dropped existing exclude lists: %+v", us.Global)
	}
}

func TestRunDisableGlobalMode_AnswerIsDurable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(settings.ClearGlobalModeCache)

	var buf bytes.Buffer
	if err := runDisableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	us, err := settings.LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Global != nil with Enabled=false is the durable "no": the ask-once
	// wizard treats it as configured and never re-asks.
	if us.Global == nil || us.Global.Enabled {
		t.Fatalf("disable --global must persist a durable false, got %+v", us.Global)
	}

	var wizardOut bytes.Buffer
	maybeAskGlobalTracking(t.Context(), &wizardOut, EnableOptions{})
	if wizardOut.Len() != 0 {
		t.Fatalf("configured answer must silence the wizard, got: %q", wizardOut.String())
	}
}

func TestMaybeAskGlobalTracking_NonInteractiveHintsWithoutWriting(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)

	var buf bytes.Buffer
	// Under go test CanPromptInteractively() is false, so this exercises the
	// non-interactive branch: hint, no prompt, no write.
	maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
	if !strings.Contains(buf.String(), "entire enable --global") {
		t.Fatalf("expected the enable --global hint, got: %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, settings.UserSettingsFileName)); !os.IsNotExist(err) {
		t.Fatalf("non-interactive path must not write the user settings file (err=%v)", err)
	}
}

func TestMaybeAskGlobalTracking_MalformedFileWarnsAndIsLeftAlone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
	out := buf.String()
	// One warning line naming the file and the remedies — not full silence
	// (which hid the problem) and not a prompt or a rewrite.
	if !strings.Contains(out, "Warning:") || !strings.Contains(out, settings.UserSettingsPath()) {
		t.Fatalf("malformed file must produce a warning naming the file, got: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one warning line, got: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(cfg, settings.UserSettingsFileName))
	if err != nil || string(data) != `{broken` {
		t.Fatalf("malformed file must not be rewritten (data=%q err=%v)", data, err)
	}
}

func TestMaybeMigrateGlobalRuntimeData_MovesRoutedFiles(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
		session.ClearGitCommonDirCache()
		settings.ClearGlobalModeCache()
	})

	// Routed runtime data as the invisible tier lays it out (namespaced per
	// worktree; the main worktree's key hashes the empty worktree ID), plus
	// one file that already exists in the worktree target (must be skipped,
	// not clobbered).
	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	source := filepath.Join(dir, filepath.FromSlash(sourceRel))
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/prompt.txt", "routed prompt")
	testutil.WriteFile(t, dir, sourceRel+"/logs/entire.log", "routed log")
	testutil.WriteFile(t, dir, sourceRel+"/tmp/scratch.txt", "routed tmp")
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/conflict.txt", "routed version")
	testutil.WriteFile(t, dir, ".entire/metadata/sess-1/conflict.txt", "worktree version")

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)

	for _, rel := range []string{
		".entire/metadata/sess-1/prompt.txt",
		".entire/logs/entire.log",
		".entire/tmp/scratch.txt",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected %s after migration: %v", rel, err)
		}
	}
	// The conflicting target keeps the worktree content.
	data, err := os.ReadFile(filepath.Join(dir, ".entire", "metadata", "sess-1", "conflict.txt"))
	if err != nil || string(data) != "worktree version" {
		t.Errorf("conflicting target must be kept (data=%q err=%v)", data, err)
	}
	// Moved files leave the source; the skipped conflict file stays behind.
	if _, err := os.Stat(filepath.Join(source, "metadata", "sess-1", "prompt.txt")); !os.IsNotExist(err) {
		t.Errorf("moved file still present in source (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(source, "metadata", "sess-1", "conflict.txt")); err != nil {
		t.Errorf("skipped file must stay in source: %v", err)
	}
	// Emptied source subtrees are removed.
	if _, err := os.Stat(filepath.Join(source, "logs")); !os.IsNotExist(err) {
		t.Errorf("emptied source logs dir not removed (err=%v)", err)
	}
	if !strings.Contains(buf.String(), "Moved 3") {
		t.Errorf("expected one-line summary with moved count, got: %q", buf.String())
	}
}

// TestMaybeMigrateGlobalRuntimeData_MigratesOnlyCurrentWorktreeNamespace pins
// the per-worktree scope of the migration: enabling one worktree moves ONLY
// that worktree's invisible-routing namespace into its .entire; a sibling
// worktree's in-flight runtime data stays untouched in the git common dir.
func TestMaybeMigrateGlobalRuntimeData_MigratesOnlyCurrentWorktreeNamespace(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	linked := filepath.Join(dir, ".worktrees", "wt1")
	wtAdd := exec.CommandContext(t.Context(), "git", "worktree", "add", linked)
	wtAdd.Dir = dir
	if out, err := wtAdd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	t.Chdir(linked)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
		session.ClearGitCommonDirCache()
	})

	mainNS := ".git/entire/worktree/" + paths.HashWorktreeID("")
	linkedNS := ".git/entire/worktree/" + paths.HashWorktreeID("wt1")
	testutil.WriteFile(t, dir, mainNS+"/metadata/sess-main/prompt.txt", "main worktree session")
	testutil.WriteFile(t, dir, linkedNS+"/metadata/sess-linked/prompt.txt", "linked worktree session")

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)

	// The enabling (linked) worktree's namespace moved into ITS .entire.
	data, err := os.ReadFile(filepath.Join(linked, ".entire", "metadata", "sess-linked", "prompt.txt"))
	if err != nil || string(data) != "linked worktree session" {
		t.Errorf("linked worktree's routed data must migrate into its .entire (data=%q err=%v)", data, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(linkedNS))); !os.IsNotExist(statErr) {
		t.Errorf("linked worktree's emptied namespace must be removed (err=%v)", statErr)
	}
	// The sibling (main) worktree's namespace is untouched.
	data, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(mainNS), "metadata", "sess-main", "prompt.txt"))
	if err != nil || string(data) != "main worktree session" {
		t.Errorf("main worktree's routed data must stay in place (data=%q err=%v)", data, err)
	}
	// Nothing leaked into the main worktree's checkout either.
	if _, statErr := os.Stat(filepath.Join(dir, ".entire")); !os.IsNotExist(statErr) {
		t.Errorf("migration must not create .entire in the sibling worktree (err=%v)", statErr)
	}
	if !strings.Contains(buf.String(), "Moved 1") {
		t.Errorf("expected a one-file summary, got: %q", buf.String())
	}
}

func TestMaybeMigrateGlobalRuntimeData_NoTriggerIsSilentNoop(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		session.ClearGitCommonDirCache()
	})

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)
	if buf.Len() != 0 {
		t.Fatalf("nothing to migrate must print nothing, got: %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".entire")); !os.IsNotExist(err) {
		t.Fatalf("no-op migration must not create .entire (err=%v)", err)
	}
}

// TestRunDisable_GloballyTrackedRepoTakeover is the reviewer's exact repro
// for the stranded-data bug: `entire disable` in a globally tracked repo
// creates the discriminator settings file, which flips invisible routing to
// the worktree. Without the takeover the routed runtime data stayed under
// .git forever (no later enable migrated it) and the untracked .entire/
// content dirtied git status. Disable itself must migrate the data, write
// the gitignore, and clear the once-per-clone lazy-setup marker.
func TestRunDisable_GloballyTrackedRepoTakeover(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	clearGlobalRoutingCaches(t)

	// Globally tracked state: routed runtime data plus the lazy-setup marker.
	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/prompt.txt", "routed prompt")
	if err := settings.ModifyClonePreferences(t.Context(), func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runDisable(t.Context(), &buf, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// Routed data migrated into the worktree .entire.
	data, err := os.ReadFile(filepath.Join(dir, ".entire", "metadata", "sess-1", "prompt.txt"))
	if err != nil || string(data) != "routed prompt" {
		t.Errorf("routed data must be migrated on disable (data=%q err=%v)", data, err)
	}
	// .gitignore present, so the migrated files never dirty git status.
	if _, err := os.Stat(filepath.Join(dir, ".entire", ".gitignore")); err != nil {
		t.Errorf(".entire/.gitignore must exist after the takeover: %v", err)
	}
	// Porcelain shows only the expected untracked .entire/ entry.
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "?? .entire/" {
		t.Errorf("porcelain must show only the .entire/ entry, got:\n%s", got)
	}
	// Marker cleared: repo-level machinery owns this clone now.
	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if prefs.GlobalSetupCompleted {
		t.Error("global_setup_completed marker must be cleared by the takeover")
	}
	// And the disable itself still landed.
	if !strings.Contains(buf.String(), "Entire is now disabled") {
		t.Errorf("missing disable confirmation, got: %q", buf.String())
	}
}

// TestRunUninstall_ClearsGlobalSetupMarker pins the enable→uninstall→re-track
// loop: uninstall removes the git hooks, so the clone's "lazy setup already
// converged" marker must be cleared with them — otherwise a re-globally-
// tracked clone short-circuits MaybeEnsureGlobalSetup forever and never gets
// hooks again.
func TestRunUninstall_ClearsGlobalSetupMarker(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	clearGlobalRoutingCaches(t)

	// Repo-level setup, with the marker still latched from an earlier
	// globally tracked phase (the pre-fix state every takeover now clears).
	testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true}`)
	if _, err := strategy.InstallGitHook(t.Context(), true, false, false); err != nil {
		t.Fatal(err)
	}
	if err := settings.ModifyClonePreferences(t.Context(), func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if err := runUninstall(t.Context(), &out, &errW, true); err != nil {
		t.Fatalf("uninstall: %v (stderr: %s)", err, errW.String())
	}
	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if prefs.GlobalSetupCompleted {
		t.Fatal("uninstall must clear the global_setup_completed marker")
	}
	if strategy.IsGitHookInstalled(t.Context()) {
		t.Fatal("uninstall must remove the git hooks")
	}

	// The clone is globally tracked again: the lazy setup must reconverge
	// instead of latching on the stale marker.
	clearGlobalRoutingCaches(t)
	strategy.MaybeEnsureGlobalSetup(t.Context())
	if !strategy.IsGitHookInstalled(t.Context()) {
		t.Fatal("MaybeEnsureGlobalSetup must reinstall git hooks after uninstall cleared the marker")
	}
}

// clearGlobalRoutingCaches resets every process-level cache a global-tier
// test can leave warm (and registers the same reset as cleanup), so tests
// observe the on-disk state they just seeded.
func clearGlobalRoutingCaches(t *testing.T) {
	t.Helper()
	reset := func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
		session.ClearGitCommonDirCache()
		settings.ClearGlobalModeCache()
	}
	reset()
	t.Cleanup(reset)
}

func TestEnableCmd_GlobalRejectsRepoScopedFlags(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"--global", "--agent", "claude-code"},
		{"--global", "--yes"},
		{"--global", "--local"},
		{"--global", "--init-repo"},
	} {
		cmd := newEnableCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("enable %v must be rejected by cobra", args)
		}
	}
	// The rejection must happen before any action: no user settings written.
	if _, err := os.Stat(settings.UserSettingsPath()); !os.IsNotExist(err) {
		t.Fatalf("rejected combinations must not write the user settings file (err=%v)", err)
	}
}

func TestDisableCmd_GlobalRejectsRepoScopedFlags(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"--global", "--uninstall"},
		{"--global", "--project"},
		{"--global", "--force"},
	} {
		cmd := newDisableCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("disable %v must be rejected by cobra", args)
		}
	}
	if _, err := os.Stat(settings.UserSettingsPath()); !os.IsNotExist(err) {
		t.Fatalf("rejected combinations must not write the user settings file (err=%v)", err)
	}
}

// TestMaybeMigrateGlobalRuntimeData_CrossDeviceFallback forces the rename to
// fail (as it does with EXDEV when .git and the worktree live on different
// filesystems) and pins the copy+remove fallback.
func TestMaybeMigrateGlobalRuntimeData_CrossDeviceFallback(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		session.ClearGitCommonDirCache()
	})

	migrateRenameFile = func(string, string) error { return errors.New("simulated EXDEV") }
	t.Cleanup(func() { migrateRenameFile = os.Rename })

	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/prompt.txt", "routed prompt")

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)

	data, err := os.ReadFile(filepath.Join(dir, ".entire", "metadata", "sess-1", "prompt.txt"))
	if err != nil || string(data) != "routed prompt" {
		t.Fatalf("copy fallback must move the file (data=%q err=%v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceRel), "metadata", "sess-1", "prompt.txt")); !os.IsNotExist(err) {
		t.Fatalf("copy fallback must remove the source file (err=%v)", err)
	}
	if !strings.Contains(buf.String(), "✓ Moved 1") {
		t.Fatalf("fallback move must count as moved, got: %q", buf.String())
	}
}

// TestMaybeMigrateGlobalRuntimeData_FailureSummaryNamesSource pins the
// failure summary shape: no checkmark, and the source path so leftovers are
// findable.
func TestMaybeMigrateGlobalRuntimeData_FailureSummaryNamesSource(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		session.ClearGitCommonDirCache()
	})

	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	testutil.WriteFile(t, dir, sourceRel+"/logs/entire.log", "routed log")
	// Block the destination: .entire/logs as a regular file makes MkdirAll fail.
	testutil.WriteFile(t, dir, ".entire/logs", "not a directory")

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)

	out := buf.String()
	if strings.Contains(out, "✓") {
		t.Errorf("failure summary must not carry the success checkmark: %q", out)
	}
	source := filepath.Join(dir, filepath.FromSlash(sourceRel))
	if !strings.Contains(out, "1 could not be moved") || !strings.Contains(out, source) {
		t.Errorf("failure summary must count failures and name %s, got: %q", source, out)
	}
	if _, err := os.Stat(filepath.Join(source, "logs", "entire.log")); err != nil {
		t.Errorf("failed file must stay in source: %v", err)
	}
}
