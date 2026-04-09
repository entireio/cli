//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// testBinaryPath holds the path to the CLI binary built once in TestMain.
// All tests share this binary to avoid repeated builds.
var testBinaryPath string

// getTestBinary returns the path to the shared test binary.
// It panics if TestMain hasn't run (testBinaryPath is empty).
func getTestBinary() string {
	if testBinaryPath == "" {
		panic("testBinaryPath not set - TestMain must run before tests")
	}
	return testBinaryPath
}

// TestEnv manages an isolated test environment for integration tests.
type TestEnv struct {
	T                  *testing.T
	RepoDir            string
	ClaudeProjectDir   string
	GeminiProjectDir   string
	OpenCodeProjectDir string
	SessionCounter     int
}

// NewTestEnv creates a new isolated test environment.
// It creates temp directories for the git repo and agent project files.
// Note: Does NOT change working directory to allow parallel test execution.
// Note: Does NOT use t.Setenv to allow parallel test execution - CLI commands
// receive the env var via cmd.Env instead.
func NewTestEnv(t *testing.T) *TestEnv {
	t.Helper()

	// Resolve symlinks on macOS where /var -> /private/var
	// This ensures the CLI subprocess and test use consistent paths
	repoDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = resolved
	}
	claudeProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(claudeProjectDir); err == nil {
		claudeProjectDir = resolved
	}
	geminiProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(geminiProjectDir); err == nil {
		geminiProjectDir = resolved
	}
	openCodeProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(openCodeProjectDir); err == nil {
		openCodeProjectDir = resolved
	}

	env := &TestEnv{
		T:                  t,
		RepoDir:            repoDir,
		ClaudeProjectDir:   claudeProjectDir,
		GeminiProjectDir:   geminiProjectDir,
		OpenCodeProjectDir: openCodeProjectDir,
	}

	// Note: Don't use t.Setenv here - it's incompatible with t.Parallel()
	// CLI commands receive ENTIRE_TEST_*_PROJECT_DIR via cmd.Env instead

	return env
}

// Cleanup is a no-op retained for backwards compatibility.
//
// Previously this method restored the working directory after NewTestEnv changed it.
// With the refactor to remove os.Chdir from NewTestEnv:
// - Temp directories are now cleaned up automatically by t.TempDir()
// - Working directory is never changed, so no restoration is needed
//
// This method is kept to avoid breaking existing tests that call defer env.Cleanup().
// New tests should not call this method as it serves no purpose.
//
// Deprecated: This method is a no-op and will be removed in a future version.
func (env *TestEnv) Cleanup() {
	// No-op - temp dirs are cleaned up by t.TempDir()
}

// gitEmptyConfigPath returns the path to an empty file suitable for use as
// GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM. We use an empty file instead of
// os.DevNull because git on Windows cannot open NUL as a config file.
var gitEmptyConfig string

func gitEmptyConfigPath() string {
	if gitEmptyConfig == "" {
		f, err := os.CreateTemp("", "git-empty-config-*")
		if err != nil {
			panic("create empty git config: " + err.Error())
		}
		f.Close()
		gitEmptyConfig = f.Name()
	}
	return gitEmptyConfig
}

// cliEnv returns the environment variables for CLI execution.
// Includes Claude, Gemini, and OpenCode project dirs so tests work for any agent.
// Delegates to testutil.GitIsolatedEnv() for git config isolation.
func (env *TestEnv) cliEnv() []string {
	return append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+env.GeminiProjectDir,
		"ENTIRE_TEST_OPENCODE_PROJECT_DIR="+env.OpenCodeProjectDir,
		"ENTIRE_TEST_TTY=0", // Prevent interactive prompts from blocking in tests
	)
}

// RunCLI runs the entire CLI with the given arguments and returns stdout.
func (env *TestEnv) RunCLI(args ...string) string {
	env.T.Helper()
	output, err := env.RunCLIWithError(args...)
	if err != nil {
		env.T.Fatalf("CLI command failed: %v\nArgs: %v\nOutput: %s", err, args, output)
	}
	return output
}

// RunCLIWithError runs the entire CLI and returns output and error.
func (env *TestEnv) RunCLIWithError(args ...string) (string, error) {
	env.T.Helper()

	// Run CLI using the shared binary
	cmd := exec.Command(getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunCLIWithStdin runs the CLI with stdin input.
func (env *TestEnv) RunCLIWithStdin(stdin string, args ...string) string {
	env.T.Helper()

	// Run CLI with stdin using the shared binary
	cmd := exec.Command(getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	cmd.Stdin = strings.NewReader(stdin)

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("CLI command failed: %v\nArgs: %v\nOutput: %s", err, args, output)
	}
	return string(output)
}

// NewRepoEnv creates a TestEnv with an initialized git repo and Entire.
// This is a convenience factory for tests that need a basic repo setup.
func NewRepoEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	return env
}

// NewRepoWithCommit creates a TestEnv with a git repo, Entire, and an initial commit.
// The initial commit contains a README.md and .gitignore (excluding .entire/).
func NewRepoWithCommit(t *testing.T) *TestEnv {
	t.Helper()
	env := NewRepoEnv(t)
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	return env
}

// NewFeatureBranchEnv creates a TestEnv ready for session testing.
// It initializes the repo, creates an initial commit on main,
// and checks out a feature branch. This is the most common setup
// for session and rewind tests since Entire tracking skips main/master.
func NewFeatureBranchEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewRepoWithCommit(t)
	env.GitCheckoutNewBranch("feature/test-branch")
	return env
}

