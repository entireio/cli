package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"

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
	// two files that already exist in the worktree target: a DIVERGENT one
	// (kept, source counted failed so it stays findable) and an identical
	// duplicate (source dropped, counted already-present).
	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	source := filepath.Join(dir, filepath.FromSlash(sourceRel))
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/prompt.txt", "routed prompt")
	testutil.WriteFile(t, dir, sourceRel+"/logs/entire.log", "routed log")
	testutil.WriteFile(t, dir, sourceRel+"/tmp/scratch.txt", "routed tmp")
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/conflict.txt", "routed version")
	testutil.WriteFile(t, dir, ".entire/metadata/sess-1/conflict.txt", "worktree version")
	testutil.WriteFile(t, dir, sourceRel+"/metadata/sess-1/dup.txt", "same bytes")
	testutil.WriteFile(t, dir, ".entire/metadata/sess-1/dup.txt", "same bytes")

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
	// Moved files leave the source; the divergent conflict file stays behind.
	if _, err := os.Stat(filepath.Join(source, "metadata", "sess-1", "prompt.txt")); !os.IsNotExist(err) {
		t.Errorf("moved file still present in source (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(source, "metadata", "sess-1", "conflict.txt")); err != nil {
		t.Errorf("divergent file must stay in source: %v", err)
	}
	// The identical duplicate is dropped from the source, not stranded.
	if _, err := os.Stat(filepath.Join(source, "metadata", "sess-1", "dup.txt")); !os.IsNotExist(err) {
		t.Errorf("identical duplicate still present in source (err=%v)", err)
	}
	// Emptied source subtrees are removed.
	if _, err := os.Stat(filepath.Join(source, "logs")); !os.IsNotExist(err) {
		t.Errorf("emptied source logs dir not removed (err=%v)", err)
	}
	for _, want := range []string{"Moved 3", "1 already present", "1 could not be moved (left in "} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary missing %q, got: %q", want, buf.String())
		}
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

