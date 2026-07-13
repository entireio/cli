package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	ghSubcmdRepo = "repo"
	ghActCreate  = "create"
	gitCmdCommit = "commit"
	gitCmdConfig = "config"
)

func TestSlugifyRepoName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"my-project":          "my-project",
		"My Cool Project":     "My-Cool-Project",
		"weird@@@name!!":      "weird-name",
		"":                    "my-repo",
		"---":                 "my-repo",
		"foo__bar":            "foo__bar",
		"a.b.c":               "a.b.c",
		"leading space":       "leading-space",
		"double  space  here": "double-space-here",
	}
	for in, want := range cases {
		if got := slugifyRepoName(in); got != want {
			t.Errorf("slugifyRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateRepoName(t *testing.T) {
	t.Parallel()
	// GitHub accepts leading ".", "-", and "_" (e.g. `.github`), so we
	// accept them too.
	valid := []string{
		"my-repo", "foo_bar", "a.b.c", "Repo123", "x",
		".github", ".leading", "-leading", "_leading",
	}
	for _, name := range valid {
		if err := validateRepoName(name); err != nil {
			t.Errorf("validateRepoName(%q) unexpectedly returned error: %v", name, err)
		}
	}
	// "." and ".." are specifically rejected; anything with / or
	// whitespace is rejected; length is capped.
	invalid := []string{"", ".", "..", "has/slash", "has space", strings.Repeat("a", 101)}
	for _, name := range invalid {
		if err := validateRepoName(name); err == nil {
			t.Errorf("validateRepoName(%q) = nil, want error", name)
		}
	}
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
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "gamma\nalpha\n\nbeta\n", nil)

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
	orgs, err := ghListOrgs(ctx, r)
	if err != nil {
		t.Fatalf("ghListOrgs error: %v", err)
	}
	// Must be sorted, trimmed, and blank-skipped.
	want := []string{"alpha", "beta", "gamma"}
	if len(orgs) != len(want) {
		t.Fatalf("orgs = %v, want %v", orgs, want)
	}
	for i, o := range orgs {
		if o != want[i] {
			t.Fatalf("orgs[%d] = %q, want %q", i, o, want[i])
		}
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

func TestResolveOwner_FlagAcceptsUnknown(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	owner, err := resolveOwner(&buf, testUser, []string{"acme"}, GitHubBootstrapOptions{RepoOwner: "external-org"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "external-org" {
		t.Fatalf("owner = %q, want external-org", owner)
	}
}

func TestResolveOwner_SingleDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	owner, err := resolveOwner(&buf, testUser, nil, GitHubBootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != testUser {
		t.Fatalf("owner = %q, want octocat", owner)
	}
	if !strings.Contains(buf.String(), testUser) {
		t.Fatalf("expected owner announcement, got %q", buf.String())
	}
}

func TestResolveVisibility_FlagInternalRequiresOrg(t *testing.T) {
	t.Parallel()
	_, err := resolveVisibility(testUser, testUser, GitHubBootstrapOptions{RepoVisibility: "internal"})
	if err == nil {
		t.Fatal("expected error for internal visibility on user repo")
	}
}

func TestResolveVisibility_FlagValid(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"public", "private", "internal"} {
		owner := testUser
		current := testUser
		if v == "internal" {
			owner = "acme"
		}
		got, err := resolveVisibility(owner, current, GitHubBootstrapOptions{RepoVisibility: v})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", v, err)
		}
		if got != v {
			t.Fatalf("%s: got %q", v, got)
		}
	}
}

func TestResolveVisibility_FlagInvalid(t *testing.T) {
	t.Parallel()
	_, err := resolveVisibility(testUser, testUser, GitHubBootstrapOptions{RepoVisibility: "weird"})
	if err == nil {
		t.Fatal("expected error for invalid visibility")
	}
}

func TestResolveRepoName_FlagValidates(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	// Return a non-ExitError; ghRepoExists then bubbles up, and resolveRepoName
	// logs a warning but proceeds with the flag-supplied name.
	r.set("gh", []string{"repo", "view", "octocat/ok-name", "--json", "name"}, "", errors.New("transient"))
	name, err := resolveRepoName(context.Background(), io.Discard, io.Discard, r, testUser, t.TempDir(), GitHubBootstrapOptions{RepoName: "ok-name"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "ok-name" {
		t.Fatalf("name = %q", name)
	}
}

func TestResolveRepoName_FlagRejectsInvalid(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	_, err := resolveRepoName(context.Background(), io.Discard, io.Discard, r, testUser, t.TempDir(), GitHubBootstrapOptions{RepoName: "has/slash"})
	if err == nil {
		t.Fatal("expected error for name containing '/'")
	}
}

func TestGhRepoExists_RealErrorPath(t *testing.T) {
	t.Parallel()
	// If `gh repo view` succeeds (no error), the repo exists.
	r := newFakeRunner()
	r.set("gh", []string{"repo", "view", "octocat/real", "--json", "name"}, "{\"name\":\"real\"}", nil)
	exists, err := ghRepoExists(context.Background(), r, testUser, "real")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
}

// commitArgs is the argument vector for the initial commit our bootstrap makes:
// GPG signing disabled and --allow-empty so a fresh dir (or the safe default
// that stages nothing) still yields a commit.
func commitArgs(message string) []string {
	return []string{"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", message}
}

func TestDoInitialCommit_EmptyDefaultDoesNotStage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newFakeRunner()
	r.set("git", commitArgs("msg"), "", nil)

	// commitExisting=false: no `git add`, just an --allow-empty commit.
	if err := doInitialCommit(context.Background(), r, dir, "msg", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("git add -A must not run when existing files are not being committed")
	}
	if !r.hasCall(argsMatch("git", commitArgs("msg"))) {
		t.Fatal("expected an --allow-empty initial commit")
	}
}

func TestDoInitialCommit_CommitExistingStages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newFakeRunner()
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", commitArgs("msg"), "", nil)

	if err := doInitialCommit(context.Background(), r, dir, "msg", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("expected git add -A when committing existing files")
	}
	// Verify gpgsign=false was passed to the commit.
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 3 && c.args[0] == "-c" && c.args[1] == "commit.gpgsign=false" && c.args[2] == gitCmdCommit
	}) {
		t.Fatal("expected commit to pass -c commit.gpgsign=false")
	}
}

// argsMatch returns a predicate for hasCall that matches when c.name == name
// and c.args starts with the given args slice.
func argsMatch(name string, args []string) func(fakeCall) bool {
	return func(c fakeCall) bool {
		if c.name != name || len(c.args) < len(args) {
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

// gitInitCalled reports whether `git init` was executed on the runner.
func gitInitCalled(r *fakeRunner) bool {
	return r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) == 1 && c.args[0] == "init"
	})
}

// ghCalled reports whether any `gh` subprocess was invoked.
func ghCalled(r *fakeRunner) bool {
	return r.hasCall(func(c fakeCall) bool { return c.name == "gh" })
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

// --- normalizeBootstrapOptions (flag model + back-compat mapping) ---------

func TestNormalize_DefaultsToPrompt(t *testing.T) {
	t.Parallel()
	opts := GitHubBootstrapOptions{}
	if err := normalizeBootstrapOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Bootstrap != bootstrapModePrompt {
		t.Fatalf("Bootstrap = %q, want prompt", opts.Bootstrap)
	}
}

func TestNormalize_InvalidModeRejected(t *testing.T) {
	t.Parallel()
	opts := GitHubBootstrapOptions{Bootstrap: "publish"}
	if err := normalizeBootstrapOptions(&opts); err == nil {
		t.Fatal("expected error for invalid --bootstrap value")
	}
}

func TestNormalize_DeprecatedFlagMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   GitHubBootstrapOptions
		want string
	}{
		{"init-repo -> local", GitHubBootstrapOptions{InitRepo: true}, bootstrapModeLocal},
		{"no-init-repo -> off", GitHubBootstrapOptions{NoInitRepo: true}, bootstrapModeOff},
		{"no-github stays prompt", GitHubBootstrapOptions{NoGitHub: true}, bootstrapModePrompt},
	}
	for _, tc := range cases {
		opts := tc.in
		if err := normalizeBootstrapOptions(&opts); err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if opts.Bootstrap != tc.want {
			t.Errorf("%s: Bootstrap = %q, want %q", tc.name, opts.Bootstrap, tc.want)
		}
	}
}

func TestNormalize_DeprecatedAndNewCombosRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   GitHubBootstrapOptions
	}{
		{"bootstrap + init-repo", GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, InitRepo: true}},
		{"bootstrap + no-github", GitHubBootstrapOptions{Bootstrap: bootstrapModeLocal, NoGitHub: true}},
		{"repo-name with local", GitHubBootstrapOptions{Bootstrap: bootstrapModeLocal, RepoName: "x"}},
		{"repo-name with off", GitHubBootstrapOptions{Bootstrap: bootstrapModeOff, RepoName: "x"}},
		{"repo-name with no-github", GitHubBootstrapOptions{NoGitHub: true, RepoName: "x"}},
		{"initial-message without commit-existing", GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, InitialCommitMessage: "hi"}},
		{"skip + commit-existing", GitHubBootstrapOptions{SkipInitialCommit: true, CommitExistingFiles: true}},
	}
	for _, tc := range cases {
		opts := tc.in
		if err := normalizeBootstrapOptions(&opts); err == nil {
			t.Errorf("%s: expected a hard error, got nil", tc.name)
		}
	}
}

func TestNormalize_ValidCombosAccepted(t *testing.T) {
	t.Parallel()
	cases := []GitHubBootstrapOptions{
		{Bootstrap: bootstrapModeGitHub, RepoName: "x", RepoVisibility: "public"},
		{Bootstrap: bootstrapModeGitHub, CommitExistingFiles: true, InitialCommitMessage: "hi"},
		{Bootstrap: bootstrapModeLocal, CommitExistingFiles: true},
		{RepoName: "x"}, // gh flag with default (prompt) mode is allowed
		{SkipInitialCommit: true},
	}
	for i, in := range cases {
		opts := in
		if err := normalizeBootstrapOptions(&opts); err != nil {
			t.Errorf("case %d %+v: unexpected error: %v", i, in, err)
		}
	}
}

