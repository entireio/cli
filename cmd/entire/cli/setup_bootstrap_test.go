package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testUser     = "octocat"
	cmdGit       = "git"
	gitCmdCommit = "commit"
	gitCmdConfig = "config"
)

// runBootstrapWith runs the full bootstrap (init + finalize) in one
// call, used by tests that don't need to assert phasing. The real caller
// runs the two phases around agent setup.
func runBootstrapWith(ctx context.Context, w io.Writer, opts BootstrapOptions, runner bootstrapRunner) error {
	state, err := runBootstrapInitWith(ctx, w, opts, runner)
	if err != nil {
		return err
	}
	return runBootstrapFinalize(ctx, w, state)
}

// fakeRunner is a test seam for bootstrapRunner. Each (name, args[0]) pair
// maps to a response.
type fakeRunner struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	calls     []fakeCall
}

type fakeResponse struct {
	stdout string
	err    error
}

type fakeCall struct {
	dir  string
	name string
	args []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		responses: make(map[string]fakeResponse),
	}
}

func (f *fakeRunner) key(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (f *fakeRunner) set(name string, args []string, stdout string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[f.key(name, args)] = fakeResponse{stdout: stdout, err: err}
}

func (f *fakeRunner) lookup(name string, args []string) (fakeResponse, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.responses[f.key(name, args)]
	return r, ok
}

func (f *fakeRunner) record(dir, name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{dir: dir, name: name, args: args})
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.record("", name, args)
	if r, ok := f.lookup(name, args); ok {
		return r.stdout, r.err
	}
	return "", fmt.Errorf("fakeRunner: unexpected call %s %v", name, args)
}

func (f *fakeRunner) RunInDir(_ context.Context, dir, name string, args ...string) (string, error) {
	f.record(dir, name, args)
	if r, ok := f.lookup(name, args); ok {
		return r.stdout, r.err
	}
	return "", fmt.Errorf("fakeRunner: unexpected call in %s: %s %v", dir, name, args)
}

// setIdentityConfigured simulates `git config --get user.name/email` returning
// non-empty values, so ensureGitIdentity treats identity as already set.
func (f *fakeRunner) setIdentityConfigured() {
	f.set("git", []string{"config", "--get", "user.name"}, "Test User\n", nil)
	f.set("git", []string{"config", "--get", "user.email"}, "test@example.com\n", nil)
}

// hasCall returns whether any recorded call matches the predicate.
func (f *fakeRunner) hasCall(match func(fakeCall) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if match(c) {
			return true
		}
	}
	return false
}

func TestGhHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFakeRunner()

	r.set("gh", []string{"--version"}, "gh version 2.81.0\n", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)

	if !ghAvailable(ctx, r) {
		t.Fatal("ghAvailable should be true")
	}
	if !ghAuthenticated(ctx, r) {
		t.Fatal("ghAuthenticated should be true")
	}
	user, err := ghCurrentUser(ctx, r)
	if err != nil || user != testUser {
		t.Fatalf("ghCurrentUser = %q, %v; want octocat", user, err)
	}
}

func TestGhAvailable_Missing(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("gh", []string{"--version"}, "", errors.New("not found"))
	if ghAvailable(context.Background(), r) {
		t.Fatal("expected ghAvailable to return false when gh is missing")
	}
}

func TestDoInitialCommit_EmptyFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newFakeRunner()
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, "", nil)

	committed, err := doInitialCommit(context.Background(), r, dir, "msg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if committed {
		t.Fatal("expected committed=false for empty folder")
	}
}

func TestDoInitialCommit_WithFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newFakeRunner()
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, " M README.md\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "msg"}, "", nil)

	committed, err := doInitialCommit(context.Background(), r, dir, "msg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("expected committed=true")
	}
	// Verify gpgsign=false was passed to the commit.
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 3 && c.args[0] == "-c" && c.args[1] == "commit.gpgsign=false" && c.args[2] == gitCmdCommit
	}) {
		t.Fatal("expected commit to pass -c commit.gpgsign=false")
	}
}

func TestRunBootstrap_DeclinedInNonInteractive(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	err := runBootstrapWith(context.Background(), io.Discard, BootstrapOptions{}, newFakeRunner())
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("expected errBootstrapDeclined, got %v", err)
	}
}

func TestRunBootstrap_LocalFlow(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, " M file\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "First!"}, "", nil)

	opts := BootstrapOptions{
		InitRepo:             true,
		InitialCommitMessage: "First!",
	}
	err := runBootstrapWith(context.Background(), io.Discard, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify git init ran in the cwd.
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) == 1 && c.args[0] == "init"
	}) {
		t.Fatal("expected git init call")
	}
	// Bootstrap is local-only: it must never shell out to gh.
	if r.hasCall(func(c fakeCall) bool { return c.name == "gh" }) {
		t.Fatal("bootstrap must not invoke gh")
	}
}