// InitRepo initializes a git repository in the test environment.
func (env *TestEnv) InitRepo() {
	env.T.Helper()

	repo, err := git.PlainInit(env.RepoDir, false)
	if err != nil {
		env.T.Fatalf("failed to init git repo: %v", err)
	}

	// Configure git user for commits
	cfg, err := repo.Config()
	if err != nil {
		env.T.Fatalf("failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"

	// Disable GPG signing for test commits (prevents failures if user has commit.gpgsign=true globally)
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")

	// Override any global core.hooksPath so tests use the repo-local hooks directory.
	cfg.Raw.Section("core").SetOption("hooksPath", filepath.Join(env.RepoDir, ".git", "hooks"))

	if err := repo.SetConfig(cfg); err != nil {
		env.T.Fatalf("failed to set repo config: %v", err)
	}
}

// InitEntire initializes the .entire directory with the specified strategy.
func (env *TestEnv) InitEntire() {
	env.InitEntireWithOptions(nil)
}

// InitEntireWithOptions initializes the .entire directory with the specified strategy and options.
func (env *TestEnv) InitEntireWithOptions(strategyOptions map[string]any) {
	env.T.Helper()
	env.initEntireInternal(strategyOptions)
}

// InitEntireWithAgent initializes an Entire test environment with a specific agent.
// The agent name is for test documentation only — the CLI resolves the agent from
// hook commands and checkpoint metadata, not from settings.json.
func (env *TestEnv) InitEntireWithAgent(_ types.AgentName) {
	env.T.Helper()
	env.initEntireInternal(nil)
}

// InitEntireWithAgentAndOptions initializes Entire with the specified strategy, agent, and options.
func (env *TestEnv) InitEntireWithAgentAndOptions(_ types.AgentName, strategyOptions map[string]any) {
	env.T.Helper()
	env.initEntireInternal(strategyOptions)
}

// initEntireInternal is the common implementation for InitEntire variants.
func (env *TestEnv) initEntireInternal(strategyOptions map[string]any) {
	env.T.Helper()

	// Create .entire directory structure
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		env.T.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create tmp directory
	tmpDir := filepath.Join(entireDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		env.T.Fatalf("failed to create .entire/tmp directory: %v", err)
	}

	// Write settings.json
	// Note: The agent name is NOT stored in settings.json — the CLI determines
	// the agent from installed hooks (detect presence) or checkpoint metadata.
	// The settings parser uses DisallowUnknownFields(), so only recognized fields are allowed.
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true, // Note: git-triggered hooks won't work (path is relative); tests call hooks via getTestBinary() instead
	}
	if strategyOptions != nil {
		settings["strategy_options"] = strategyOptions
	}
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		env.T.Fatalf("failed to marshal settings: %v", err)
	}
	settingsPath := filepath.Join(entireDir, paths.SettingsFileName)
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		env.T.Fatalf("failed to write %s: %v", paths.SettingsFileName, err)
	}
}

// WriteFile creates a file with the given content in the test repo.
// It creates parent directories as needed.
func (env *TestEnv) WriteFile(path, content string) {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)

	// Create parent directories
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		env.T.Fatalf("failed to create directory %s: %v", dir, err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		env.T.Fatalf("failed to write file %s: %v", path, err)
	}
}

// ReadFile reads a file from the test repo.
func (env *TestEnv) ReadFile(path string) string {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		env.T.Fatalf("failed to read file %s: %v", path, err)
	}
	return string(data)
}

// ReadFileAbsolute reads a file using an absolute path.
func (env *TestEnv) ReadFileAbsolute(path string) string {
	env.T.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		env.T.Fatalf("failed to read file %s: %v", path, err)
	}
	return string(data)
}

// FileExists checks if a file exists in the test repo.
func (env *TestEnv) FileExists(path string) bool {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)
	_, err := os.Stat(fullPath)
	return err == nil
}

// GitAdd stages files for commit.
func (env *TestEnv) GitAdd(paths ...string) {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	for _, path := range paths {
		if _, err := worktree.Add(path); err != nil {
			env.T.Fatalf("failed to add file %s: %v", path, err)
		}
	}
}