// --- Decline / no-side-effect paths (issue #1717 core guarantees) ---------

// TestBootstrap_BareNonInteractive_NoSideEffects is the core #1717 guarantee:
// bare `enable` in a non-repo directory, non-interactively, must refuse WITHOUT
// running git init / probing gh / creating anything.
//
// SECURITY-CRITICAL (mutation target: resolveBootstrapDecision's non-interactive
// default must return proceed=false, not true).
func TestBootstrap_BareNonInteractive_NoSideEffects(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner() // no stubs: any git/gh call is an unexpected-call error
	_, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{}, r)
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("expected errBootstrapDeclined, got %v", err)
	}
	if gitInitCalled(r) {
		t.Fatal("git init must NOT run when bootstrap is declined")
	}
	if ghCalled(r) {
		t.Fatal("no gh calls expected on a declined bootstrap")
	}
}

// TestBootstrap_Off_DeclinesWithoutGh verifies --bootstrap=off (and its
// deprecated alias --no-init-repo) exits with no init and no gh probe.
func TestBootstrap_Off_DeclinesWithoutGh(t *testing.T) {
	for _, opts := range []GitHubBootstrapOptions{
		{Bootstrap: bootstrapModeOff},
		{NoInitRepo: true}, // deprecated alias
	} {
		t.Setenv("ENTIRE_TEST_TTY", "1") // even with a TTY, off never prompts or probes
		dir := t.TempDir()
		restoreCwd(t, dir)
		r := newFakeRunner()
		_, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, opts, r)
		if !errors.Is(err, errBootstrapDeclined) {
			t.Fatalf("%+v: expected errBootstrapDeclined, got %v", opts, err)
		}
		if gitInitCalled(r) {
			t.Fatalf("%+v: git init must not run for bootstrap=off", opts)
		}
		if ghCalled(r) {
			t.Fatalf("%+v: no gh probe should run for bootstrap=off", opts)
		}
	}
}

// TestBootstrap_InteractiveDeclineInit_NoGhProbe is keeper #2 (no network before
// consent): a user who declines the init prompt must make ZERO gh/network calls.
//
// SECURITY-CRITICAL (mutation target: the gh probe must sit AFTER git init in
// runGitHubBootstrapInitWith, and the init confirm must default to NO).
func TestBootstrap_InteractiveDeclineInit_NoGhProbe(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1") // force interactive (canPrompt == true)
	t.Setenv("ACCESSIBLE", "1")      // text prompt reads os.Stdin

	// Immediate-EOF stdin so the init confirm falls back to its default (NO).
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pw.Close()
	t.Cleanup(func() { pr.Close() })
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	restoreCwd(t, dir)

	// Default prompt mode: GitHub creation is on the table (no --no-github), so a
	// pre-fix ordering would probe gh before the init prompt. No stubs — any gh
	// call is an unexpected-call error.
	r := newFakeRunner()
	_, err = runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{}, r)
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("expected errBootstrapDeclined (default NO on EOF), got %v", err)
	}
	if ghCalled(r) {
		t.Fatal("no gh availability/auth probe may run before the user consents to git init")
	}
	if gitInitCalled(r) {
		t.Fatal("git init must not run when the init confirm defaults to NO")
	}
}

// TestBootstrap_InteractiveDefaultsToNo is keeper #1 (default-NO on every
// confirmation): a bare Enter / EOF on the init prompt must not initialize.
//
// SECURITY-CRITICAL (mutation target: confirmInitRepo's `confirmed := false`).
func TestBootstrap_InteractiveDefaultsToNo(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pw.Close()
	t.Cleanup(func() { pr.Close() })
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restoreCwd(t, dir)

	r := newFakeRunner()
	// --no-github so the interactive init confirm is the only gate under test.
	_, err = runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{NoGitHub: true}, r)
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("expected errBootstrapDeclined (default NO on EOF), got %v", err)
	}
	if gitInitCalled(r) {
		t.Fatal("git init must NOT run when the init confirmation defaults to NO")
	}
}

// --- local mode --------------------------------------------------------------

// TestBootstrap_Local_EmptyCommit_NoGitHub covers the safe default in local
// mode (and its --yes equivalent): git init, an EMPTY initial commit, existing
// files NOT staged, no GitHub, and ZERO gh calls.
//
// SECURITY-CRITICAL (mutation target: doInitialCommit must not `git add` when
// commitFiles is false; local mode must not touch gh).
func TestBootstrap_Local_EmptyCommit_NoGitHub(t *testing.T) {
	for _, opts := range []GitHubBootstrapOptions{
		{Bootstrap: bootstrapModeLocal},
		{Yes: true}, // prompt + --yes resolves to a local init
	} {
		dir := t.TempDir()
		restoreCwd(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("shh"), 0o644); err != nil {
			t.Fatal(err)
		}

		r := newFakeRunner()
		r.setIdentityConfigured()
		r.set("git", []string{"init"}, "", nil)
		r.set("git", commitArgs("Initial commit"), "", nil)

		if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
			t.Fatalf("%+v: unexpected error: %v", opts, err)
		}
		if !gitInitCalled(r) {
			t.Fatalf("%+v: expected git init", opts)
		}
		if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
			t.Fatalf("%+v: existing files must NOT be staged by default", opts)
		}
		if !r.hasCall(argsMatch("git", commitArgs("Initial commit"))) {
			t.Fatalf("%+v: expected an empty initial commit", opts)
		}
		if ghCalled(r) {
			t.Fatalf("%+v: local mode must make ZERO gh calls", opts)
		}
	}
}