func TestResolveCommitMessage_SkipFlag(t *testing.T) {
	t.Parallel()
	msg, commit, err := resolveCommitMessage(BootstrapOptions{SkipInitialCommit: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commit {
		t.Fatal("commit should be false when SkipInitialCommit is set")
	}
	if msg != "" {
		t.Fatalf("message should be empty when skipping, got %q", msg)
	}
}

func TestResolveCommitMessage_FlagTakesMessage(t *testing.T) {
	t.Parallel()
	msg, commit, err := resolveCommitMessage(BootstrapOptions{InitialCommitMessage: "custom"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !commit {
		t.Fatal("commit should be true with explicit message flag")
	}
	if msg != "custom" {
		t.Fatalf("message = %q, want custom", msg)
	}
}

func TestResolveCommitMessage_NonInteractiveDefault(t *testing.T) {
	msg, commit, err := resolveCommitMessage(BootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !commit {
		t.Fatal("commit should default to true non-interactively")
	}
	if msg != defaultInitialCommitMessage {
		t.Fatalf("message = %q, want Initial commit", msg)
	}
}

// TestRunBootstrap_CreatesNoRemote is the guard for the invariant that
// `entire enable` bootstrapping is local-only. Creating a repository on a
// forge and publishing a directory's contents are the user's calls to make
// with their own tooling, so bootstrap must never reach the network: no gh
// invocation, and no `git remote`/`git push`. Any future flag that adds one
// back has to break this test first.
//
// It sweeps every input shape rather than just --yes, because a remote would
// most plausibly return attached to one option rather than to all of them.
func TestRunBootstrap_CreatesNoRemote(t *testing.T) {
	cases := map[string]BootstrapOptions{
		"yes":            {Yes: true},
		"init-repo":      {InitRepo: true},
		"custom message": {InitRepo: true, InitialCommitMessage: "custom"},
		"skipped commit": {InitRepo: true, SkipInitialCommit: true},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			// No t.Parallel: restoreCwd chdirs, which is process-global.
			restoreCwd(t, t.TempDir())

			r := newFakeRunner()
			r.setIdentityConfigured()
			r.set("git", []string{"init"}, "", nil)
			r.set("git", []string{"add", "-A"}, "", nil)
			r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, " M f\n", nil)
			r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", defaultInitialCommitMessage}, "", nil)
			r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "custom"}, "", nil)

			if err := runBootstrapWith(context.Background(), io.Discard, opts, r); err != nil {
				t.Fatalf("bootstrap failed: %v", err)
			}

			if r.hasCall(func(c fakeCall) bool { return c.name == "gh" }) {
				t.Error("bootstrap must not invoke gh")
			}
			for _, sub := range []string{"remote", "push"} {
				if r.hasCall(gitArgsMatch([]string{sub})) {
					t.Errorf("bootstrap must not run git %s", sub)
				}
			}
		})
	}
}

// TestRunBootstrap_InitBeforeFinalize verifies the two-phase split: init
// runs git init up front, finalize creates the commit. A simulated "agent
// setup" step writes a file between the phases; that file must end up in the
// initial commit (i.e. `git add -A` happens after setup, not before).
func TestRunBootstrap_InitBeforeFinalize(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, " A .entire/settings.json\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "First"}, "", nil)

	opts := BootstrapOptions{
		InitRepo:             true,
		InitialCommitMessage: "First",
	}

	// Phase 1: init. This must NOT stage or commit.
	state, err := runBootstrapInitWith(context.Background(), io.Discard, opts, r)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state after init")
	}
	if !r.hasCall(gitArgsMatch([]string{"init"})) {
		t.Fatal("expected git init during phase 1")
	}
	forbidden := [][]string{
		{"add", "-A"},
		{"--no-optional-locks", "status", "--porcelain"},
		{"-c", "commit.gpgsign=false", gitCmdCommit, "-m", "First"},
	}
	for _, args := range forbidden {
		if r.hasCall(gitArgsMatch(args)) {
			t.Fatalf("git %v was called during init; should have been deferred to finalize", args)
		}
	}

	// Phase 2: finalize. Now the commit lands.
	if err := runBootstrapFinalize(context.Background(), io.Discard, state); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if !r.hasCall(gitArgsMatch([]string{"-c", "commit.gpgsign=false", gitCmdCommit, "-m", "First"})) {
		t.Fatal("expected commit during finalize")
	}
}

// gitArgsMatch returns a predicate for hasCall that matches a `git` call
// whose args start with the given slice. Bootstrap shells out to nothing
// else, so the command name is not a parameter.
func gitArgsMatch(args []string) func(fakeCall) bool {
	return func(c fakeCall) bool {
		if c.name != cmdGit || len(c.args) < len(args) {
			return false
		}
		for i, a := range args {
			if c.args[i] != a {
				return false
			}
		}
		return true
	}
}

