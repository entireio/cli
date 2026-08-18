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

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/pflag"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

// isolateUserHome points os.UserHomeDir at a temp dir so the user-level agent
// hook install/removal in the global enable/disable flow never touches the
// developer's real ~/.claude or ~/.gemini settings.
func isolateUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	return home
}

func TestRunEnableGlobalMode_OutsideGitRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
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
	out := buf.String()
	if !strings.Contains(out, "Global tracking enabled.") {
		t.Fatalf("missing confirmation, got: %q", out)
	}
	// enable --global installs user-level agent hooks and reports per agent.
	if !strings.Contains(out, "User-level agent hooks:") {
		t.Fatalf("missing user-level hook report, got: %q", out)
	}
	for _, want := range []string{"claude-code: installed", "gemini: installed", "user-level hooks not supported:"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q, got: %q", want, out)
		}
	}
	for _, f := range []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".gemini", "settings.json"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("user-level hook file not written: %v", err)
		}
	}
}

func TestRunEnableGlobalMode_ReportsAlreadyInstalled(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	isolateUserHome(t)
	t.Cleanup(settings.ClearGlobalModeCache)

	if err := runEnableGlobalMode(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runEnableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude-code: already installed", "gemini: already installed"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("second enable missing %q, got: %q", want, buf.String())
		}
	}
}

func TestRunEnableGlobalMode_PreservesExcludeLists(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
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
	isolateUserHome(t)
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

// TestRunDisableGlobalMode_RemovesUserHooksNonInteractive: without a TTY the
// disable flow removes Entire's user-level hooks without prompting — and only
// Entire's entries, leaving the rest of the user file intact.
func TestRunDisableGlobalMode_RemovesUserHooksNonInteractive(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := isolateUserHome(t)
	t.Cleanup(settings.ClearGlobalModeCache)

	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runEnableGlobalMode(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runDisableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "user-level hooks removed") {
		t.Fatalf("missing removal report, got: %q", buf.String())
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "entire hooks claude-code") {
		t.Errorf("Entire hooks left in user settings: %s", data)
	}
	if !strings.Contains(string(data), "opus") {
		t.Errorf("unrelated user settings key removed: %s", data)
	}
	gemini, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gemini), "entire hooks gemini") {
		t.Errorf("Entire hooks left in gemini user settings: %s", gemini)
	}
}