// TestBootstrap_Local_CommitExisting stages + commits the existing files
// locally, but still never touches GitHub.
func TestBootstrap_Local_CommitExisting(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", commitArgs("Initial commit"), "", nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeLocal, CommitExistingFiles: true}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("expected git add -A with --commit-existing-files")
	}
	if ghCalled(r) {
		t.Fatal("local mode must never call gh")
	}
}

// TestBootstrap_DeprecatedInitRepo_LocalOnly verifies the back-compat alias:
// --init-repo maps to local (no GitHub), even in an interactive context and
// with no explicit --no-github.
func TestBootstrap_DeprecatedInitRepo_LocalOnly(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1") // even interactive, --init-repo => local, no GitHub confirm
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)

	state, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{InitRepo: true}, r)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if state.useGitHub {
		t.Fatal("--init-repo must map to a local-only init, never GitHub")
	}
	if ghCalled(r) {
		t.Fatal("--init-repo (local) must not consult gh")
	}
}

// --- github mode -------------------------------------------------------------

// stubGitHubCreate wires a fakeRunner for a full github-mode create + push of
// repo octocat/<name> at the given visibility.
func stubGitHubCreate(r *fakeRunner, name, visibility string) {
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh 2.81.0", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("gh", []string{"repo", "view", "octocat/" + name, "--json", "name"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("gh", []string{"repo", "create", "octocat/" + name, "--" + visibility, "--source=.", "--remote=origin"}, "", nil)
	r.set("git", []string{"push", "-q", "--no-verify", "-u", "origin", "HEAD"}, "", nil)
}

// TestBootstrap_GitHub_EmptyCommit_ExistingFilesNotPushed is the safe-default
// github path: --bootstrap=github --yes creates the repo and pushes an EMPTY
// initial commit — the user's existing files are NOT staged or pushed.
//
// SECURITY-CRITICAL (mutation target: commitFiles gate — no `git add` without
// --commit-existing-files).
func TestBootstrap_GitHub_EmptyCommit_ExistingFilesNotPushed(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "secret.env"), []byte("TOKEN=abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := newFakeRunner()
	stubGitHubCreate(r, "myrepo", "private")
	r.set("git", commitArgs("Initial commit"), "", nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, Yes: true, RepoName: "myrepo"}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Repo created + pushed…
	if !r.hasCall(argsMatch("gh", []string{"repo", "create"})) {
		t.Fatal("expected gh repo create")
	}
	if !r.hasCall(argsMatch("git", []string{"push", "-q", "--no-verify"})) {
		t.Fatal("expected the empty initial commit to be pushed")
	}
	// …but the existing files were never staged.
	if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("existing files must NOT be staged/pushed without --commit-existing-files")
	}
}

// TestBootstrap_GitHub_CommitExisting_StagesAndPushes: with the explicit
// opt-in, existing files are staged, committed, and pushed.
func TestBootstrap_GitHub_CommitExisting_StagesAndPushes(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	stubGitHubCreate(r, "myrepo", "private")
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", commitArgs("Initial commit"), "", nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, Yes: true, RepoName: "myrepo", CommitExistingFiles: true}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("expected git add -A with --commit-existing-files")
	}
	if !r.hasCall(argsMatch("git", []string{"push", "-q", "--no-verify"})) {
		t.Fatal("expected push after commit")
	}
}

// TestBootstrap_GhFlagsWithYes_CreatesGitHub preserves released behavior for
// scripts: a --repo-* flag is explicit GitHub intent, so `--repo-name x --yes`
// (default prompt mode, non-interactive) still creates the repo — even though
// --yes on its own resolves to a local-only init.
func TestBootstrap_GhFlagsWithYes_CreatesGitHub(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	stubGitHubCreate(r, "wanted", "private")
	r.set("git", commitArgs("Initial commit"), "", nil)

	// No --bootstrap: prompt mode. --repo-name provides the GitHub intent.
	opts := GitHubBootstrapOptions{Yes: true, RepoName: "wanted"}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasCall(argsMatch("gh", []string{"repo", "create", "octocat/wanted"})) {
		t.Fatal("--repo-name --yes must still create the GitHub repo (back-compat)")
	}
	// Still safe-by-default on content: existing files not staged.
	if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("existing files must not be staged without --commit-existing-files")
	}
}