// TestBootstrap_FreshMachine_RealGit is an integration-style test that runs
// real git via execRunner on a temp dir isolated from the user's global git
// config. Regression guard for the issue where bootstrap commits failed
// without a configured identity or because of commit.gpgsign=true.
func TestBootstrap_FreshMachine_RealGit(t *testing.T) {
	// Isolate from any global git config: point HOME + GIT_CONFIG_* at
	// empty/missing locations, and force a broken GPG signing config that
	// would fail any commit if we did not pass -c commit.gpgsign=false.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	// A global config that demands signing with a non-existent program. If
	// our bootstrap did not override gpgsign for its commit, git would
	// error out here.
	globalCfg := filepath.Join(emptyHome, ".gitconfig")
	globalContent := "[user]\n\tname = Fresh User\n\temail = fresh@example.com\n[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /does/not/exist\n"
	if err := writeTempFile(globalCfg, globalContent); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	// Ensure no system config interferes.
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	// Create a file to commit.
	if err := writeTempFile(filepath.Join(projectDir, "README.md"), "hello\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	opts := BootstrapOptions{
		InitRepo:             true,
		InitialCommitMessage: "Initial",
	}
	err := runBootstrapWith(context.Background(), io.Discard, opts, execRunner{})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Verify a commit actually landed on HEAD.
	out, err := execRunner{}.RunInDir(context.Background(), projectDir, "git", "log", "--oneline")
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if !strings.Contains(out, "Initial") {
		t.Fatalf("expected 'Initial' commit in log, got: %q", out)
	}
}

func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// Bootstrap no longer resolves the git identity: that moved to the enable
// command, which runs the preflight after agent selection and before the
// initial commit (needsIdentity covers the bootstrap-with-commit case). This
// asserts the deferral — init succeeds with no identity configured and leaves
// the commit decision for later — replacing the test that asserted bootstrap
// itself failed here.
func TestBootstrapInit_DefersGitIdentity(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	if err := writeTempFile(filepath.Join(projectDir, "README.md"), "hi\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	state, err := runBootstrapInitWith(context.Background(), io.Discard, BootstrapOptions{
		InitRepo:             true,
		InitialCommitMessage: "x",
	}, execRunner{})
	if err != nil {
		t.Fatalf("bootstrap init: %v", err)
	}
	if state == nil || !state.commit {
		t.Fatalf("bootstrap state = %+v, want deferred initial commit", state)
	}
}

// TestErrSentinels_DistinctPrePostInit documents the contract that the two
// error sentinels signal: errBootstrapDeclined before `git init`,
// errBootstrapInterrupted after. setup.go relies on this to show the
// right user-facing message.
func TestErrSentinels_DistinctPrePostInit(t *testing.T) {
	t.Parallel()
	if errors.Is(errBootstrapDeclined, errBootstrapInterrupted) {
		t.Fatal("errBootstrapDeclined and errBootstrapInterrupted must not match as the same sentinel")
	}
}

func TestEnableCmd_InitCommitMessageFlagsMutuallyExclusive(t *testing.T) {
	setupTestRepo(t)

	cmd := newEnableCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--initial-commit-message", "foo", "--skip-initial-commit"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when both --initial-commit-message and --skip-initial-commit are set")
	}
	if !strings.Contains(err.Error(), "initial-commit-message") || !strings.Contains(err.Error(), "skip-initial-commit") {
		t.Fatalf("expected error to mention both flags, got: %v", err)
	}
}

func TestEnableCmd_InitRepoFlagsMutuallyExclusive(t *testing.T) {
	setupTestRepo(t)

	cmd := newEnableCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--init-repo", "--no-init-repo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when both --init-repo and --no-init-repo are set")
	}
	if !strings.Contains(err.Error(), "init-repo") || !strings.Contains(err.Error(), "no-init-repo") {
		t.Fatalf("expected error to mention both flags, got: %v", err)
	}
}

// withInteractivePromptStdin forces interactive, accessible (text-based)
// prompt mode and feeds input to os.Stdin for the duration of the test, so a
// huh prompt reads a scripted answer instead of opening /dev/tty or blocking
// on a real terminal. ENTIRE_TEST_TTY makes CanPromptInteractively report
// true; ACCESSIBLE makes the form read os.Stdin rather than dial the terminal.
func withInteractivePromptStdin(t *testing.T, input string) {
	t.Helper()
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pr.Close() })
	go func() {
		pw.WriteString(input) //nolint:errcheck // test helper
		pw.Close()
	}()
	old := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = old })
}

