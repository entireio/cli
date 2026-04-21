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

func TestDoInitialCommit_EmptyFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newFakeRunner()
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, "", nil)

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
	r.set("git", []string{"status", "--porcelain"}, " M README.md\n", nil)
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

func TestRunGitHubBootstrap_DeclinedInNonInteractive(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{}, newFakeRunner())
	if !errors.Is(err, errBootstrapDeclined) {
		t.Fatalf("expected errBootstrapDeclined, got %v", err)
	}
}

func TestRunGitHubBootstrap_NoGitHubFlow(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, " M file\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "First!"}, "", nil)

	opts := GitHubBootstrapOptions{
		InitRepo:             true,
		NoGitHub:             true,
		InitialCommitMessage: "First!",
	}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify git init ran in the cwd.
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) == 1 && c.args[0] == "init"
	}) {
		t.Fatal("expected git init call")
	}
	// Verify no gh calls were made.
	if r.hasCall(func(c fakeCall) bool { return c.name == "gh" }) {
		t.Fatal("did not expect gh calls with --no-github")
	}
}

func TestRunGitHubBootstrap_GhMissingFallsBackToLocal(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, "", nil)

	opts := GitHubBootstrapOptions{InitRepo: true}
	var errBuf bytes.Buffer
	err := runGitHubBootstrapWith(context.Background(), io.Discard, &errBuf, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "gh CLI not found") {
		t.Fatalf("expected hint about installing gh, got %q", errBuf.String())
	}
}