// TestBootstrap_BareYes_NeverGitHub is the counterpart: --yes ALONE (no --repo-*
// flags, default prompt mode) must resolve to a local init and make ZERO gh
// calls (keeper #4 scope-bounding).
func TestBootstrap_BareYes_NeverGitHub(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", commitArgs("Initial commit"), "", nil)

	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{Yes: true}, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ghCalled(r) {
		t.Fatal("--yes alone must never touch GitHub")
	}
}

// TestBootstrap_GitHub_CustomMessageAndVisibility exercises --initial-commit-message
// (allowed with --commit-existing-files) and a non-default visibility.
func TestBootstrap_GitHub_CustomMessageAndVisibility(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	stubGitHubCreate(r, "seedme", "public")
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", commitArgs("Seed"), "", nil)

	opts := GitHubBootstrapOptions{
		Bootstrap:            bootstrapModeGitHub,
		Yes:                  true,
		RepoName:             "seedme",
		RepoVisibility:       "public",
		CommitExistingFiles:  true,
		InitialCommitMessage: "Seed",
	}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasCall(argsMatch("gh", []string{"repo", "create", "octocat/seedme", "--public"})) {
		t.Fatal("expected repo created with --public")
	}
	if !r.hasCall(argsMatch("git", commitArgs("Seed"))) {
		t.Fatal("expected commit with custom message")
	}
}

// TestBootstrap_GitHub_GhMissingFallsBackToLocal: github intent but gh isn't
// installed — fall back to local-only and warn (still after init consent).
func TestBootstrap_GitHub_GhMissingFallsBackToLocal(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("git", commitArgs("Initial commit"), "", nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, RepoName: "wanted"}
	var errBuf bytes.Buffer
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, &errBuf, opts, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "gh CLI not found") {
		t.Fatalf("expected hint about installing gh, got %q", errBuf.String())
	}
	if r.hasCall(argsMatch("gh", []string{"repo", "create"})) {
		t.Fatal("must not create a repo when gh is unavailable")
	}
}

// TestBootstrap_GitHub_RepoExistsFails surfaces a taken repo name.
func TestBootstrap_GitHub_RepoExistsFails(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("gh", []string{"repo", "view", "octocat/taken", "--json", "name"}, "{\"name\":\"taken\"}", nil)
	r.set("git", []string{"init"}, "", nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, RepoName: "taken"}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got %v", err)
	}
}

// TestBootstrap_YesRepoExistsNoTTY_Fails: github mode + --yes + suggested name
// taken + no TTY => a clear error rather than a silent gh failure.
func TestBootstrap_YesRepoExistsNoTTY_Fails(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh 2.81.0", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "myuser\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("git", []string{"init"}, "", nil)
	repoName := filepath.Base(dir)
	r.set("gh", []string{"repo", "view", "myuser/" + repoName, "--json", "name"}, `{"name":"`+repoName+`"}`, nil)

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeGitHub, Yes: true}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got %v", err)
	}
}

func TestResolveRepoName_YesRepoExistsWithTTY_FallsBackToPrompt(t *testing.T) {
	// When --yes is set, the name is taken, and a TTY is available,
	// resolveRepoName should print a conflict message and fall through
	// to the interactive prompt.
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pr.Close() })
	go func() {
		pw.WriteString("unique-test-repo\n") //nolint:errcheck // test helper
		pw.Close()
	}()
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	repoName := filepath.Base(dir)
	r.set("gh", []string{"repo", "view", "myuser/" + repoName, "--json", "name"}, `{"name":"`+repoName+`"}`, nil)

	var stdout bytes.Buffer
	opts := GitHubBootstrapOptions{Yes: true}
	name, err := resolveRepoName(context.Background(), &stdout, io.Discard, r, "myuser", dir, opts)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "already exists on GitHub") {
		t.Errorf("expected conflict message in output, got: %s", stdout.String())
	}
	if name != "unique-test-repo" {
		t.Errorf("expected name %q, got %q", "unique-test-repo", name)
	}
}

// --- two-phase init/finalize ordering ---------------------------------------

// TestBootstrap_InitBeforeFinalize verifies the two-phase split: init runs
// `git init` up front; the commit + push happen only in finalize, so a file
// written by "agent setup" between the phases would be captured by a
// commit-existing-files run.
func TestBootstrap_InitBeforeFinalize(t *testing.T) {
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	stubGitHubCreate(r, "phased", "private")
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", commitArgs("First"), "", nil)

	opts := GitHubBootstrapOptions{
		Bootstrap:            bootstrapModeGitHub,
		Yes:                  true,
		RepoName:             "phased",
		CommitExistingFiles:  true,
		InitialCommitMessage: "First",
	}

	// Phase 1: init. Must NOT commit or create the repo yet.
	state, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state after init")
	}
	if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("git add must be deferred to finalize")
	}
	if r.hasCall(argsMatch("git", commitArgs("First"))) {
		t.Fatal("commit must be deferred to finalize")
	}
	if r.hasCall(argsMatch("gh", []string{"repo", "create"})) {
		t.Fatal("gh repo create must be deferred to finalize")
	}

	// Phase 2: finalize. Now commit + push.
	if err := runGitHubBootstrapFinalize(context.Background(), io.Discard, state); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if !r.hasCall(argsMatch("git", commitArgs("First"))) {
		t.Fatal("expected commit during finalize")
	}
	if !r.hasCall(argsMatch("gh", []string{"repo", "create"})) {
		t.Fatal("expected gh repo create during finalize")
	}
}