// GitCommit creates a commit with all staged files.
func (env *TestEnv) GitCommit(message string) {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GitCommitWithMetadata creates a commit with Entire-Metadata trailer.
// This simulates commits created by the commit strategy.
func (env *TestEnv) GitCommitWithMetadata(message, metadataDir string) {
	env.T.Helper()

	// Format message with metadata trailer
	fullMessage := message + "\n\nEntire-Metadata: " + metadataDir + "\n"

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(fullMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GitCommitWithCheckpointID creates a commit with Entire-Checkpoint trailer.
// This simulates commits.
func (env *TestEnv) GitCommitWithCheckpointID(message, checkpointID string) {
	env.T.Helper()

	// Format message with checkpoint trailer
	fullMessage := message + "\n\nEntire-Checkpoint: " + checkpointID + "\n"

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(fullMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GitCommitWithMultipleSessions creates a commit with multiple Entire-Session trailers.
// This simulates merge commits that combine work from multiple sessions.
func (env *TestEnv) GitCommitWithMultipleSessions(message string, sessionIDs []string) {
	env.T.Helper()

	// Format message with multiple session trailers
	fullMessage := message + "\n\n"
	var fullMessageSb404 strings.Builder
	for _, sessionID := range sessionIDs {
		fullMessageSb404.WriteString("Entire-Session: " + sessionID + "\n")
	}
	fullMessage += fullMessageSb404.String()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(fullMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GitCommitWithMultipleCheckpoints creates a commit with multiple Entire-Checkpoint trailers.
// This simulates a GitHub squash merge commit where multiple individual commits with
// checkpoint trailers are combined into a single commit message.
func (env *TestEnv) GitCommitWithMultipleCheckpoints(message string, checkpointIDs []string) {
	env.T.Helper()

	// Format message with multiple checkpoint trailers (simulating squash merge format)
	var sb strings.Builder
	sb.WriteString(message)
	sb.WriteString("\n\n")
	for _, cpID := range checkpointIDs {
		sb.WriteString("Entire-Checkpoint: " + cpID + "\n")
	}

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(sb.String(), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GetHeadHash returns the current HEAD commit hash.
func (env *TestEnv) GetHeadHash() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		env.T.Fatalf("failed to get HEAD: %v", err)
	}

	return head.Hash().String()
}

// GetShadowBranchName returns the worktree-specific shadow branch name for the current HEAD.
// Format: entire/<commit[:7]>-<hash(worktreeID)[:6]>
func (env *TestEnv) GetShadowBranchName() string {
	env.T.Helper()

	headHash := env.GetHeadHash()
	worktreeID, err := paths.GetWorktreeID(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to get worktree ID: %v", err)
	}
	return checkpoint.ShadowBranchNameForCommit(headHash, worktreeID)
}

// GetShadowBranchNameForCommit returns the worktree-specific shadow branch name for a given commit.
// Format: entire/<commit[:7]>-<hash(worktreeID)[:6]>
func (env *TestEnv) GetShadowBranchNameForCommit(commitHash string) string {
	env.T.Helper()

	worktreeID, err := paths.GetWorktreeID(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to get worktree ID: %v", err)
	}
	return checkpoint.ShadowBranchNameForCommit(commitHash, worktreeID)
}

// GetGitLog returns a list of commit hashes from HEAD.
func (env *TestEnv) GetGitLog() []string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		env.T.Fatalf("failed to get HEAD: %v", err)
	}

	commitIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		env.T.Fatalf("failed to get log: %v", err)
	}

	var commits []string
	err = commitIter.ForEach(func(c *object.Commit) error {
		commits = append(commits, c.Hash.String())
		return nil
	})
	if err != nil {
		env.T.Fatalf("failed to iterate commits: %v", err)
	}

	return commits
}

// GitCheckoutNewBranch creates and checks out a new branch.
// Uses git CLI instead of go-git to work around go-git v5 bug where Checkout
// deletes untracked files (see https://github.com/go-git/go-git/issues/970).
func (env *TestEnv) GitCheckoutNewBranch(branchName string) {
	env.T.Helper()

	cmd := exec.Command("git", "checkout", "-b", branchName)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to checkout new branch %s: %v\nOutput: %s", branchName, err, output)
	}
}

// GetCurrentBranch returns the current branch name.
func (env *TestEnv) GetCurrentBranch() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		env.T.Fatalf("failed to get HEAD: %v", err)
	}

	if !head.Name().IsBranch() {
		return "" // Detached HEAD
	}

	return head.Name().Short()
}

// RewindPoint mirrors strategy.RewindPoint for test assertions.
type RewindPoint struct {
	ID               string
	Message          string
	MetadataDir      string
	Date             time.Time
	IsTaskCheckpoint bool
	ToolUseID        string
	IsLogsOnly       bool
	CondensationID   string
}

// GetRewindPoints returns available rewind points using the CLI.
func (env *TestEnv) GetRewindPoints() []RewindPoint {
	env.T.Helper()

	// Run rewind --list using the shared binary
	cmd := exec.Command(getTestBinary(), "rewind", "--list")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("rewind --list failed: %v\nOutput: %s", err, output)
	}

	// Parse JSON output
	var jsonPoints []struct {
		ID               string `json:"id"`
		Message          string `json:"message"`
		MetadataDir      string `json:"metadata_dir"`
		Date             string `json:"date"`
		IsTaskCheckpoint bool   `json:"is_task_checkpoint"`
		ToolUseID        string `json:"tool_use_id"`
		IsLogsOnly       bool   `json:"is_logs_only"`
		CondensationID   string `json:"condensation_id"`
	}

	if err := json.Unmarshal(output, &jsonPoints); err != nil {
		env.T.Fatalf("failed to parse rewind points: %v\nOutput: %s", err, output)
	}

	points := make([]RewindPoint, len(jsonPoints))
	for i, jp := range jsonPoints {
		date, _ := time.Parse(time.RFC3339, jp.Date)
		points[i] = RewindPoint{
			ID:               jp.ID,
			Message:          jp.Message,
			MetadataDir:      jp.MetadataDir,
			Date:             date,
			IsTaskCheckpoint: jp.IsTaskCheckpoint,
			ToolUseID:        jp.ToolUseID,
			IsLogsOnly:       jp.IsLogsOnly,
			CondensationID:   jp.CondensationID,
		}
	}

	return points
}

// Rewind performs a rewind to the specified commit ID using the CLI.
func (env *TestEnv) Rewind(commitID string) error {
	env.T.Helper()

	// Run rewind --to <commitID> using the shared binary
	cmd := exec.Command(getTestBinary(), "rewind", "--to", commitID)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("rewind failed: " + string(output))
	}

	env.T.Logf("Rewind output: %s", output)
	return nil
}

// RewindLogsOnly performs a logs-only rewind using the CLI.
// This restores session logs without modifying the working directory.
func (env *TestEnv) RewindLogsOnly(commitID string) error {
	env.T.Helper()

	// Run rewind --to <commitID> --logs-only using the shared binary
	cmd := exec.Command(getTestBinary(), "rewind", "--to", commitID, "--logs-only")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("rewind logs-only failed: " + string(output))
	}

	env.T.Logf("Rewind logs-only output: %s", output)
	return nil
}

// RewindReset performs a reset rewind using the CLI.
// This resets the branch to the specified commit (destructive).
func (env *TestEnv) RewindReset(commitID string) error {
	env.T.Helper()

	// Run rewind --to <commitID> --reset using the shared binary
	cmd := exec.Command(getTestBinary(), "rewind", "--to", commitID, "--reset")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("rewind reset failed: " + string(output))
	}

	env.T.Logf("Rewind reset output: %s", output)
	return nil
}

// BranchExists checks if a branch exists in the repository.
func (env *TestEnv) BranchExists(branchName string) bool {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	_, err = repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	return err == nil
}

// GetCommitMessage returns the commit message for the given commit hash.
func (env *TestEnv) GetCommitMessage(hash string) string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	commitHash := plumbing.NewHash(hash)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		env.T.Fatalf("failed to get commit %s: %v", hash, err)
	}

	return commit.Message
}

// FileExistsInBranch checks if a file exists in a specific branch's tree.
func (env *TestEnv) FileExistsInBranch(branchName, filePath string) bool {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	// Get the branch reference
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		// Try as a remote-style ref
		ref, err = repo.Reference(plumbing.ReferenceName("refs/heads/"+branchName), true)
		if err != nil {
			return false
		}
	}

	// Get the commit
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return false
	}

	// Get the tree
	tree, err := commit.Tree()
	if err != nil {
		return false
	}

	// Check if file exists
	_, err = tree.File(filePath)
	return err == nil
}