// TestMaybeMigrateGlobalRuntimeData_DefersWhileSessionActive pins the
// active-writer guard: an ACTIVE session in THIS worktree defers the whole
// migration (a hook may be mid-write under the routed dir), while idle
// sessions here and active sessions of other worktrees do not.
func TestMaybeMigrateGlobalRuntimeData_DefersWhileSessionActive(t *testing.T) {
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
	})

	store, err := session.NewStateStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	saveState := func(id string, phase session.Phase, worktree string) {
		t.Helper()
		if err := store.Save(t.Context(), &session.State{
			SessionID: id, Phase: phase, WorktreePath: worktree, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	saveState("sess-idle-here", session.PhaseIdle, dir)
	saveState("sess-active-elsewhere", session.PhaseActive, filepath.Join(dir, "elsewhere"))

	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	testutil.WriteFile(t, dir, sourceRel+"/logs/first.log", "routed")
	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)
	if !strings.Contains(buf.String(), "Moved 1") {
		t.Fatalf("idle/foreign sessions must not defer migration, got: %q", buf.String())
	}

	saveState("sess-active-here", session.PhaseActive, dir)
	testutil.WriteFile(t, dir, sourceRel+"/logs/second.log", "routed")
	buf.Reset()
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)
	if !strings.Contains(buf.String(), "deferred") {
		t.Fatalf("expected a deferral line, got: %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceRel), "logs", "second.log")); err != nil {
		t.Errorf("deferred migration must leave the routed file in place: %v", err)
	}
}

// A hook that starts DURING migration must wait until every routed file has
// landed. The production hook dispatcher takes the same worktree-scoped lock
// before lifecycle work, closing the pre-check/move TOCTOU window.
func TestMaybeMigrateGlobalRuntimeData_BlocksHookDuringMigration(t *testing.T) {
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
	})

	sourceRel := ".git/entire/worktree/" + paths.HashWorktreeID("")
	testutil.WriteFile(t, dir, sourceRel+"/logs/entire.log", "routed")

	moveStarted := make(chan struct{})
	allowMove := make(chan struct{})
	restore := migrateMoveFile
	migrateMoveFile = func(oldpath, newpath string) (bool, error) {
		close(moveStarted)
		<-allowMove
		return restore(oldpath, newpath)
	}
	t.Cleanup(func() { migrateMoveFile = restore })

	var buf bytes.Buffer
	migrationDone := make(chan struct{})
	go func() {
		maybeMigrateGlobalRuntimeData(t.Context(), &buf)
		close(migrationDone)
	}()
	<-moveStarted

	hookAcquired := make(chan struct{})
	hookDone := make(chan struct{})
	go func() {
		release, lockErr := acquireGlobalRuntimeMigrationGate(t.Context(), dir)
		if lockErr != nil {
			t.Errorf("acquire hook migration gate: %v", lockErr)
			close(hookDone)
			return
		}
		close(hookAcquired)
		release()
		close(hookDone)
	}()

	select {
	case <-hookAcquired:
		t.Fatal("hook acquired migration gate while routed-file move was in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(allowMove)
	<-migrationDone
	<-hookDone

	if _, err := os.Stat(filepath.Join(dir, ".entire", "logs", "entire.log")); err != nil {
		t.Errorf("file should have migrated before hook gate released: %v", err)
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

// isolateUserHome points os.UserHomeDir at a temp dir (HOME on Unix,
// USERPROFILE on Windows) so end-to-end enable/disable runs can never touch
// the developer's real user-level files.
func isolateUserHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// TestEnableCmd_GlobalEndToEnd drives the full cobra command, pinning the two
// success shapes of `entire enable --global`: outside a git repository it
// still exits 0 and writes the user settings file; inside a repository it
// stays machine-wide only — no .entire/, no git hooks.
func TestEnableCmd_GlobalEndToEnd(t *testing.T) {
	t.Run("outside a git repository", func(t *testing.T) {
		isolateUserHome(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Chdir(t.TempDir()) // deliberately not a git repository
		t.Cleanup(settings.ClearGlobalModeCache)

		cmd := newEnableCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--global"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("enable --global outside a repo must succeed: %v\n%s", err, out.String())
		}
		us, err := settings.LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if us.Global == nil || !us.Global.Enabled {
			t.Fatalf("global.enabled not persisted: %+v", us.Global)
		}
	})

	t.Run("inside a repository writes no repo files", func(t *testing.T) {
		isolateUserHome(t)
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		t.Chdir(dir)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		clearGlobalRoutingCaches(t)

		cmd := newEnableCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--global"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("enable --global inside a repo must succeed: %v\n%s", err, out.String())
		}
		if _, err := os.Stat(filepath.Join(dir, ".entire")); !os.IsNotExist(err) {
			t.Fatalf("--global must not create .entire/ (err=%v)", err)
		}
		if strategy.IsGitHookInstalled(t.Context()) {
			t.Fatal("--global must not install repo git hooks")
		}
	})
}

// TestDisableCmd_GlobalEndToEnd mirrors the enable pin for `entire disable
// --global`: exit 0 in and outside a repo, a durable false persisted, and no
// repo-level files touched.
func TestDisableCmd_GlobalEndToEnd(t *testing.T) {
	t.Run("outside a git repository", func(t *testing.T) {
		isolateUserHome(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Chdir(t.TempDir())
		t.Cleanup(settings.ClearGlobalModeCache)

		cmd := newDisableCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--global"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("disable --global outside a repo must succeed: %v\n%s", err, out.String())
		}
		us, err := settings.LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if us.Global == nil || us.Global.Enabled {
			t.Fatalf("disable --global must persist a durable false, got %+v", us.Global)
		}
	})

	t.Run("inside a repository writes no repo files", func(t *testing.T) {
		isolateUserHome(t)
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		t.Chdir(dir)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		clearGlobalRoutingCaches(t)

		cmd := newDisableCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--global"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("disable --global inside a repo must succeed: %v\n%s", err, out.String())
		}
		if _, err := os.Stat(filepath.Join(dir, ".entire")); !os.IsNotExist(err) {
			t.Fatalf("--global must not create .entire/ (err=%v)", err)
		}
	})
}

// TestEnableCmd_GlobalRefusesUnknownKeys pins the strict-load protection:
// `enable --global` against a settings file written by a newer entire (an
// unknown key) must fail with an error naming the file, and must leave the
// file byte-identical — a blind read-modify-write would silently drop the
// newer binary's keys.
func TestEnableCmd_GlobalRefusesUnknownKeys(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Chdir(t.TempDir())
	original := `{"global":{"enabled":false},"future_key":{"answer":42}}`
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newEnableCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--global"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("enable --global must refuse a file with unknown keys")
	}
	if !strings.Contains(err.Error(), settings.UserSettingsPath()) {
		t.Fatalf("error must name the settings file, got: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(cfg, settings.UserSettingsFileName))
	if readErr != nil || string(data) != original {
		t.Fatalf("refused write must leave the file byte-identical (data=%q err=%v)", data, readErr)
	}
}

// TestMaybeAskGlobalTracking_YesSkipsPromptEvenOnTTY pins the --yes disjunct:
// even when a TTY is available (forced via ENTIRE_TEST_TTY=1), --yes must
// take the hint path — no prompt, no write.
func TestMaybeAskGlobalTracking_YesSkipsPromptEvenOnTTY(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	restore := askGlobalTrackingConfirm
	askGlobalTrackingConfirm = func(context.Context) (bool, error) {
		t.Error("--yes must never reach the interactive prompt")
		return false, nil
	}
	t.Cleanup(func() { askGlobalTrackingConfirm = restore })

	var buf bytes.Buffer
	maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{Yes: true})
	if !strings.Contains(buf.String(), "entire enable --global") {
		t.Fatalf("expected the enable --global hint, got: %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, settings.UserSettingsFileName)); !os.IsNotExist(err) {
		t.Fatalf("--yes must not write the user settings file (err=%v)", err)
	}
}

// TestMaybeAskGlobalTracking_AnswerPersistence pins the wizard's persistence
// contract through the prompt seam: a "No" is durable (saved, never re-asked)
// and a cancelled prompt saves nothing (a later enable asks again).
func TestMaybeAskGlobalTracking_AnswerPersistence(t *testing.T) {
	t.Run("no is durable", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		t.Cleanup(settings.ClearGlobalModeCache)
		restore := askGlobalTrackingConfirm
		askGlobalTrackingConfirm = func(context.Context) (bool, error) { return false, nil }
		t.Cleanup(func() { askGlobalTrackingConfirm = restore })

		var buf bytes.Buffer
		maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
		us, err := settings.LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if us.Global == nil || us.Global.Enabled {
			t.Fatalf("answered no must persist a durable false, got %+v", us.Global)
		}

		// Configured (either answer) means never re-ask.
		askGlobalTrackingConfirm = func(context.Context) (bool, error) {
			t.Error("a configured answer must never re-prompt")
			return false, nil
		}
		var again bytes.Buffer
		maybeAskGlobalTracking(t.Context(), &again, EnableOptions{})
		if again.Len() != 0 {
			t.Fatalf("configured answer must silence the wizard, got: %q", again.String())
		}
	})

	t.Run("mid-prompt configuration is not overturned", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		t.Cleanup(settings.ClearGlobalModeCache)
		restore := askGlobalTrackingConfirm
		askGlobalTrackingConfirm = func(context.Context) (bool, error) {
			// While this prompt is open, another terminal answers the
			// machine-wide question with an explicit `disable --global`.
			if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":false}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return true, nil // this prompt's "yes" arrives too late
		}
		t.Cleanup(func() { askGlobalTrackingConfirm = restore })

		var buf bytes.Buffer
		maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
		us, err := settings.LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Explicit off is durable: the by-then-invalid prompt answer must
		// not be persisted over the configuration made during the window.
		if us.Global == nil || us.Global.Enabled {
			t.Fatalf("mid-prompt explicit answer must win, got %+v", us.Global)
		}
		if strings.Contains(buf.String(), "Global tracking enabled") {
			t.Fatalf("superseded answer must not report success, got: %q", buf.String())
		}
	})

	t.Run("cancel saves nothing", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		restore := askGlobalTrackingConfirm
		askGlobalTrackingConfirm = func(context.Context) (bool, error) { return false, huh.ErrUserAborted }
		t.Cleanup(func() { askGlobalTrackingConfirm = restore })

		var buf bytes.Buffer
		maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
		if buf.Len() != 0 {
			t.Fatalf("a cancelled prompt must stay silent, got: %q", buf.String())
		}
		if _, err := os.Stat(filepath.Join(cfg, settings.UserSettingsFileName)); !os.IsNotExist(err) {
			t.Fatalf("a cancelled prompt must not write the user settings file (err=%v)", err)
		}
	})
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
	// An unrelated feature may already have created a settings file without
	// recording repo-local activation. Disable is still the first activation
	// choice and therefore still owes the routed-data takeover.
	testutil.WriteFile(t, dir, ".entire/settings.local.json", `{"investigate":{"max_turns":4}}`)
	paths.ClearInvisibleRuntimeCache()

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
	if _, err := strategy.InstallGitHook(t.Context(), true, false); err != nil {
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
	// Hardening: if a combination were ever NOT rejected, it must run against
	// a throwaway directory, not the real repo this test file lives in.
	t.Chdir(t.TempDir())
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
	// Hardening: if a combination were ever NOT rejected, it must run against
	// a throwaway directory, not the real repo this test file lives in.
	t.Chdir(t.TempDir())
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

	migrateLinkFile = func(string, string) error { return errors.New("simulated EXDEV") }
	t.Cleanup(func() { migrateLinkFile = os.Link })

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

func TestMaybeMigrateGlobalRuntimeData_DoesNotOverwriteConcurrentDestination(t *testing.T) {
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
	source := filepath.Join(dir, filepath.FromSlash(sourceRel), "logs", "entire.log")
	destination := filepath.Join(dir, ".entire", "logs", "entire.log")
	testutil.WriteFile(t, dir, sourceRel+"/logs/entire.log", "older routed data")

	restore := migrateLinkFile
	migrateLinkFile = func(oldpath, newpath string) error {
		if err := os.WriteFile(newpath, []byte("new live data"), 0o600); err != nil {
			return err
		}
		return restore(oldpath, newpath)
	}
	t.Cleanup(func() { migrateLinkFile = restore })

	var buf bytes.Buffer
	maybeMigrateGlobalRuntimeData(t.Context(), &buf)

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new live data" {
		t.Fatalf("concurrent destination was overwritten: %q", got)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must remain for a later retry: %v", err)
	}
	if !strings.Contains(buf.String(), "1 could not be moved") {
		t.Fatalf("race must be reported as a retained migration failure, got %q", buf.String())
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