// --- plan text (keeper #3: truthful visibility) -----------------------------

// TestPrintBootstrapPlan_ShowsResolvedVisibility verifies the plan states the
// visibility the repo will actually be created with — never a hardcoded default.
func TestPrintBootstrapPlan_ShowsResolvedVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		opts        GitHubBootstrapOptions
		wantContain string
		wantAbsent  string
	}{
		{"default is private", GitHubBootstrapOptions{}, "PRIVATE repository", "PUBLIC"},
		{"flag public", GitHubBootstrapOptions{RepoVisibility: "public"}, "PUBLIC repository", "PRIVATE"},
		{"flag internal", GitHubBootstrapOptions{RepoVisibility: "internal"}, "INTERNAL repository", "PRIVATE"},
		{"flag mixed case", GitHubBootstrapOptions{RepoVisibility: "Public"}, "PUBLIC repository", "PRIVATE"},
		{"yes locks private", GitHubBootstrapOptions{Yes: true}, "PRIVATE repository", "PUBLIC"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		printBootstrapPlan(&buf, t.TempDir(), tc.opts, true)
		out := buf.String()
		if !strings.Contains(out, tc.wantContain) {
			t.Errorf("%s: plan missing %q\n%s", tc.name, tc.wantContain, out)
		}
		if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
			t.Errorf("%s: plan must not contain %q\n%s", tc.name, tc.wantAbsent, out)
		}
	}
}

// TestPrintBootstrapPlan_CommitExistingClause verifies the plan tells the truth
// about whether existing files will be committed.
func TestPrintBootstrapPlan_CommitExistingClause(t *testing.T) {
	t.Parallel()
	var empty bytes.Buffer
	printBootstrapPlan(&empty, t.TempDir(), GitHubBootstrapOptions{}, false)
	if !strings.Contains(empty.String(), "empty initial commit") {
		t.Errorf("default plan should mention an empty initial commit, got:\n%s", empty.String())
	}
	var full bytes.Buffer
	printBootstrapPlan(&full, t.TempDir(), GitHubBootstrapOptions{CommitExistingFiles: true}, false)
	if !strings.Contains(full.String(), "Stage and commit the files already") {
		t.Errorf("commit-existing plan should mention staging existing files, got:\n%s", full.String())
	}
}

// TestPlannedRepoVisibility_MatchesResolveVisibility guards that the plan's
// planned visibility agrees with what resolveVisibility will actually use for
// the deterministic (non-interactive) cases.
func TestPlannedRepoVisibility_MatchesResolveVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		opts       GitHubBootstrapOptions
		wantVis    string
		wantLocked bool
	}{
		{GitHubBootstrapOptions{RepoVisibility: "public"}, "public", true},
		{GitHubBootstrapOptions{RepoVisibility: "private"}, "private", true},
		{GitHubBootstrapOptions{Yes: true}, "private", true},
		{GitHubBootstrapOptions{}, "private", false},
	}
	for _, tc := range cases {
		gotVis, gotLocked := plannedRepoVisibility(tc.opts)
		if gotVis != tc.wantVis || gotLocked != tc.wantLocked {
			t.Errorf("plannedRepoVisibility(%+v) = (%q, %v), want (%q, %v)", tc.opts, gotVis, gotLocked, tc.wantVis, tc.wantLocked)
		}
		if tc.wantLocked && tc.opts.RepoVisibility != "" {
			resolved, err := resolveVisibility(testUser, testUser, tc.opts)
			if err != nil {
				t.Fatalf("resolveVisibility error: %v", err)
			}
			if resolved != gotVis {
				t.Errorf("plan says %q but resolveVisibility yields %q", gotVis, resolved)
			}
		}
	}
}

// --- git identity ------------------------------------------------------------

func TestEnsureGitIdentity_AlreadyConfigured(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.setIdentityConfigured()

	// allowGH=false: even with identity already set, no gh call should happen.
	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ghCalled(r) {
		t.Fatal("no gh calls when identity is already configured")
	}
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 2 && c.args[0] == gitCmdConfig && (c.args[1] == "user.name" || c.args[1] == "user.email")
	}) {
		t.Fatal("did not expect identity writes when already configured")
	}
}

// TestEnsureGitIdentity_LocalMode_NoGh verifies that a local-only init
// (allowGH=false) never probes gh even when identity is missing — it errors
// with guidance instead (non-interactive).
//
// SECURITY-CRITICAL for the "local => 0 gh calls" matrix guarantee.
func TestEnsureGitIdentity_LocalMode_NoGh(t *testing.T) {
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), false)
	if err == nil {
		t.Fatal("expected error when identity missing in local (no-gh) mode")
	}
	if ghCalled(r) {
		t.Fatal("local mode must not probe gh to source identity")
	}
}