// ReadFileFromBranch reads a file's content from a specific branch's tree.
// Returns the content and true if found, empty string and false if not found.
func (env *TestEnv) ReadFileFromBranch(branchName, filePath string) (string, bool) {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	// Get the branch reference
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		// Try as a remote-style ref
		ref, err = repo.Reference(plumbing.ReferenceName("refs/heads/"+branchName), true)
		if err != nil {
			return "", false
		}
	}

	// Get the commit
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}

	// Get the tree
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}

	// Get the file
	file, err := tree.File(filePath)
	if err != nil {
		return "", false
	}

	// Get the content
	content, err := file.Contents()
	if err != nil {
		return "", false
	}

	return content, true
}

// ReadFileFromRef reads a file's content from a specific ref's tree.
// Unlike ReadFileFromBranch, this takes a full ref name (e.g., "refs/entire/checkpoints/v2/main")
// and does not prepend "refs/heads/".
// Returns the content and true if found, empty string and false if not found.
func (env *TestEnv) ReadFileFromRef(refName, filePath string) (string, bool) {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	ref, err := repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil {
		return "", false
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}

	file, err := tree.File(filePath)
	if err != nil {
		return "", false
	}

	content, err := file.Contents()
	if err != nil {
		return "", false
	}

	return content, true
}

// AssertCheckpointContainsSession verifies that the checkpoint summary includes
// a session with the given session ID by reading per-session metadata from the
// metadata branch.
func (env *TestEnv) AssertCheckpointContainsSession(t *testing.T, summary checkpoint.CheckpointSummary, sessionID string) {
	t.Helper()
	for _, s := range summary.Sessions {
		if env.sessionMetadataMatchesID(s.Metadata, sessionID) {
			return
		}
	}
	t.Errorf("Checkpoint did not include session %q", sessionID)
}

// AssertCheckpointExcludesSession verifies that the checkpoint summary does NOT
// include a session with the given session ID.
func (env *TestEnv) AssertCheckpointExcludesSession(t *testing.T, summary checkpoint.CheckpointSummary, sessionID string) {
	t.Helper()
	for _, s := range summary.Sessions {
		if env.sessionMetadataMatchesID(s.Metadata, sessionID) {
			t.Errorf("Checkpoint incorrectly included session %q", sessionID)
			return
		}
	}
}

// sessionMetadataMatchesID reads session metadata from the metadata branch and
// checks if it belongs to the given session ID.
func (env *TestEnv) sessionMetadataMatchesID(metadataPath, sessionID string) bool {
	// Strip leading slash — git tree paths are relative
	cleanPath := strings.TrimPrefix(metadataPath, "/")
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, cleanPath)
	if !found {
		return false
	}
	var meta checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(content), &meta); err != nil {
		return false
	}
	return meta.SessionID == sessionID
}

// RefExists checks if a ref exists in the repository.
func (env *TestEnv) RefExists(refName string) bool {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	_, err = repo.Reference(plumbing.ReferenceName(refName), true)
	return err == nil
}

// GetLatestCommitMessageOnBranch returns the commit message of the latest commit on the given branch.
func (env *TestEnv) GetLatestCommitMessageOnBranch(branchName string) string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	// Get the branch reference
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		env.T.Fatalf("failed to get branch %s reference: %v", branchName, err)
	}

	// Get the commit
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		env.T.Fatalf("failed to get commit object: %v", err)
	}

	return commit.Message
}

// GitCommitWithShadowHooks stages and commits files, simulating the prepare-commit-msg
// and post-commit hooks as a human (with TTY). This is the default for tests.
func (env *TestEnv) GitCommitWithShadowHooks(message string, files ...string) {
	env.T.Helper()
	env.gitCommitWithShadowHooks(message, true, files...)
}

// GitCommitWithShadowHooksAsAgent is like GitCommitWithShadowHooks but simulates
// an agent commit (no TTY). This triggers the fast path in PrepareCommitMsg that
// skips content detection and interactive prompts for ACTIVE sessions.
func (env *TestEnv) GitCommitWithShadowHooksAsAgent(message string, files ...string) {
	env.T.Helper()
	env.gitCommitWithShadowHooks(message, false, files...)
}

// gitCommitWithShadowHooks is the shared implementation for committing with shadow hooks.
// When simulateTTY is true, sets ENTIRE_TEST_TTY=1 to simulate a human at the terminal.
// When false, filters it out to simulate an agent subprocess (no controlling terminal).
func (env *TestEnv) gitCommitWithShadowHooks(message string, simulateTTY bool, files ...string) {
	env.T.Helper()

	// Stage files using go-git
	for _, file := range files {
		env.GitAdd(file)
	}

	// Create a temp file for the commit message (prepare-commit-msg hook modifies this)
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		env.T.Fatalf("failed to write commit message file: %v", err)
	}

	// Run prepare-commit-msg hook using the shared binary.
	// Pass source="message" to match real `git commit -m` behavior.
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile, "message")
	prepCmd.Dir = env.RepoDir
	if simulateTTY {
		// Simulate human at terminal: ENTIRE_TEST_TTY=1 makes hasTTY() return true
		// and askConfirmTTY() return defaultYes without reading from /dev/tty.
		prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=1")
	} else {
		// Simulate agent: ENTIRE_TEST_TTY=0 makes hasTTY() return false,
		// triggering the fast path that adds trailers for ACTIVE sessions.
		prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=0")
	}
	if output, err := prepCmd.CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", output)
		// Don't fail - hook may silently succeed
	}

	// Read the modified message
	modifiedMsg, err := os.ReadFile(msgFile)
	if err != nil {
		env.T.Fatalf("failed to read modified commit message: %v", err)
	}

	// Create the commit using go-git with the modified message
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(string(modifiedMsg), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}

	// Run post-commit hook using the shared binary
	// This triggers condensation if the commit has an Entire-Checkpoint trailer
	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", output)
		// Don't fail - hook may silently succeed
	}
}

func (env *TestEnv) gitHookEnv(extra ...string) []string {
	envVars := append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_OPENCODE_PROJECT_DIR="+env.OpenCodeProjectDir,
		"ENTIRE_TEST_OPENCODE_MOCK_EXPORT=1",
	)
	return append(envVars, extra...)
}