// TestConfirmInitRepo_DefaultsToNo verifies that pressing Enter (empty
// input) at the init-repo prompt declines. `entire enable` is often run
// reflexively, so a stray run in a non-repo directory must not initialize
// a repo on the user's behalf. Regression guard for issue #1717.
func TestConfirmInitRepo_DefaultsToNo(t *testing.T) {
	withInteractivePromptStdin(t, "\n")

	proceed, err := confirmInitRepo(t.TempDir(), BootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proceed {
		t.Fatal("confirmInitRepo should default to No (decline) on empty input")
	}
}

// TestConfirmInitRepo_ExplicitYesProceeds verifies an explicit "y" still
// opts in, so the safer default doesn't block intentional use.
func TestConfirmInitRepo_ExplicitYesProceeds(t *testing.T) {
	withInteractivePromptStdin(t, "y\n")

	proceed, err := confirmInitRepo(t.TempDir(), BootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceed {
		t.Fatal("confirmInitRepo should proceed when the user explicitly answers yes")
	}
}

func TestPromptBootstrapSetupChoice_DefaultsToLocalInitialCommit(t *testing.T) {
	withInteractivePromptStdin(t, "\n")

	var out bytes.Buffer
	choice, err := promptBootstrapSetupChoice(&out, "/tmp/example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != bootstrapSetupLocal {
		t.Fatalf("choice = %q, want %q", choice, bootstrapSetupLocal)
	}
	if !strings.Contains(out.String(), "Set one up?") {
		t.Fatalf("expected merged init+setup prompt, got: %s", out.String())
	}
	// The wrong-directory guard: the prompt must show where the repo would
	// be created (issue #1717's concern, carried over from the confirm).
	if !strings.Contains(out.String(), "/tmp/example") {
		t.Fatalf("expected prompt to show the target directory, got: %s", out.String())
	}
}

func TestPromptBootstrapSetupChoice_OffersCustomizeSecond(t *testing.T) {
	withInteractivePromptStdin(t, "2\n")

	choice, err := promptBootstrapSetupChoice(io.Discard, "/tmp/example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != bootstrapSetupCustom {
		t.Fatalf("choice = %q, want %q", choice, bootstrapSetupCustom)
	}
}

func TestPromptBootstrapSetupChoice_OffersDecline(t *testing.T) {
	// Options are local(1) / customize(2) / No(3).
	withInteractivePromptStdin(t, "3\n")

	choice, err := promptBootstrapSetupChoice(io.Discard, "/tmp/example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != bootstrapSetupDecline {
		t.Fatalf("choice = %q, want %q", choice, bootstrapSetupDecline)
	}
}

func TestRunBootstrapInit_InteractiveLocalPresetUsesOneSetupAnswer(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)
	withInteractivePromptStdin(t, "\n")

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)

	var out bytes.Buffer
	state, err := runBootstrapInitWith(context.Background(), &out, BootstrapOptions{}, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.commit || state.message != defaultInitialCommitMessage {
		t.Fatalf("local preset commit = %v, message = %q", state.commit, state.message)
	}
	if !strings.Contains(out.String(), "Set one up?") {
		t.Fatalf("expected merged init+setup prompt, got: %s", out.String())
	}
}

// TestRunBootstrapInit_InteractiveDeclineRunsNoGit verifies that
// declining the merged prompt leaves the folder untouched: the select runs
// before `git init`, so "No" must not create a repository.
func TestRunBootstrapInit_InteractiveDeclineRunsNoGit(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)
	// The option list is local(1) / customize(2) / No(3).
	withInteractivePromptStdin(t, "3\n")

	r := newFakeRunner()
	_, err := runBootstrapInitWith(context.Background(), io.Discard, BootstrapOptions{}, r)
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("err = %v, want errBootstrapDeclined", err)
	}
	if r.hasCall(gitArgsMatch([]string{"init"})) {
		t.Fatal("declining the merged prompt must not run git init")
	}
}

// restoreCwd chdirs into dir for the duration of the test.
func restoreCwd(t *testing.T, dir string) {
	t.Helper()
	// macOS resolves /tmp → /private/tmp; canonicalize for safety.
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canon = dir
	}
	t.Chdir(canon)
}

func TestRunBootstrap_YesAcceptsAllDefaults(t *testing.T) {
	// --yes should init the repo and commit with the default message,
	// without any interactive prompts. That it creates no remote is
	// TestRunBootstrap_CreatesNoRemote's job, along with every other input.
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"--no-optional-locks", "status", "--porcelain"}, " M f\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", defaultInitialCommitMessage}, "", nil)

	var stdout bytes.Buffer
	err := runBootstrapWith(context.Background(), &stdout, BootstrapOptions{Yes: true}, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.hasCall(gitArgsMatch([]string{"init"})) {
		t.Error("expected git init")
	}
	if !r.hasCall(gitArgsMatch([]string{"-c", "commit.gpgsign=false", gitCmdCommit, "-m", defaultInitialCommitMessage})) {
		t.Error("expected commit with default 'Initial commit' message")
	}
}
