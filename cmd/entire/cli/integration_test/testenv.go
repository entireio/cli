//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"entire.io/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	T                *testing.T
	RepoDir          string
	ClaudeProjectDir string
	GeminiProjectDir string
	SessionCounter   int
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

	env := &TestEnv{
		T:                t,
		RepoDir:          repoDir,
		ClaudeProjectDir: claudeProjectDir,
		GeminiProjectDir: geminiProjectDir,
	}

	// Note: Don't use t.Setenv here - it's incompatible with t.Parallel()
	// CLI commands receive ENTIRE_TEST_CLAUDE_PROJECT_DIR or ENTIRE_TEST_GEMINI_PROJECT_DIR via cmd.Env instead

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

// NewRepoEnv creates a TestEnv with an initialized git repo and Entire.
// This is a convenience factory for tests that need a basic repo setup.
func NewRepoEnv(t *testing.T, strategy string) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire(strategy)
	return env
}

// NewRepoWithCommit creates a TestEnv with a git repo, Entire, and an initial commit.
// The initial commit contains a README.md file.
func NewRepoWithCommit(t *testing.T, strategy string) *TestEnv {
	t.Helper()
	env := NewRepoEnv(t, strategy)
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	return env
}

// NewFeatureBranchEnv creates a TestEnv ready for session testing.
// It initializes the repo, creates an initial commit on main,
// and checks out a feature branch. This is the most common setup
// for session and rewind tests since Entire tracking skips main/master.
func NewFeatureBranchEnv(t *testing.T, strategyName string) *TestEnv {
	t.Helper()
	env := NewRepoWithCommit(t, strategyName)
	env.GitCheckoutNewBranch("feature/test-branch")
	return env
}

// AllStrategies returns all strategy names for parameterized tests.
func AllStrategies() []string {
	return []string{
		strategy.StrategyNameAutoCommit,
		strategy.StrategyNameManualCommit,
	}
}

// RunForAllStrategies runs a test function for each strategy in parallel.
// This reduces boilerplate for tests that need to verify behavior across all strategies.
// Each subtest gets its own TestEnv with a feature branch ready for testing.
func RunForAllStrategies(t *testing.T, testFn func(t *testing.T, env *TestEnv, strategyName string)) {
	t.Helper()
	for _, strat := range AllStrategies() {
		strat := strat // capture for parallel
		t.Run(strat, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t, strat)
			testFn(t, env, strat)
		})
	}
}

// RunForAllStrategiesWithRepoEnv runs a test function for each strategy in parallel,
// using NewRepoWithCommit instead of NewFeatureBranchEnv. Use this for tests
// that need to test behavior on the main branch.
func RunForAllStrategiesWithRepoEnv(t *testing.T, testFn func(t *testing.T, env *TestEnv, strategyName string)) {
	t.Helper()
	for _, strat := range AllStrategies() {
		strat := strat // capture for parallel
		t.Run(strat, func(t *testing.T) {
			t.Parallel()
			env := NewRepoWithCommit(t, strat)
			testFn(t, env, strat)
		})
	}
}

// RunForAllStrategiesWithBasicEnv runs a test function for each strategy in parallel,
// using NewRepoEnv (git repo + entire init, no commits). Use this for tests
// that need to verify basic initialization behavior.
func RunForAllStrategiesWithBasicEnv(t *testing.T, testFn func(t *testing.T, env *TestEnv, strategyName string)) {
	t.Helper()
	for _, strat := range AllStrategies() {
		strat := strat // capture for parallel
		t.Run(strat, func(t *testing.T) {
			t.Parallel()
			env := NewRepoEnv(t, strat)
			testFn(t, env, strat)
		})
	}
}

// RunForStrategiesSequential runs a test function for specific strategies sequentially.
// Use this for tests that cannot be parallelized (e.g., tests using os.Chdir).
// The strategies parameter allows testing a subset of strategies.
func RunForStrategiesSequential(t *testing.T, strategies []string, testFn func(t *testing.T, strategyName string)) {
	t.Helper()
	for _, strat := range strategies {
		t.Run(strat, func(t *testing.T) {
			testFn(t, strat)
		})
	}
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
	if err := repo.SetConfig(cfg); err != nil {
		env.T.Fatalf("failed to set repo config: %v", err)
	}
}

// InitEntire initializes the .entire directory with the specified strategy.
func (env *TestEnv) InitEntire(strategyName string) {
	env.InitEntireWithOptions(strategyName, nil)
}

// InitEntireWithOptions initializes the .entire directory with the specified strategy and options.
func (env *TestEnv) InitEntireWithOptions(strategyName string, strategyOptions map[string]any) {
	env.T.Helper()
	env.initEntireInternal(strategyName, "", strategyOptions)
}

// InitEntireWithAgent initializes an Entire test environment with a specific agent.
// If agentName is empty, defaults to claude-code.
func (env *TestEnv) InitEntireWithAgent(strategyName, agentName string) {
	env.T.Helper()
	env.initEntireInternal(strategyName, agentName, nil)
}

// InitEntireWithAgentAndOptions initializes Entire with the specified strategy, agent, and options.
func (env *TestEnv) InitEntireWithAgentAndOptions(strategyName, agentName string, strategyOptions map[string]any) {
	env.T.Helper()
	env.initEntireInternal(strategyName, agentName, strategyOptions)
}

// initEntireInternal is the common implementation for InitEntire variants.
func (env *TestEnv) initEntireInternal(strategyName, agentName string, strategyOptions map[string]any) {
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
	settings := map[string]any{
		"strategy":  strategyName,
		"local_dev": true, // Use go run for hooks in tests
	}
	// Only add agent if specified (otherwise defaults to claude-code)
	if agentName != "" {
		settings["agent"] = agentName
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
// This simulates commits created by the auto-commit strategy.
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
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)

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
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)

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
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)

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
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)

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

	refs, err := repo.References()
	if err != nil {
		env.T.Fatalf("failed to get references: %v", err)
	}

	found := false
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().Short() == branchName {
			found = true
		}
		return nil
	})

	return found
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

// GitCommitWithShadowHooks stages and commits files, simulating the prepare-commit-msg and post-commit hooks.
// This is used for testing manual-commit strategy which needs:
// - prepare-commit-msg hook: adds the Entire-Checkpoint trailer
// - post-commit hook: condenses session data if trailer is present
func (env *TestEnv) GitCommitWithShadowHooks(message string, files ...string) {
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

	// Run prepare-commit-msg hook using the shared binary
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile)
	prepCmd.Dir = env.RepoDir
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
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", output)
		// Don't fail - hook may silently succeed
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

	// Run prepare-commit-msg hook using the shared binary
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile)
	prepCmd.Dir = env.RepoDir
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

// GetLatestCheckpointID returns the most recent checkpoint ID from the entire/sessions branch.
// This is used by tests that previously extracted the checkpoint ID from commit message trailers.
// Now that active branch commits are clean (no trailers), we get the ID from the sessions branch.
// Fatals if the checkpoint ID cannot be found, with detailed context about what was found.
func (env *TestEnv) GetLatestCheckpointID() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	// Get the entire/sessions branch
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

// TryGetLatestCheckpointID returns the most recent checkpoint ID from the entire/sessions branch.
// Returns empty string if the branch doesn't exist or has no checkpoint commits yet.
// Use this when you need to check if a checkpoint exists without failing the test.
func (env *TestEnv) TryGetLatestCheckpointID() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		return ""
	}

	// Get the entire/sessions branch
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