// TestRunEnableGlobalMode_PerAgentFailureIsolation: a corrupt
// ~/.claude/settings.json must fail ONLY claude-code's install (a `!` line),
// leave the corrupt file untouched, and let gemini's install land — partial
// success stays exit 0.
func TestRunEnableGlobalMode_PerAgentFailureIsolation(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := isolateUserHome(t)
	t.Cleanup(settings.ClearGlobalModeCache)

	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o750); err != nil {
		t.Fatal(err)
	}
	const corrupt = `{not json`
	if err := os.WriteFile(claudePath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runEnableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatalf("partial success must stay exit 0, got: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "! claude-code: install failed") {
		t.Errorf("missing claude-code failure line, got: %q", out)
	}
	if !strings.Contains(out, "✓ gemini: installed") {
		t.Errorf("gemini install must proceed past claude-code's failure, got: %q", out)
	}
	data, err := os.ReadFile(claudePath)
	if err != nil || string(data) != corrupt {
		t.Errorf("corrupt claude settings must be left untouched (data=%q err=%v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); err != nil {
		t.Errorf("gemini user-level hook file not written: %v", err)
	}
}

// TestRunEnableGlobalMode_ZeroCoverageIsAnError: when NO supporting agent
// ends up with hooks installed, "Global tracking enabled" plus exit 0 would
// report a tracking state that does not exist — the command must say so and
// return an error. (The setting itself stays persisted; a re-run repairs.)
func TestRunEnableGlobalMode_ZeroCoverageIsAnError(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := isolateUserHome(t)
	t.Cleanup(settings.ClearGlobalModeCache)

	for _, dir := range []string{".claude", ".gemini"} {
		path := filepath.Join(home, dir, "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	err := runEnableGlobalMode(t.Context(), &buf)
	if err == nil {
		t.Fatalf("zero agents covered must return an error, output: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "will not capture any sessions") {
		t.Errorf("missing zero-coverage explanation, got: %q", buf.String())
	}
	us, loadErr := settings.LoadUserSettings(t.Context())
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if us.Global == nil || !us.Global.Enabled {
		t.Errorf("the enabled setting itself must persist so a re-run repairs: %+v", us.Global)
	}
}

// TestRunDisableGlobalMode_UnreadableAgentConfigReported: an agent whose
// user-level config cannot be read must get a `! ... could not remove` line
// instead of being silently skipped, while readable agents still get their
// hooks removed.
func TestRunDisableGlobalMode_UnreadableAgentConfigReported(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	home := isolateUserHome(t)
	t.Cleanup(settings.ClearGlobalModeCache)

	if err := runEnableGlobalMode(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(claudePath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runDisableGlobalMode(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "! claude-code: could not remove:") {
		t.Errorf("unreadable agent config must be reported, got: %q", out)
	}
	if !strings.Contains(out, "✓ gemini: user-level hooks removed") {
		t.Errorf("gemini removal must proceed, got: %q", out)
	}
	gemini, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gemini), "entire hooks gemini") {
		t.Errorf("Entire hooks left in gemini user settings: %s", gemini)
	}
}

// TestMaybeAskGlobalTracking_YesInstallsUserHooks pins the wizard's yes path
// end to end: the stubbed confirm answers yes and the user-level hooks must
// actually land in the isolated home — deleting the install call (not just
// the report line) fails this test.
func TestMaybeAskGlobalTracking_YesInstallsUserHooks(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Cleanup(settings.ClearGlobalModeCache)
	restore := askGlobalTrackingConfirm
	askGlobalTrackingConfirm = func(context.Context) (bool, error) { return true, nil }
	t.Cleanup(func() { askGlobalTrackingConfirm = restore })

	var buf bytes.Buffer
	maybeAskGlobalTracking(t.Context(), &buf, EnableOptions{})
	if !strings.Contains(buf.String(), "Global tracking enabled") {
		t.Fatalf("missing enable confirmation, got: %q", buf.String())
	}
	us, err := settings.LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if us.Global == nil || !us.Global.Enabled {
		t.Fatalf("wizard yes must persist enabled, got %+v", us.Global)
	}
	claude, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(claude), "entire hooks claude-code stop") {
		t.Errorf("claude user-level hooks not installed by the wizard yes (err=%v): %s", err, claude)
	}
	gemini, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if err != nil || !strings.Contains(string(gemini), "entire hooks gemini session-start") {
		t.Errorf("gemini user-level hooks not installed by the wizard yes (err=%v): %s", err, gemini)
	}
	// The wizard's confirmation IS the generation's announcement, exactly like
	// enable --global: without the ack, PersistentPostRun's detection warn
	// stacks on top of it in the same `entire enable` run.
	if _, err := os.Stat(filepath.Join(cfg, globalWarnMarkerName)); err != nil {
		t.Fatalf("wizard enable must ack the global warn marker itself: %v", err)
	}
	var warn bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &warn)
	if warn.Len() != 0 {
		t.Errorf("detection warn must not stack on the wizard's own confirmation, got: %q", warn.String())
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

// TestEnableCmd_GlobalConflictListCoversEveryFlag derives the --global
// exclusivity contract from the live flag set instead of trusting the
// hand-maintained list in markEnableGlobalFlagConflicts: every flag `entire
// enable` registers must either be classified below as genuinely
// global-compatible or be rejected by cobra when combined with --global. A
// future repo-scoped flag added to newEnableCmd without a matching conflict
// entry fails here until classified — the silent escape would reintroduce
// the ignored-flag bug the pairwise registration exists to prevent.
func TestEnableCmd_GlobalConflictListCoversEveryFlag(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	// Flags that may legitimately combine with --global. Every entry needs a
	// one-line reason; anything not listed must be mutually exclusive.
	allowlist := map[string]string{
		"global": "the machine-wide mode selector itself",
		"help":   "cobra built-in; prints usage and exits before any repo-level action",
	}

	enum := newEnableCmd()
	enum.InitDefaultHelpFlag() // Execute() registers --help; include it in the enumeration
	var names []string
	enum.Flags().VisitAll(func(f *pflag.Flag) {
		if _, allowed := allowlist[f.Name]; !allowed {
			names = append(names, f.Name)
		}
	})
	if len(names) == 0 {
		t.Fatal("enumeration found no non-allowlisted enable flags; the harness is broken")
	}

	for _, name := range names {
		cmd := newEnableCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		arg := "--" + name
		if typ := cmd.Flags().Lookup(name).Value.Type(); typ != "bool" {
			// Non-bool flags need a value that parses for their type, so the
			// rejection asserted below is cobra's mutual-exclusion check and
			// not a value parse error.
			val := "x"
			switch {
			case strings.HasPrefix(typ, "int") || strings.HasPrefix(typ, "uint") || strings.HasPrefix(typ, "float"):
				val = "1"
			case typ == "duration":
				val = "1s"
			}
			arg += "=" + val
		}
		cmd.SetArgs([]string{"--global", arg})
		err := cmd.Execute()
		if err == nil {
			t.Errorf("--%s combined with --global was accepted; add it to markEnableGlobalFlagConflicts or classify it in this test's allowlist", name)
			continue
		}
		// Pin the rejection to cobra's mutual-exclusion validation, not some
		// unrelated parse or runtime failure.
		if !strings.Contains(err.Error(), "none of the others can be") {
			t.Errorf("--%s with --global was rejected for a reason other than mutual exclusion: %v", name, err)
		}
	}

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