func TestEnsureGitIdentity_SourcedFromGh(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"octo","name":"Octo Cat","email":"octo@example.com"}`, nil)
	r.set("git", []string{"config", "user.name", "Octo Cat"}, "", nil)
	r.set("git", []string{"config", "user.email", "octo@example.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureGitIdentity_GhNoreplyFallback(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"octo","name":"","email":null}`, nil)
	r.set("git", []string{"config", "user.name", "octo"}, "", nil)
	r.set("git", []string{"config", "user.email", "42+octo@users.noreply.github.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureGitIdentity_PreservesExistingName(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "John Doe\n", nil)
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"johndoe","name":"Johnny Dough","email":"john@example.com"}`, nil)
	r.set("git", []string{"config", "user.email", "john@example.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 2 && c.args[0] == gitCmdConfig && c.args[1] == "user.name"
	}) {
		t.Fatal("ensureGitIdentity should not write user.name when it's already set globally")
	}
}

func TestEnsureGitIdentity_PreservesExistingEmail(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "john@example.com\n", nil)
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"johndoe","name":"Johnny","email":"other@example.com"}`, nil)
	r.set("git", []string{"config", "user.name", "Johnny"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 2 && c.args[0] == gitCmdConfig && c.args[1] == "user.email"
	}) {
		t.Fatal("ensureGitIdentity should not write user.email when it's already set globally")
	}
}

func TestEnsureGitIdentity_NonInteractiveNoGh_Errors(t *testing.T) {
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	r.set("gh", []string{"--version"}, "", errors.New("not found"))

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir(), true)
	if err == nil {
		t.Fatal("expected error when identity missing and gh unavailable")
	}
	if !strings.Contains(err.Error(), "git config --global user.name") {
		t.Fatalf("expected guidance to set git config, got %v", err)
	}
}

func TestGhUserIdentity_NameFallsBackToLogin(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("gh", []string{"api", "user"}, `{"id":7,"login":"dev","name":"","email":"dev@example.com"}`, nil)
	name, email, err := ghUserIdentity(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "dev" {
		t.Fatalf("name = %q", name)
	}
	if email != "dev@example.com" {
		t.Fatalf("email = %q", email)
	}
}

// --- real-git integration ----------------------------------------------------

func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// ghFailingRunner wraps another bootstrapRunner and forces all `gh`
// invocations to fail, while letting real `git` calls through.
type ghFailingRunner struct {
	inner bootstrapRunner
}

func (r ghFailingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "gh" {
		return "", errors.New("gh not available (test)")
	}
	return r.inner.Run(ctx, name, args...)
}

func (r ghFailingRunner) RunInDir(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "gh" {
		return "", errors.New("gh not available (test)")
	}
	return r.inner.RunInDir(ctx, dir, name, args...)
}

// TestBootstrap_FreshMachine_RealGit runs real git via execRunner on a temp dir
// isolated from the user's global git config. Regression guard for commits
// failing without a configured identity or because of commit.gpgsign=true.
func TestBootstrap_FreshMachine_RealGit(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	globalCfg := filepath.Join(emptyHome, ".gitconfig")
	globalContent := "[user]\n\tname = Fresh User\n\temail = fresh@example.com\n[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /does/not/exist\n"
	if err := writeTempFile(globalCfg, globalContent); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	if err := writeTempFile(filepath.Join(projectDir, "README.md"), "hello\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Local mode, commit the existing files, so a real commit lands on HEAD.
	opts := GitHubBootstrapOptions{
		Bootstrap:            bootstrapModeLocal,
		CommitExistingFiles:  true,
		InitialCommitMessage: "Initial",
	}
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, execRunner{}); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	out, err := execRunner{}.RunInDir(context.Background(), projectDir, "git", "log", "--oneline")
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if !strings.Contains(out, "Initial") {
		t.Fatalf("expected 'Initial' commit in log, got: %q", out)
	}
	// The README the user placed must be tracked (we opted in).
	files, err := execRunner{}.RunInDir(context.Background(), projectDir, "git", "ls-files")
	if err != nil {
		t.Fatalf("git ls-files failed: %v", err)
	}
	if !strings.Contains(files, "README.md") {
		t.Fatalf("expected README.md tracked with --commit-existing-files, got: %q", files)
	}
}

// TestBootstrap_FreshMachine_RealGit_EmptyCommitLeavesFilesUntracked is the
// security-critical real-git counterpart: the safe default records an EMPTY
// commit and leaves the user's file untracked.
//
// SECURITY-CRITICAL (mutation target: doInitialCommit must not stage when
// commitFiles is false).
func TestBootstrap_FreshMachine_RealGit_EmptyCommitLeavesFilesUntracked(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	globalCfg := filepath.Join(emptyHome, ".gitconfig")
	if err := writeTempFile(globalCfg, "[user]\n\tname = Fresh User\n\temail = fresh@example.com\n"); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	if err := writeTempFile(filepath.Join(projectDir, "secret.env"), "TOKEN=leak\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	run := execRunner{}
	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeLocal} // no --commit-existing-files
	if err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, run); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	// HEAD exists (empty commit)…
	if _, err := run.RunInDir(context.Background(), projectDir, "git", "rev-parse", "HEAD"); err != nil {
		t.Fatalf("expected a HEAD commit, git rev-parse failed: %v", err)
	}
	// …but the user's file is NOT tracked.
	files, err := run.RunInDir(context.Background(), projectDir, "git", "ls-files")
	if err != nil {
		t.Fatalf("git ls-files failed: %v", err)
	}
	if strings.TrimSpace(files) != "" {
		t.Fatalf("expected NO tracked files with the safe default, got: %q", files)
	}
}