// GitCommitAmendWithShadowHooks amends the last commit with shadow hooks.
// This simulates `git commit --amend` with the prepare-commit-msg and post-commit hooks.
// The prepare-commit-msg hook is called with "commit" source to indicate an amend.
func (env *TestEnv) GitCommitAmendWithShadowHooks(message string, files ...string) {
	env.T.Helper()

	// Stage any additional files
	for _, file := range files {
		env.GitAdd(file)
	}

	// Write commit message to temp file
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		env.T.Fatalf("failed to write commit message file: %v", err)
	}

	// Run prepare-commit-msg hook with "commit" source (indicates amend).
	// Set ENTIRE_TEST_TTY=1 to simulate human (amend is always a human operation).
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile, "commit")
	prepCmd.Dir = env.RepoDir
	prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=1")
	if output, err := prepCmd.CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg (amend) output: %s", output)
	}

	// Read the modified message
	modifiedMsg, err := os.ReadFile(msgFile)
	if err != nil {
		env.T.Fatalf("failed to read modified commit message: %v", err)
	}

	// Amend the commit using go-git
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(string(modifiedMsg), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
		Amend: true,
	})
	if err != nil {
		env.T.Fatalf("failed to amend commit: %v", err)
	}

	// Run post-commit hook
	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit (amend) output: %s", output)
	}
}

// GitCommitWithTrailerRemoved stages and commits files, simulating what happens when
// a user removes the Entire-Checkpoint trailer during commit message editing.
// This tests the opt-out behavior where removing the trailer skips condensation.
func (env *TestEnv) GitCommitWithTrailerRemoved(message string, files ...string) {
	env.T.Helper()

	// Stage files using go-git
	for _, file := range files {
		env.GitAdd(file)
	}

	// Create a temp file for the commit message (prepare-commit-msg hook modifies this)
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		env.T.Fatalf("failed to write commit message file: %v", err)
	}

	// Run prepare-commit-msg hook using the shared binary.
	// Set ENTIRE_TEST_TTY=1 to simulate human (this tests the editor flow where
	// the user removes the trailer before committing).
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile)
	prepCmd.Dir = env.RepoDir
	prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=1")
	if output, err := prepCmd.CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", output)
	}

	// Read the modified message (with trailer added by hook)
	modifiedMsg, err := os.ReadFile(msgFile)
	if err != nil {
		env.T.Fatalf("failed to read modified commit message: %v", err)
	}

	// REMOVE the Entire-Checkpoint trailer (simulating user editing the message)
	lines := strings.Split(string(modifiedMsg), "\n")
	var cleanedLines []string
	for _, line := range lines {
		// Skip the trailer and the comments about it
		if strings.HasPrefix(line, "Entire-Checkpoint:") {
			continue
		}
		if strings.Contains(line, "Remove the Entire-Checkpoint trailer") {
			continue
		}
		if strings.Contains(line, "trailer will be added to your next commit") {
			continue
		}
		cleanedLines = append(cleanedLines, line)
	}
	cleanedMsg := strings.TrimRight(strings.Join(cleanedLines, "\n"), "\n") + "\n"

	// Create the commit using go-git with the cleaned message (no trailer)
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(cleanedMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}

	// Run post-commit hook - since trailer was removed, no condensation should happen
	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", output)
	}
}

// GitRm stages file deletions using git rm.
func (env *TestEnv) GitRm(paths ...string) {
	env.T.Helper()

	args := append([]string{"rm", "--"}, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git rm failed: %v\nOutput: %s", err, output)
	}
}

// GitCommitStagedWithShadowHooks commits whatever is already staged (without adding files first),
// running the prepare-commit-msg and post-commit hooks like a real workflow.
// Use this after GitRm or when files are already staged.
func (env *TestEnv) GitCommitStagedWithShadowHooks(message string) {
	env.T.Helper()
	env.gitCommitStagedWithShadowHooks(message, true)
}

// gitCommitStagedWithShadowHooks is the shared implementation for committing staged changes with hooks.
func (env *TestEnv) gitCommitStagedWithShadowHooks(message string, simulateTTY bool) {
	env.T.Helper()

	// Create a temp file for the commit message (prepare-commit-msg hook modifies this)
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		env.T.Fatalf("failed to write commit message file: %v", err)
	}

	// Run prepare-commit-msg hook using the shared binary.
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile, "message")
	prepCmd.Dir = env.RepoDir
	if simulateTTY {
		prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=1")
	} else {
		prepCmd.Env = env.gitHookEnv("ENTIRE_TEST_TTY=0")
	}
	if output, err := prepCmd.CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", output)
	}

	// Read the modified message
	modifiedMsg, err := os.ReadFile(msgFile)
	if err != nil {
		env.T.Fatalf("failed to read modified commit message: %v", err)
	}

	// Create the commit using go-git with the modified message
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(string(modifiedMsg), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}

	// Run post-commit hook
	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", output)
	}
}

// ListBranchesWithPrefix returns all branches that start with the given prefix.
func (env *TestEnv) ListBranchesWithPrefix(prefix string) []string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	refs, err := repo.References()
	if err != nil {
		env.T.Fatalf("failed to get references: %v", err)
	}

	var branches []string
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			branches = append(branches, name)
		}
		return nil
	})

	return branches
}

// GetLatestCheckpointID returns the most recent checkpoint ID from the entire/checkpoints/v1 branch.
// This is used by tests that previously extracted the checkpoint ID from commit message trailers.
// Now that active branch commits are clean (no trailers), we get the ID from the sessions branch.
// Fatals if the checkpoint ID cannot be found, with detailed context about what was found.
func (env *TestEnv) GetLatestCheckpointID() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	// Get the entire/checkpoints/v1 branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		env.T.Fatalf("failed to get %s branch: %v", paths.MetadataBranchName, err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		env.T.Fatalf("failed to get commit: %v", err)
	}

	// Extract checkpoint ID from commit message
	// Format: "Checkpoint: <12-hex-char-id>\n\nSession: ...\nStrategy: ..."
	for _, line := range strings.Split(commit.Message, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Checkpoint: ") {
			return strings.TrimPrefix(line, "Checkpoint: ")
		}
	}

	env.T.Fatalf("could not find checkpoint ID in %s branch commit message:\n%s",
		paths.MetadataBranchName, commit.Message)
	return ""
}

