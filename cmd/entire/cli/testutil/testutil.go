// Package testutil provides shared test utilities for both integration and e2e tests.
// This package has no build tags, making it usable by all test packages.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// PendingCheckpoint mirrors the `checkpoint list --pending --json` JSON output.
type PendingCheckpoint struct {
	ID               string    `json:"id"`
	Message          string    `json:"message"`
	MetadataDir      string    `json:"metadata_dir"`
	Date             time.Time `json:"date"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint"`
	ToolUseID        string    `json:"tool_use_id"`
	IsLogsOnly       bool      `json:"is_logs_only"`
	CondensationID   string    `json:"condensation_id"`
}

// InitRepo initializes a git repository in the given directory with test user config.
func InitRepo(t *testing.T, repoDir string) {
	t.Helper()

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	defer repo.Close()

	// Configure git user for commits
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"

	// Disable GPG signing for test commits
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	cfg.Core.AutoCRLF = "true"

	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("failed to set repo config: %v", err)
	}
}

// WriteFile creates a file with the given content in the repo directory.
// It creates parent directories as needed.
func WriteFile(t *testing.T, repoDir, path, content string) {
	t.Helper()

	fullPath := filepath.Join(repoDir, path)

	// Create parent directories
	dir := filepath.Dir(fullPath)
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create directory %s: %v", dir, err)
	}

	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}

// GitAdd stages files for commit.
func GitAdd(t *testing.T, repoDir string, paths ...string) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	for _, path := range paths {
		if _, err := worktree.Add(path); err != nil {
			t.Fatalf("failed to add file %s: %v", path, err)
		}
	}
}

// GitCommit creates a commit with all staged files.
func GitCommit(t *testing.T, repoDir, message string) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

// GitCheckoutNewBranch creates and checks out a new branch.
// Uses git CLI to work around go-git v5 bug with checkout deleting untracked files.
func GitCheckoutNewBranch(t *testing.T, repoDir, branchName string) {
	t.Helper()

	//nolint:noctx // test code, no context needed for git checkout
	cmd := exec.Command("git", "checkout", "-b", branchName)
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to checkout new branch %s: %v\nOutput: %s", branchName, err, output)
	}
}

// GetHeadHash returns the current HEAD commit hash.
func GetHeadHash(t *testing.T, repoDir string) string {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	return head.Hash().String()
}

// RunGit runs one git command in dir with an isolated git config, failing the
// test on error and returning combined output. Use it for operations go-git
// cannot express (force-add past .gitignore, rm --cached, worktree add) or
// where shelling out is simply clearer.
func RunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:noctx // test helper, no context needed
	cmd.Dir = dir
	cmd.Env = GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// GitAddForce stages paths past .gitignore. GitAdd goes through go-git's
// worktree.Add, which has no force option.
func GitAddForce(t *testing.T, repoDir string, paths ...string) {
	t.Helper()
	RunGit(t, repoDir, append([]string{"add", "-f"}, paths...)...)
}

// CreateBranch creates a local branch at the current HEAD.
func CreateBranch(t *testing.T, dir string, name string) {
	t.Helper()
	cmd := exec.Command("git", "branch", name) //nolint:noctx // test helper, no context needed
	cmd.Dir = dir
	cmd.Env = GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch %s: %v\n%s", name, err, out)
	}
}

// AddRemote adds a git remote named name pointing at url in repoDir.
func AddRemote(t *testing.T, repoDir, name, url string) {
	t.Helper()
	cmd := exec.Command("git", "remote", "add", name, url) //nolint:noctx // test helper, no context needed
	cmd.Dir = repoDir
	cmd.Env = GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add %s: %v\n%s", name, err, out)
	}
}

// WriteCheckpointPushRemoteSetting writes .entire/settings.json configuring
// strategy_options.checkpoint_push_remote to remoteName (with enabled: true).
func WriteCheckpointPushRemoteSetting(t *testing.T, repoDir, remoteName string) {
	t.Helper()
	content := `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "` + remoteName + `"}}`
	WriteFile(t, repoDir, filepath.Join(".entire", "settings.json"), content)
}

// GitUpdateRef points ref at hash in repoDir via git update-ref.
func GitUpdateRef(t *testing.T, repoDir, ref, hash string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", ref, hash) //nolint:noctx // test helper, no context needed
	cmd.Dir = repoDir
	cmd.Env = GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git update-ref %s %s: %v\n%s", ref, hash, err, out)
	}
}

// GitReset runs git reset --hard to the given ref.
func GitReset(t *testing.T, dir string, ref string) {
	t.Helper()
	cmd := exec.Command("git", "reset", "--hard", ref) //nolint:noctx // test helper, no context needed
	cmd.Dir = dir
	cmd.Env = GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git reset --hard %s: %v\n%s", ref, err, out)
	}
}

// BranchExists checks if a branch exists in the repository.
func BranchExists(t *testing.T, repoDir, branchName string) bool {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	refs, err := repo.References()
	if err != nil {
		t.Fatalf("failed to get references: %v", err)
	}

	found := false
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().Short() == branchName {
			found = true
		}
		return nil
	})

	return found
}

// gitEmptyConfigPath returns the path to a config file suitable for use as
// GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM. We use a real file instead of
// os.DevNull because git on Windows cannot open NUL as a config file.
//
// The file is not strictly empty: it pins background maintenance off so that
// no detached `git gc`/`git maintenance` process lingers after a test holding
// an open handle on the temp repo's .git/objects. Such a lingering process
// races t.TempDir()'s deferred RemoveAll and fails the test with
// "directory not empty" (see COR-394). Suppressing it centrally keeps every
// git-shelling test that uses GitIsolatedEnv/IsolateGitConfigEnv safe.
var gitEmptyConfig string
var gitEmptyConfigOnce sync.Once