func TestRunGitHubBootstrap_FullNonInteractive(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh 2.81.0", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	// Name availability check: repo does not exist yet.
	r.set("gh", []string{"repo", "view", "octocat/my-new", "--json", "name"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, " M f\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "Seed"}, "", nil)
	r.set("gh", []string{
		"repo", "create", "octocat/my-new",
		"--private",
		"--source=.",
		"--remote=origin",
	}, "", nil)
	r.set("git", []string{"push", "-q", "--no-verify", "-u", "origin", "HEAD"}, "", nil)

	opts := GitHubBootstrapOptions{
		InitRepo:             true,
		RepoName:             "my-new",
		RepoVisibility:       "private",
		InitialCommitMessage: "Seed",
	}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.hasCall(func(c fakeCall) bool {
		return c.name == "gh" && len(c.args) > 3 && c.args[0] == ghSubcmdRepo && c.args[1] == ghActCreate
	}) {
		t.Fatal("expected gh repo create call")
	}
	// The initial push must bypass hooks (--no-verify) and be quiet (-q).
	if !r.hasCall(argsMatch("git", []string{"push", "-q", "--no-verify", "-u", "origin", "HEAD"})) {
		t.Fatal("expected git push -q --no-verify after repo create")
	}
}

func TestRunGitHubBootstrap_RepoExistsFails(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	// The name is already taken. Since we aren't returning an *exec.ExitError,
	// ghRepoExists returns (false, err) and ghRepoExists wraps. To avoid
	// plumbing ExitError into the test, use the "already exists" path directly
	// by returning success — meaning the repo was found.
	r.set("gh", []string{"repo", "view", "octocat/taken", "--json", "name"}, "{\"name\":\"taken\"}", nil)
	r.set("git", []string{"init"}, "", nil)

	opts := GitHubBootstrapOptions{
		InitRepo: true,
		RepoName: "taken",
	}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err == nil {
		t.Fatal("expected error when repo already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got %v", err)
	}
}

func TestResolveCommitMessage_SkipFlag(t *testing.T) {
	t.Parallel()
	msg, commit, err := resolveCommitMessage(GitHubBootstrapOptions{SkipInitialCommit: true})
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
	msg, commit, err := resolveCommitMessage(GitHubBootstrapOptions{InitialCommitMessage: "custom"})
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
	t.Setenv("ENTIRE_TEST_TTY", "0")
	msg, commit, err := resolveCommitMessage(GitHubBootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !commit {
		t.Fatal("commit should default to true non-interactively")
	}
	if msg != "Initial commit" {
		t.Fatalf("message = %q, want Initial commit", msg)
	}
}

// TestRunGitHubBootstrap_SkipCommitKeepsGitHub verifies that passing
// --skip-initial-commit still creates the GitHub repo (if requested) but
// skips both commit and push. The local repo's files remain unstaged.
func TestRunGitHubBootstrap_SkipCommitKeepsGitHub(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("gh", []string{"repo", "view", "octocat/skipme", "--json", "name"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("gh", []string{
		"repo", "create", "octocat/skipme",
		"--private",
		"--source=.",
		"--remote=origin",
	}, "", nil)

	opts := GitHubBootstrapOptions{
		InitRepo:          true,
		RepoName:          "skipme",
		RepoVisibility:    "private",
		SkipInitialCommit: true,
	}
	var out bytes.Buffer
	if err := runGitHubBootstrapWith(context.Background(), &out, io.Discard, opts, r); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	if r.hasCall(argsMatch("git", []string{"add", "-A"})) {
		t.Fatal("git add should not run when SkipInitialCommit is set")
	}
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 1 && (c.args[0] == gitCmdCommit || (len(c.args) >= 3 && c.args[2] == gitCmdCommit))
	}) {
		t.Fatal("git commit should not run when SkipInitialCommit is set")
	}
	if r.hasCall(argsMatch("git", []string{"push"})) {
		t.Fatal("git push should not run when commit was skipped")
	}
	// gh repo create should still have run.
	if !r.hasCall(argsMatch("gh", []string{"repo", "create"})) {
		t.Fatal("gh repo create should still run when only the commit is skipped")
	}
	// Output should mention how to commit manually.
	if !strings.Contains(out.String(), "git add -A") {
		t.Fatalf("expected manual-commit hint in output, got: %s", out.String())
	}
}

func TestGhFlagsProvided(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts GitHubBootstrapOptions
		want bool
	}{
		{"none", GitHubBootstrapOptions{}, false},
		{"repo-name", GitHubBootstrapOptions{RepoName: "foo"}, true},
		{"repo-owner", GitHubBootstrapOptions{RepoOwner: "octocat"}, true},
		{"repo-visibility", GitHubBootstrapOptions{RepoVisibility: "private"}, true},
		// NoGitHub intentionally does NOT count as "provided" — it's the
		// opposite signal. It's handled separately upstream.
		{"no-github", GitHubBootstrapOptions{NoGitHub: true}, false},
		{"init-repo only", GitHubBootstrapOptions{InitRepo: true}, false},
	}
	for _, tc := range cases {
		if got := ghFlagsProvided(tc.opts); got != tc.want {
			t.Errorf("%s: ghFlagsProvided = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRunGitHubBootstrap_NonInteractive_NoFlagsDefaultsToGitHub confirms the
// non-interactive happy path still creates a GitHub repo when the user
// didn't set any explicit flag (the confirm prompt is only interactive).
func TestRunGitHubBootstrap_NonInteractive_NoFlagsDefaultsToGitHub(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	// Default folder slug derived from t.TempDir().
	suggested := slugifyRepoName(filepath.Base(dir))
	r.set("gh", []string{"repo", "view", "octocat/" + suggested, "--json", "name"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)

	state, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, GitHubBootstrapOptions{InitRepo: true}, r)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if !state.useGitHub {
		t.Fatal("non-interactive bootstrap should default to using GitHub")
	}
}

// TestRunGitHubBootstrap_InitBeforeFinalize verifies the two-phase split:
// init runs git init up front, finalize creates the commit + pushes. A
// simulated "agent setup" step writes a file between the phases; that
// file must end up in the initial commit (i.e. `git add -A` happens
// after setup, not before).
func TestRunGitHubBootstrap_InitBeforeFinalize(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("gh", []string{"repo", "view", "octocat/phased", "--json", "name"}, "", errors.New("not found"))
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, " A .entire/settings.json\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "First"}, "", nil)
	r.set("gh", []string{
		"repo", "create", "octocat/phased",
		"--private",
		"--source=.",
		"--remote=origin",
	}, "", nil)
	r.set("git", []string{"push", "-q", "--no-verify", "-u", "origin", "HEAD"}, "", nil)

	opts := GitHubBootstrapOptions{
		InitRepo:             true,
		RepoName:             "phased",
		RepoVisibility:       "private",
		InitialCommitMessage: "First",
	}

	// Phase 1: init. This must NOT call git add/commit/ gh repo create.
	state, err := runGitHubBootstrapInitWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state after init")
	}
	forbidden := [][]string{
		{"add", "-A"},
		{"status", "--porcelain"},
		{"-c", "commit.gpgsign=false", "commit", "-m", "First"},
	}
	for _, args := range forbidden {
		if r.hasCall(argsMatch("git", args)) {
			t.Fatalf("git %v was called during init; should have been deferred to finalize", args)
		}
	}
	if r.hasCall(func(c fakeCall) bool {
		return c.name == "gh" && len(c.args) >= 2 && c.args[0] == ghSubcmdRepo && c.args[1] == ghActCreate
	}) {
		t.Fatal("gh repo create was called during init; should have been deferred to finalize")
	}

	// Phase 2: finalize. Now commit + push.
	if err := runGitHubBootstrapFinalize(context.Background(), io.Discard, state); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if !r.hasCall(argsMatch("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "First"})) {
		t.Fatal("expected commit during finalize")
	}
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == "gh" && len(c.args) >= 2 && c.args[0] == ghSubcmdRepo && c.args[1] == ghActCreate
	}) {
		t.Fatal("expected gh repo create during finalize")
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

func TestEnsureGitIdentity_AlreadyConfigured(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.setIdentityConfigured()

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No git config writes should have occurred.
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 2 && c.args[0] == gitCmdConfig && (c.args[1] == "user.name" || c.args[1] == "user.email")
	}) {
		t.Fatal("did not expect identity writes when already configured")
	}
}

func TestEnsureGitIdentity_SourcedFromGh(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	// Identity missing locally (empty stdout).
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	// gh available and authenticated.
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"octo","name":"Octo Cat","email":"octo@example.com"}`, nil)
	// Expect writes with values from gh.
	r.set("git", []string{"config", "user.name", "Octo Cat"}, "", nil)
	r.set("git", []string{"config", "user.email", "octo@example.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
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
	// email is null/missing: should fall back to id+login noreply.
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"octo","name":"","email":null}`, nil)
	r.set("git", []string{"config", "user.name", "octo"}, "", nil)
	r.set("git", []string{"config", "user.email", "42+octo@users.noreply.github.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestEnsureGitIdentity_PreservesExistingName covers the partial-config
// case: `user.name` is set globally but `user.email` is missing. We must
// source only the email (from gh) and leave the name untouched — we
// never want to silently replace the user's configured name with a
// gh-derived login.
func TestEnsureGitIdentity_PreservesExistingName(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	// Name is set globally, email is not.
	r.set("git", []string{"config", "--get", "user.name"}, "John Doe\n", nil)
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	// gh available and returns both values.
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"johndoe","name":"Johnny Dough","email":"john@example.com"}`, nil)
	// Only the email should be written locally — the name must stay
	// at the user's global value.
	r.set("git", []string{"config", "user.email", "john@example.com"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No `git config user.name ...` call should have been made.
	if r.hasCall(func(c fakeCall) bool {
		return c.name == cmdGit && len(c.args) >= 2 && c.args[0] == gitCmdConfig && c.args[1] == "user.name"
	}) {
		t.Fatal("ensureGitIdentity should not write user.name when it's already set globally")
	}
}

// TestEnsureGitIdentity_PreservesExistingEmail mirrors the above for the
// other direction: email set, name missing.
func TestEnsureGitIdentity_PreservesExistingEmail(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "john@example.com\n", nil)
	r.set("gh", []string{"--version"}, "gh", nil)
	r.set("gh", []string{"auth", "status"}, "ok", nil)
	r.set("gh", []string{"api", "user"}, `{"id":42,"login":"johndoe","name":"Johnny","email":"other@example.com"}`, nil)
	r.set("git", []string{"config", "user.name", "Johnny"}, "", nil)

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
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
	t.Setenv("ENTIRE_TEST_TTY", "0")
	r := newFakeRunner()
	r.set("git", []string{"config", "--get", "user.name"}, "", errors.New("not set"))
	r.set("git", []string{"config", "--get", "user.email"}, "", errors.New("not set"))
	r.set("gh", []string{"--version"}, "", errors.New("not found"))

	err := ensureGitIdentity(context.Background(), io.Discard, io.Discard, r, t.TempDir())
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

// TestBootstrap_FreshMachine_RealGit is an integration-style test that runs
// real git via execRunner on a temp dir isolated from the user's global git
// config. Regression guard for the issue where bootstrap commits failed
// without a configured identity or because of commit.gpgsign=true.
func TestBootstrap_FreshMachine_RealGit(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")

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

	opts := GitHubBootstrapOptions{
		InitRepo:             true,
		NoGitHub:             true,
		InitialCommitMessage: "Initial",
	}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, execRunner{})
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

// ghFailingRunner wraps another bootstrapRunner and forces all `gh`
// invocations to fail, while letting real `git` calls through. This
// lets tests deterministically exercise the "gh unavailable" path
// regardless of whether `gh` is installed/authenticated on the host.
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

// TestBootstrap_FreshMachine_NoIdentity_RealGit verifies that a fresh
// machine without any git identity configured fails cleanly in
// non-interactive mode with a helpful error message, instead of letting
// `git commit` fail with a confusing "please tell me who you are" stderr.
//
// Uses a gh-failing runner wrapper rather than PATH manipulation so the
// test isn't sensitive to whether `gh` + GH_TOKEN/GITHUB_TOKEN are set
// on the host.
func TestBootstrap_FreshMachine_NoIdentity_RealGit(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")

	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	// Empty global config: no user.name/user.email.
	globalCfg := filepath.Join(emptyHome, ".gitconfig")
	if err := writeTempFile(globalCfg, ""); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	// Belt-and-suspenders: unset any GitHub tokens so a wrapper bypass
	// would still not find credentials.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	projectDir := t.TempDir()
	restoreCwd(t, projectDir)
	if err := writeTempFile(filepath.Join(projectDir, "README.md"), "hi\n"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	opts := GitHubBootstrapOptions{
		InitRepo:             true,
		NoGitHub:             true,
		InitialCommitMessage: "x",
	}
	runner := ghFailingRunner{inner: execRunner{}}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, runner)
	if err == nil {
		t.Fatal("expected error when identity missing and gh unavailable")
	}
	if !strings.Contains(err.Error(), "git config --global user.name") {
		t.Fatalf("expected guidance to set git config, got: %v", err)
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

func TestRunGitHubBootstrap_YesAcceptsAllDefaults(t *testing.T) {
	// --yes should init repo, create GitHub repo under user's account (private),
	// and use default commit message — without any interactive prompts.
	t.Setenv("ENTIRE_TEST_TTY", "0") // non-interactive
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh 2.81.0", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "myuser\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "myorg\n", nil)
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, " M f\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "Initial commit"}, "", nil)

	// Expect repo created under the user's account (not org), private
	repoName := filepath.Base(dir)
	fullName := "myuser/" + repoName
	r.set("gh", []string{
		"repo", "create", fullName,
		"--private",
		"--source=.",
		"--remote=origin",
	}, "", nil)
	r.set("git", []string{"push", "-q", "--no-verify", "-u", "origin", "HEAD"}, "", nil)

	opts := GitHubBootstrapOptions{Yes: true}
	var stdout bytes.Buffer
	err := runGitHubBootstrapWith(context.Background(), &stdout, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have used user's account, not org
	output := stdout.String()
	if !strings.Contains(output, "Using GitHub owner: myuser") {
		t.Errorf("expected owner to be user's account, got: %s", output)
	}
	// Should have committed with default message
	if !r.hasCall(argsMatch("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "Initial commit"})) {
		t.Error("expected commit with default 'Initial commit' message")
	}
	// Should have created the repo
	if !r.hasCall(func(c fakeCall) bool {
		return c.name == "gh" && len(c.args) > 3 && c.args[0] == ghSubcmdRepo && c.args[1] == ghActCreate
	}) {
		t.Error("expected gh repo create call")
	}
}

func TestRunGitHubBootstrap_YesRepoExistsNoTTY_Fails(t *testing.T) {
	// When --yes is set, the repo name is taken, and there's no TTY,
	// we should get a clear error instead of a silent gh failure.
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("gh", []string{"--version"}, "gh 2.81.0", nil)
	r.set("gh", []string{"auth", "status"}, "Logged in", nil)
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "myuser\n", nil)
	r.set("gh", []string{"api", "user/orgs", "--jq", ".[].login"}, "", nil)
	r.set("git", []string{"init"}, "", nil)

	// The suggested repo name already exists.
	repoName := filepath.Base(dir)
	r.set("gh", []string{"repo", "view", "myuser/" + repoName, "--json", "name"}, `{"name":"`+repoName+`"}`, nil)

	opts := GitHubBootstrapOptions{Yes: true}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err == nil {
		t.Fatal("expected error when repo name exists and no TTY")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}
}

func TestResolveRepoName_YesRepoExistsWithTTY_FallsBackToPrompt(t *testing.T) {
	// When --yes is set, the name is taken, and a TTY is available,
	// resolveRepoName should print a conflict message and fall through
	// to the interactive prompt (which we can't complete in a test, but
	// we can verify it reached the right path via the output).
	t.Setenv("ENTIRE_TEST_TTY", "1")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	repoName := filepath.Base(dir)
	// The suggested name exists.
	r.set("gh", []string{"repo", "view", "myuser/" + repoName, "--json", "name"}, `{"name":"`+repoName+`"}`, nil)

	var stdout bytes.Buffer
	opts := GitHubBootstrapOptions{Yes: true}
	// resolveRepoName will print the conflict message, then try to run
	// the interactive form which will fail without a real TTY — that's fine,
	// we just need to verify it printed the conflict message (reached the
	// fallback path) rather than returning the taken name or a hard error.
	_, err := resolveRepoName(context.Background(), &stdout, io.Discard, r, "myuser", dir, opts)

	output := stdout.String()
	if !strings.Contains(output, "already exists on GitHub") {
		t.Errorf("expected conflict message in output, got: %s", output)
	}
	// The form.Run() will error since there's no real TTY — that's expected.
	// The key assertion is that we got the conflict message, proving the
	// fallback path was taken instead of returning the taken name.
	if err == nil {
		t.Error("expected error from form.Run() without a real TTY")
	}
}

func TestRunGitHubBootstrap_YesWithNoGitHub(t *testing.T) {
	// --yes combined with --no-github should skip GitHub but still init + commit.
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := t.TempDir()
	restoreCwd(t, dir)

	r := newFakeRunner()
	r.setIdentityConfigured()
	r.set("git", []string{"init"}, "", nil)
	r.set("git", []string{"add", "-A"}, "", nil)
	r.set("git", []string{"status", "--porcelain"}, " M f\n", nil)
	r.set("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "Initial commit"}, "", nil)

	opts := GitHubBootstrapOptions{Yes: true, NoGitHub: true}
	err := runGitHubBootstrapWith(context.Background(), io.Discard, io.Discard, opts, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT have called gh at all
	if r.hasCall(func(c fakeCall) bool { return c.name == "gh" }) {
		t.Error("expected no gh calls with --no-github")
	}
	// Should have committed
	if !r.hasCall(argsMatch("git", []string{"-c", "commit.gpgsign=false", "commit", "-m", "Initial commit"})) {
		t.Error("expected commit with default message")
	}
}