// TryGetLatestCheckpointID returns the most recent checkpoint ID from the entire/checkpoints/v1 branch.
// Returns empty string if the branch doesn't exist or has no checkpoint commits yet.
// Use this when you need to check if a checkpoint exists without failing the test.
func (env *TestEnv) TryGetLatestCheckpointID() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		return ""
	}

	// Get the entire/checkpoints/v1 branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return ""
	}

	// Extract checkpoint ID from commit message
	// Format: "Checkpoint: <12-hex-char-id>\n\nSession: ...\nStrategy: ..."
	for _, line := range strings.Split(commit.Message, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Checkpoint: ") {
			return strings.TrimPrefix(line, "Checkpoint: ")
		}
	}

	return ""
}

// GetLatestCondensationID is an alias for GetLatestCheckpointID for backwards compatibility.
func (env *TestEnv) GetLatestCondensationID() string {
	return env.GetLatestCheckpointID()
}

// GetCheckpointIDFromCommitMessage extracts the Entire-Checkpoint trailer from a commit message.
// Returns empty string if no trailer found.
func (env *TestEnv) GetCheckpointIDFromCommitMessage(commitSHA string) string {
	env.T.Helper()

	msg := env.GetCommitMessage(commitSHA)
	cpID, found := trailers.ParseCheckpoint(msg)
	if !found {
		return ""
	}
	return cpID.String()
}

// GetLatestCheckpointIDFromHistory walks backwards from HEAD on the active branch
// and returns the checkpoint ID from the first commit that has an Entire-Checkpoint trailer.
// This verifies that condensation actually happened (commit has trailer) without relying
// on timestamp-based matching.
func (env *TestEnv) GetLatestCheckpointIDFromHistory() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		env.T.Fatalf("failed to get HEAD: %v", err)
	}

	commitIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		env.T.Fatalf("failed to iterate commits: %v", err)
	}

	var checkpointID string
	//nolint:errcheck // ForEach callback handles errors
	commitIter.ForEach(func(c *object.Commit) error {
		if cpID, found := trailers.ParseCheckpoint(c.Message); found {
			checkpointID = cpID.String()
			return errors.New("stop iteration") // Found it, stop
		}
		return nil
	})

	if checkpointID == "" {
		env.T.Fatalf("no commit with Entire-Checkpoint trailer found in history")
	}

	return checkpointID
}

// ShardedCheckpointPath returns the sharded path for a checkpoint ID.
// Format: <id[:2]>/<id[2:]>
// Delegates to id.CheckpointID.Path() for consistency.
func ShardedCheckpointPath(checkpointID string) string {
	return id.CheckpointID(checkpointID).Path()
}

// SessionFilePath returns the path to a session file within a checkpoint.
// Session files are stored in numbered subdirectories using 0-based indexing (e.g., 0/full.jsonl).
// This function constructs the path for the first (default) session.
func SessionFilePath(checkpointID string, fileName string) string {
	return id.CheckpointID(checkpointID).Path() + "/0/" + fileName
}

// CheckpointSummaryPath returns the path to the root metadata.json (CheckpointSummary) for a checkpoint.
func CheckpointSummaryPath(checkpointID string) string {
	return id.CheckpointID(checkpointID).Path() + "/" + paths.MetadataFileName
}

// SessionMetadataPath returns the path to the session-level metadata.json for a checkpoint.
func SessionMetadataPath(checkpointID string) string {
	return SessionFilePath(checkpointID, paths.MetadataFileName)
}

// CheckpointValidation contains expected values for checkpoint validation.
type CheckpointValidation struct {
	// CheckpointID is the expected checkpoint ID
	CheckpointID string

	// SessionID is the expected session ID
	SessionID string

	// Strategy is the expected strategy name
	Strategy string

	// FilesTouched are the expected files in files_touched
	FilesTouched []string

	// ExpectedPrompts are strings that should appear in prompt.txt
	ExpectedPrompts []string

	// ExpectedTranscriptContent are strings that should appear in full.jsonl
	ExpectedTranscriptContent []string

	// CheckpointsCount is the expected checkpoint count (0 means don't validate)
	CheckpointsCount int
}

// ValidateCheckpoint performs comprehensive validation of a checkpoint on the metadata branch.
// It validates:
// - Root metadata.json (CheckpointSummary) structure and expected fields
// - Session metadata.json (CommittedMetadata) structure and expected fields
// - Transcript file (full.jsonl) is valid JSONL and contains expected content
// - Content hash file (content_hash.txt) matches SHA256 of transcript
// - Prompt file (prompt.txt) contains expected prompts
func (env *TestEnv) ValidateCheckpoint(v CheckpointValidation) {
	env.T.Helper()

	// Validate root metadata.json (CheckpointSummary)
	env.validateCheckpointSummary(v)

	// Validate session metadata.json (CommittedMetadata)
	env.validateSessionMetadata(v)

	// Validate transcript is valid JSONL
	env.validateTranscriptJSONL(v.CheckpointID, v.ExpectedTranscriptContent)

	// Validate content hash matches transcript
	env.validateContentHash(v.CheckpointID)

	// Validate prompt.txt contains expected prompts
	if len(v.ExpectedPrompts) > 0 {
		env.validatePromptContent(v.CheckpointID, v.ExpectedPrompts)
	}
}

// validateCheckpointSummary validates the root metadata.json (CheckpointSummary).
func (env *TestEnv) validateCheckpointSummary(v CheckpointValidation) {
	env.T.Helper()

	summaryPath := CheckpointSummaryPath(v.CheckpointID)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, summaryPath)
	if !found {
		env.T.Fatalf("CheckpointSummary not found at %s", summaryPath)
	}

	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err != nil {
		env.T.Fatalf("Failed to parse CheckpointSummary: %v\nContent: %s", err, content)
	}

	// Validate checkpoint_id
	if summary.CheckpointID.String() != v.CheckpointID {
		env.T.Errorf("CheckpointSummary.CheckpointID = %q, want %q", summary.CheckpointID, v.CheckpointID)
	}

	// Validate strategy
	if v.Strategy != "" && summary.Strategy != v.Strategy {
		env.T.Errorf("CheckpointSummary.Strategy = %q, want %q", summary.Strategy, v.Strategy)
	}

	// Validate sessions array is populated
	if len(summary.Sessions) == 0 {
		env.T.Error("CheckpointSummary.Sessions should have at least one entry")
	}

	// Validate files_touched
	if len(v.FilesTouched) > 0 {
		touchedSet := make(map[string]bool)
		for _, f := range summary.FilesTouched {
			touchedSet[f] = true
		}
		for _, expected := range v.FilesTouched {
			if !touchedSet[expected] {
				env.T.Errorf("CheckpointSummary.FilesTouched missing %q, got %v", expected, summary.FilesTouched)
			}
		}
	}

	// Validate checkpoints_count
	if v.CheckpointsCount > 0 && summary.CheckpointsCount != v.CheckpointsCount {
		env.T.Errorf("CheckpointSummary.CheckpointsCount = %d, want %d", summary.CheckpointsCount, v.CheckpointsCount)
	}
}