// TestBootstrap_FreshMachine_NoIdentity_RealGit verifies a fresh machine with
// no git identity fails cleanly in non-interactive mode with a helpful message.
func TestBootstrap_FreshMachine_NoIdentity_RealGit(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	globalCfg := filepath.Join(emptyHome, ".gitconfig")
	if err := writeTempFile(globalCfg, ""); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	if err := writeTempFile(filepath.Join(projectDir, "README.md"), "hi\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	opts := GitHubBootstrapOptions{Bootstrap: bootstrapModeLocal, CommitExistingFiles: true}
	runner := ghFailingRunner{inner: execRunner{}}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, runner)
	if err == nil {
		t.Fatal("expected error when identity missing and gh unavailable")
	}
	if !strings.Contains(err.Error(), "git config --global user.name") {
		t.Fatalf("expected guidance to set git config, got: %v", err)
	}
}

// --- sentinels + cobra flag wiring ------------------------------------------

func TestErrSentinels_DistinctPrePostInit(t *testing.T) {
	t.Parallel()
	// The three pre/post-init sentinels must be mutually distinct so setup.go
	// can print the right guidance for each: declined (bare Enter / EOF / no),
	// cancelled (Esc / Ctrl+C before init), interrupted (abort after init).
	sentinels := []error{errBootstrapDeclined, errBootstrapCancelled, errBootstrapInterrupted}
	for i := range sentinels {
		for j := range sentinels {
			if i != j && errors.Is(sentinels[i], sentinels[j]) {
				t.Fatalf("sentinels %d and %d must be distinct", i, j)
			}
		}
	}
}

// TestConfirmInitRepo_EOFDeclinesNotCancelled proves EOF (a bare Enter with no
// input) is handled as a plain decline — NOT the cancel sentinel — so the
// safety-critical "default NO on EOF" path keeps its "Not a git repository"
// guidance. Only a deliberate Esc/Ctrl+C yields errBootstrapCancelled.
func TestConfirmInitRepo_EOFDeclinesNotCancelled(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pw.Close()
	t.Cleanup(func() { pr.Close() })
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	_, err = runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{NoGitHub: true}, r)
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("EOF must map to errBootstrapDeclined, got %v", err)
	}
	if errors.Is(err, errBootstrapCancelled) {
		t.Fatal("EOF must NOT be treated as an explicit cancel")
	}
	if gitInitCalled(r) {
		t.Fatal("git init must not run on an EOF decline")
	}
}

// TestPrintBootstrapPlan_GitHubOptInWording verifies the plan does not state
// GitHub creation as guaranteed in the prompt path (default-NO opt-in), but
// does when an explicit --repo-* flag signals intent.
func TestPrintBootstrapPlan_GitHubOptInWording(t *testing.T) {
	t.Parallel()
	// No explicit intent: worded as optional / asked.
	var optional bytes.Buffer
	printBootstrapPlan(&optional, t.TempDir(), GitHubBootstrapOptions{}, true)
	if !strings.Contains(optional.String(), "Optionally create") || !strings.Contains(optional.String(), "defaults to no") {
		t.Errorf("prompt-path plan should mark GitHub creation optional, got:\n%s", optional.String())
	}
	// Explicit --repo-name intent: worded as definite.
	var definite bytes.Buffer
	printBootstrapPlan(&definite, t.TempDir(), GitHubBootstrapOptions{RepoName: "x"}, true)
	if strings.Contains(definite.String(), "Optionally create") {
		t.Errorf("explicit --repo-* intent should not be worded as optional, got:\n%s", definite.String())
	}
	if !strings.Contains(definite.String(), "Create a new PRIVATE repository") {
		t.Errorf("explicit intent should state the create step, got:\n%s", definite.String())
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

func TestEnableCmd_BootstrapFlagRegistered(t *testing.T) {
	cmd := newEnableCmd()
	for _, name := range []string{"bootstrap", "commit-existing-files", "repo-name", "repo-owner", "repo-visibility", "initial-commit-message"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected flag --%s to be registered", name)
		}
	}
	// Deprecated aliases remain registered (hidden) for back-compat.
	for _, name := range []string{"init-repo", "no-init-repo", "no-github", "skip-initial-commit"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("expected deprecated flag --%s to remain registered", name)
			continue
		}
		if f.Deprecated == "" {
			t.Errorf("expected --%s to be marked deprecated", name)
		}
	}
}