// EnvGitHermetic, when set to a non-empty value, makes gitEmptyConfigPath append
// per-host HTTP proxy config that routes git HTTPS transport to real external
// hosts (github.com, gitlab.com) through an unroutable loopback proxy. Any test
// whose git commands accidentally dial those hosts then fails fast (connection
// refused at 127.0.0.1:1) instead of reaching the network or prompting for
// credentials. It is opt-in per test process — the integration TestMain sets it
// — so unit test packages that don't set it are unaffected. Because
// GitIsolatedEnv strips all inherited GIT_CONFIG_* env, this config must live in
// the file GIT_CONFIG_GLOBAL points at (this one), not in GIT_CONFIG_* env
// entries.
//
// A dead proxy (not url.insteadOf) is used deliberately: insteadOf rewrites the
// effective URL that git reports on read, which breaks production code that
// resolves the origin URL to detect the forge (e.g. `entire trail`). The proxy
// blocks transport only, leaving the configured URL string intact, and is scoped
// per host so loopback (127.0.0.1) test servers are never proxied.
//
// Regression class: tests accidentally hitting live github.com / the macOS
// keychain (#1463, 53bc37a88).
const EnvGitHermetic = "ENTIRE_TEST_GIT_HERMETIC"

// hermeticGitConfig routes HTTPS transport to real external hosts through a dead
// loopback proxy. Loopback test servers (127.0.0.1) and file:// / bare-path
// remotes are not proxied, so the in-process HTTPS git server still works. Only
// HTTPS is covered — the accidental-dial regression class is HTTPS fetches; SSH
// (git@…) to a real host fails on its own without credentials.
const hermeticGitConfig = "[http \"https://github.com/\"]\n" +
	"\tproxy = http://127.0.0.1:1\n" +
	"[http \"https://gitlab.com/\"]\n" +
	"\tproxy = http://127.0.0.1:1\n"

func gitEmptyConfigPath() string {
	gitEmptyConfigOnce.Do(func() {
		f, err := os.CreateTemp("", "git-isolation-config-*")
		if err != nil {
			panic("create git isolation config: " + err.Error())
		}
		content := "[gc]\n\tauto = 0\n\tautoDetach = false\n[maintenance]\n\tauto = false\n[fetch]\n\twriteCommitGraph = false\n"
		if os.Getenv(EnvGitHermetic) != "" {
			content += hermeticGitConfig
		}
		_, err = f.WriteString(content)
		if err != nil {
			panic("write git isolation config: " + err.Error())
		}
		if err := f.Close(); err != nil {
			panic("close git isolation config: " + err.Error())
		}
		gitEmptyConfig = f.Name()
	})
	return gitEmptyConfig
}

// GitIsolatedEnv returns os.Environ() with git isolation variables set.
// This prevents user/system git config (global gitignore, aliases, etc.) from
// affecting test behavior. Use this for any exec.Command that runs git or the
// CLI binary in integration tests.
//
// See https://git-scm.com/docs/git#Documentation/git.txt-GITCONFIGGLOBAL
//
// Every inherited GIT_CONFIG_* entry is filtered out — including
// GIT_CONFIG_PARAMETERS and the indexed KEY_/VALUE_ pairs that can inject
// `git -c` overrides — so our explicit isolation overrides take effect
// regardless of parent env.
//
// GIT_CONFIG_NOSYSTEM=1 is required, not redundant with GIT_CONFIG_SYSTEM.
// Apple Git still reads credential.helper=osxkeychain from
// /Library/Developer/CommandLineTools/usr/share/git-core/gitconfig when only
// GIT_CONFIG_SYSTEM is overridden; that helper is what hangs a 401 from a
// loopback HTTPS server (GIT_TERMINAL_PROMPT=0 does not stop helpers, and the
// hermetic proxy does not cover 127.0.0.1). IsolateGitConfigEnv sets the same
// flag for in-process git.
func GitIsolatedEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+3)
	for _, e := range env {
		if isGitConfigEnv(e) {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(filtered,
		"GIT_CONFIG_GLOBAL="+gitEmptyConfigPath(), // Isolate from user's global git config (e.g. global gitignore)
		"GIT_CONFIG_SYSTEM="+gitEmptyConfigPath(), // Isolate from system git config
		"GIT_CONFIG_NOSYSTEM=1",                   // Skip Apple git-core config (osxkeychain); GIT_CONFIG_SYSTEM does not replace it
	)
}

// IsolateGitConfigEnv applies the same git config isolation to the current
// process. Use this in tests that exercise production code paths which invoke
// git with os.Environ(). All inherited GIT_CONFIG_* variables are cleared
// before the isolation overrides are set, so values such as
// GIT_CONFIG_PARAMETERS or indexed KEY_/VALUE_ overrides cannot leak into
// child git invocations.
func IsolateGitConfigEnv(t *testing.T) {
	t.Helper()

	for _, e := range os.Environ() {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			t.Setenv(key, "")
		}
	}

	t.Setenv("GIT_CONFIG_GLOBAL", gitEmptyConfigPath())
	t.Setenv("GIT_CONFIG_SYSTEM", gitEmptyConfigPath())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
}

func isGitConfigEnv(e string) bool {
	return strings.HasPrefix(e, "GIT_CONFIG_")
}