// validateSessionMetadata validates the session-level metadata.json (CommittedMetadata).
func (env *TestEnv) validateSessionMetadata(v CheckpointValidation) {
	env.T.Helper()

	metadataPath := SessionMetadataPath(v.CheckpointID)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, metadataPath)
	if !found {
		env.T.Fatalf("Session metadata not found at %s", metadataPath)
	}

	var metadata checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		env.T.Fatalf("Failed to parse CommittedMetadata: %v\nContent: %s", err, content)
	}

	// Validate checkpoint_id
	if metadata.CheckpointID.String() != v.CheckpointID {
		env.T.Errorf("CommittedMetadata.CheckpointID = %q, want %q", metadata.CheckpointID, v.CheckpointID)
	}

	// Validate session_id
	if v.SessionID != "" && metadata.SessionID != v.SessionID {
		env.T.Errorf("CommittedMetadata.SessionID = %q, want %q", metadata.SessionID, v.SessionID)
	}

	// Validate strategy
	if v.Strategy != "" && metadata.Strategy != v.Strategy {
		env.T.Errorf("CommittedMetadata.Strategy = %q, want %q", metadata.Strategy, v.Strategy)
	}

	// Validate created_at is not zero
	if metadata.CreatedAt.IsZero() {
		env.T.Error("CommittedMetadata.CreatedAt should not be zero")
	}

	// Validate files_touched
	if len(v.FilesTouched) > 0 {
		touchedSet := make(map[string]bool)
		for _, f := range metadata.FilesTouched {
			touchedSet[f] = true
		}
		for _, expected := range v.FilesTouched {
			if !touchedSet[expected] {
				env.T.Errorf("CommittedMetadata.FilesTouched missing %q, got %v", expected, metadata.FilesTouched)
			}
		}
	}

	// Validate checkpoints_count
	if v.CheckpointsCount > 0 && metadata.CheckpointsCount != v.CheckpointsCount {
		env.T.Errorf("CommittedMetadata.CheckpointsCount = %d, want %d", metadata.CheckpointsCount, v.CheckpointsCount)
	}
}

// validateTranscriptJSONL validates that full.jsonl exists and is valid JSON or JSONL.
// It supports both:
// - JSON format (single document, used by OpenCode and Gemini CLI)
// - JSONL format (one JSON object per line, used by Claude Code)
func (env *TestEnv) validateTranscriptJSONL(checkpointID string, expectedContent []string) {
	env.T.Helper()

	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		env.T.Fatalf("Transcript not found at %s", transcriptPath)
	}

	// First try to parse as a single JSON document (OpenCode/Gemini format)
	var jsonDoc any
	if err := json.Unmarshal([]byte(content), &jsonDoc); err == nil {
		// Valid JSON document - validation passed
	} else {
		// Fall back to JSONL validation (Claude Code format)
		lines := strings.Split(content, "\n")
		validLines := 0
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			validLines++
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				env.T.Errorf("Transcript line %d is not valid JSON: %v\nLine: %s", i+1, err, line)
			}
		}

		if validLines == 0 {
			env.T.Error("Transcript is empty (no valid JSON content)")
		}
	}

	// Validate expected content appears in transcript
	for _, expected := range expectedContent {
		if !strings.Contains(content, expected) {
			env.T.Errorf("Transcript should contain %q", expected)
		}
	}
}

// validateContentHash validates that content_hash.txt matches the SHA256 of the transcript.
func (env *TestEnv) validateContentHash(checkpointID string) {
	env.T.Helper()

	// Read transcript
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	transcript, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		env.T.Fatalf("Transcript not found at %s", transcriptPath)
	}

	// Read content hash
	hashPath := SessionFilePath(checkpointID, "content_hash.txt")
	storedHash, found := env.ReadFileFromBranch(paths.MetadataBranchName, hashPath)
	if !found {
		env.T.Fatalf("Content hash not found at %s", hashPath)
	}
	storedHash = strings.TrimSpace(storedHash)

	// Calculate expected hash with sha256: prefix (matches format in committed.go)
	hash := sha256.Sum256([]byte(transcript))
	expectedHash := "sha256:" + hex.EncodeToString(hash[:])

	if storedHash != expectedHash {
		env.T.Errorf("Content hash mismatch:\n  stored:   %s\n  expected: %s", storedHash, expectedHash)
	}
}

// validatePromptContent validates that prompt.txt contains the expected prompts.
func (env *TestEnv) validatePromptContent(checkpointID string, expectedPrompts []string) {
	env.T.Helper()

	promptPath := SessionFilePath(checkpointID, paths.PromptFileName)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath)
	if !found {
		env.T.Fatalf("Prompt file not found at %s", promptPath)
	}

	for _, expected := range expectedPrompts {
		if !strings.Contains(content, expected) {
			env.T.Errorf("Prompt file should contain %q\nContent: %s", expected, content)
		}
	}
}

// SetupBareRemote creates a bare git repository, adds it as "origin" remote to the
// test repo, and pushes the current HEAD. Returns the bare repo path.
// This mirrors the E2E helper in e2e/testutil/repo.go but adapted for TestEnv.
func (env *TestEnv) SetupBareRemote() string {
	env.T.Helper()
	return env.SetupNamedBareRemote("origin")
}

// SetupNamedBareRemote creates a bare git repository with a custom remote name.
// Returns the bare repo path. Use this for checkpoint_remote scenarios that need
// multiple remotes.
func (env *TestEnv) SetupNamedBareRemote(remoteName string) string {
	env.T.Helper()

	ctx := env.T.Context()

	bareDir := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(bareDir); err == nil {
		bareDir = resolved
	}

	// Initialize bare repo
	cmd := exec.CommandContext(ctx, "git", "init", "--bare")
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to init bare repo: %v\n%s", err, output)
	}

	// Add as remote
	cmd = exec.CommandContext(ctx, "git", "remote", "add", remoteName, bareDir)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to add remote %s: %v\n%s", remoteName, err, output)
	}

	// Push HEAD to the remote
	cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "-u", remoteName, "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to push to %s: %v\n%s", remoteName, err, output)
	}

	return bareDir
}

// CloneFrom clones from a bare repo into a new temp directory and returns a new TestEnv
// pointing at the clone. The clone has its own .entire directory initialized.
// The clone checks out the same branch as the current env's HEAD.
func (env *TestEnv) CloneFrom(bareDir string) *TestEnv {
	env.T.Helper()

	ctx := env.T.Context()

	cloneDir := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(cloneDir); err == nil {
		cloneDir = resolved
	}

	// Get the current branch name to clone the right branch
	currentBranch := env.GetCurrentBranch()

	// Clone the bare repo, explicitly checking out the right branch.
	// Bare repos may have HEAD pointing to a non-existent default branch
	// when the original was on a feature branch.
	cloneArgs := []string{"clone"}
	if currentBranch != "" {
		cloneArgs = append(cloneArgs, "--branch", currentBranch)
	}
	cloneArgs = append(cloneArgs, bareDir, cloneDir)
	cmd := exec.CommandContext(ctx, "git", cloneArgs...)
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to clone from %s: %v\n%s", bareDir, err, output)
	}

	// Configure git user (clone doesn't inherit local config from the bare repo)
	for _, kv := range [][2]string{
		{"user.name", "Test User"},
		{"user.email", "test@example.com"},
		{"commit.gpgsign", "false"},
	} {
		cmd = exec.CommandContext(ctx, "git", "config", kv[0], kv[1])
		cmd.Dir = cloneDir
		cmd.Env = testutil.GitIsolatedEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			env.T.Fatalf("failed to set git config %s: %v\n%s", kv[0], err, output)
		}
	}

	claudeProjectDir := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(claudeProjectDir); err == nil {
		claudeProjectDir = resolved
	}
	geminiProjectDir := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(geminiProjectDir); err == nil {
		geminiProjectDir = resolved
	}
	openCodeProjectDir := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(openCodeProjectDir); err == nil {
		openCodeProjectDir = resolved
	}

	cloneEnv := &TestEnv{
		T:                  env.T,
		RepoDir:            cloneDir,
		ClaudeProjectDir:   claudeProjectDir,
		GeminiProjectDir:   geminiProjectDir,
		OpenCodeProjectDir: openCodeProjectDir,
	}

	// Initialize Entire in the clone
	cloneEnv.InitEntire()

	return cloneEnv
}

// BranchExistsOnRemote checks if a branch exists on a bare remote by inspecting its refs.
func (env *TestEnv) BranchExistsOnRemote(bareDir, branchName string) bool {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// PatchSettings merges extra keys into .entire/settings.json.
func (env *TestEnv) PatchSettings(extra map[string]any) {
	env.T.Helper()

	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // G304: path is constructed from test env, not user input
	if err != nil {
		env.T.Fatalf("failed to read settings: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		env.T.Fatalf("failed to parse settings: %v", err)
	}

	for k, v := range extra {
		settings[k] = v
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		env.T.Fatalf("failed to marshal settings: %v", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(settingsPath, out, 0o644); err != nil { //nolint:gosec // G306: consistent with other settings writes in testenv.go
		env.T.Fatalf("failed to write settings: %v", err)
	}
}

// GitPush pushes a branch to a remote. Fails the test on error.
func (env *TestEnv) GitPush(remote, refSpec string) {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "push", "--no-verify", remote, refSpec)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git push %s %s failed: %v\n%s", remote, refSpec, err, output)
	}
}

// RunPrePush runs the pre-push hook via the CLI binary, consistent with how
// other CLI invocations (GitCommitWithShadowHooks, RunCLI) use env.cliEnv().
func (env *TestEnv) RunPrePush(remote string) {
	env.T.Helper()
	if err := env.RunPrePushWithError(remote); err != nil {
		env.T.Fatalf("PrePush failed: %v", err)
	}
}

// RunPrePushWithError runs the pre-push hook and returns any error instead of failing.
func (env *TestEnv) RunPrePushWithError(remote string) error {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), getTestBinary(), "hooks", "git", "pre-push", remote)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	cmd.Stdin = nil

	output, err := cmd.CombinedOutput()
	env.T.Logf("pre-push output: %s", output)
	if err != nil {
		return fmt.Errorf("pre-push hook failed: %w", err)
	}
	return nil
}

// FetchMetadataBranch fetches the entire/checkpoints/v1 branch from a remote URL.
// Fails the test on error. Use this for clone-and-resume tests that need metadata.
func (env *TestEnv) FetchMetadataBranch(remoteURL string) {
	env.T.Helper()

	branchName := paths.MetadataBranchName
	refSpec := "+refs/heads/" + branchName + ":refs/heads/" + branchName
	cmd := exec.CommandContext(env.T.Context(), "git", "fetch", "--no-tags", "--filter=blob:none", remoteURL, refSpec)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("fetch metadata branch failed: %v\n%s", err, output)
	}
}

// GetBranchTipParentCount returns the number of parents for the tip commit of a branch.
func (env *TestEnv) GetBranchTipParentCount(branchName string) int {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		env.T.Fatalf("failed to get branch %s: %v", branchName, err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		env.T.Fatalf("failed to get commit for branch %s: %v", branchName, err)
	}

	return len(commit.ParentHashes)
}

func findModuleRoot() string {
	// Start from this source file's location and walk up to find go.mod
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("failed to get current file path via runtime.Caller")
	}
	dir := filepath.Dir(thisFile)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find go.mod starting from " + thisFile)
		}
		dir = parent
	}
}
