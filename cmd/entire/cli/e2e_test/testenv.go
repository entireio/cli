//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestEnv manages an isolated test environment for E2E tests with real agent calls.
type TestEnv struct {
	T       *testing.T
	RepoDir string
	Agent   AgentRunner
}

// NewTestEnv creates a new isolated E2E test environment.
func NewTestEnv(t *testing.T) *TestEnv {
	t.Helper()

	// Resolve symlinks on macOS where /var -> /private/var
	repoDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = resolved
	}

	// Create agent runner
	agent := NewAgentRunner(defaultAgent, AgentRunnerConfig{})

	return &TestEnv{
		T:       t,
		RepoDir: repoDir,
		Agent:   agent,
	}
}

// NewFeatureBranchEnv creates an E2E test environment ready for testing.
// It initializes the repo, creates an initial commit on main,
// checks out a feature branch, and sets up agent hooks.
func NewFeatureBranchEnv(t *testing.T, strategyName string) *TestEnv {
	t.Helper()

	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository\n\nThis is a test repository for E2E testing.\n")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/e2e-test")

	// Use `entire enable` to set up everything (hooks, settings, etc.)
	// This sets up .entire/settings.json and .claude/settings.json with hooks
	env.RunEntireEnable(strategyName)

	return env
}

// RunEntireEnable runs `entire enable` to set up the project with hooks.
func (env *TestEnv) RunEntireEnable(strategyName string) {
	env.T.Helper()

	args := []string{
		"enable",
		"--agent", "claude-code",
		"--strategy", strategyName,
		"--telemetry=false",
		"--force", // Force reinstall hooks in case they exist
	}

	//nolint:gosec,noctx // test code, args are static
	cmd := exec.Command(getTestBinary(), args...)
	cmd.Dir = env.RepoDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("entire enable failed: %v\nOutput: %s", err, output)
	}
	env.T.Logf("entire enable output: %s", output)
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
	cfg.User.Name = "E2E Test User"
	cfg.User.Email = "e2e-test@example.com"

	// Disable GPG signing for test commits
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")

	if err := repo.SetConfig(cfg); err != nil {
		env.T.Fatalf("failed to set repo config: %v", err)
	}
}

// InitEntire initializes the .entire directory with the specified strategy.
func (env *TestEnv) InitEntire(strategyName string) {
	env.T.Helper()

	// Create .entire directory structure
	entireDir := filepath.Join(env.RepoDir, ".entire")
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		env.T.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create tmp directory
	tmpDir := filepath.Join(entireDir, "tmp")
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		env.T.Fatalf("failed to create .entire/tmp directory: %v", err)
	}

	// Write settings.json
	settings := map[string]any{
		"strategy":  strategyName,
		"local_dev": true, // Use go run for hooks in tests
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		env.T.Fatalf("failed to marshal settings: %v", err)
	}
	data = append(data, '\n')
	settingsPath := filepath.Join(entireDir, "settings.json")
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		env.T.Fatalf("failed to write settings.json: %v", err)
	}
}

// WriteFile creates a file with the given content in the test repo.
func (env *TestEnv) WriteFile(path, content string) {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)

	// Create parent directories
	dir := filepath.Dir(fullPath)
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(dir, 0o755); err != nil {
		env.T.Fatalf("failed to create directory %s: %v", dir, err)
	}

	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		env.T.Fatalf("failed to write file %s: %v", path, err)
	}
}

// ReadFile reads a file from the test repo.
func (env *TestEnv) ReadFile(path string) string {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)
	//nolint:gosec // test code, path is from test setup
	data, err := os.ReadFile(fullPath)
	if err != nil {
		env.T.Fatalf("failed to read file %s: %v", path, err)
	}
	return string(data)
}

// TryReadFile reads a file from the test repo, returning empty string if not found.
func (env *TestEnv) TryReadFile(path string) string {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)
	//nolint:gosec // test code, path is from test setup
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
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
			Name:  "E2E Test User",
			Email: "e2e-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}
}

// GitCommitWithShadowHooks stages and commits files, running the prepare-commit-msg
// and post-commit hooks like a real workflow.
func (env *TestEnv) GitCommitWithShadowHooks(message string, files ...string) {
	env.T.Helper()

	// Stage files using go-git
	for _, file := range files {
		env.GitAdd(file)
	}

	// Create a temp file for the commit message
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		env.T.Fatalf("failed to write commit message file: %v", err)
	}

	// Run prepare-commit-msg hook
	//nolint:gosec,noctx // test code, args are from trusted test setup, no context needed
	prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile, "message")
	prepCmd.Dir = env.RepoDir
	prepCmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=1")
	if output, err := prepCmd.CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", output)
	}

	// Read the modified message
	//nolint:gosec // test code, path is from test setup
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
			Name:  "E2E Test User",
			Email: "e2e-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		env.T.Fatalf("failed to commit: %v", err)
	}

	// Run post-commit hook
	//nolint:gosec,noctx // test code, args are from trusted test setup, no context needed
	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	if output, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", output)
	}
}

// GitCheckoutNewBranch creates and checks out a new branch.
func (env *TestEnv) GitCheckoutNewBranch(branchName string) {
	env.T.Helper()

	//nolint:noctx // test code, no context needed for git checkout
	cmd := exec.Command("git", "checkout", "-b", branchName)
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("failed to checkout new branch %s: %v\nOutput: %s", branchName, err, output)
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

// RewindPoint mirrors the rewind --list JSON output.
type RewindPoint struct {
	ID               string    `json:"id"`
	Message          string    `json:"message"`
	MetadataDir      string    `json:"metadata_dir"`
	Date             time.Time `json:"date"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint"`
	ToolUseID        string    `json:"tool_use_id"`
	IsLogsOnly       bool      `json:"is_logs_only"`
	CondensationID   string    `json:"condensation_id"`
}

// GetRewindPoints returns available rewind points using the CLI.
func (env *TestEnv) GetRewindPoints() []RewindPoint {
	env.T.Helper()

	//nolint:gosec,noctx // test code, args are static, no context needed
	cmd := exec.Command(getTestBinary(), "rewind", "--list")
	cmd.Dir = env.RepoDir

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
		//nolint:errcheck // date parsing failure is acceptable, defaults to zero time
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

	//nolint:gosec,noctx // test code, commitID is from test setup, no context needed
	cmd := exec.Command(getTestBinary(), "rewind", "--to", commitID)
	cmd.Dir = env.RepoDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("rewind failed: " + string(output))
	}

	env.T.Logf("Rewind output: %s", output)
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
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	refs.ForEach(func(ref *plumbing.Reference) error {
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

// GetLatestCheckpointIDFromHistory walks backwards from HEAD and returns
// the checkpoint ID from the first commit with an Entire-Checkpoint trailer.
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
	//nolint:errcheck,gosec // ForEach callback returns error to stop iteration, not a real error
	commitIter.ForEach(func(c *object.Commit) error {
		// Look for Entire-Checkpoint trailer
		for _, line := range strings.Split(c.Message, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Entire-Checkpoint:") {
				checkpointID = strings.TrimSpace(strings.TrimPrefix(line, "Entire-Checkpoint:"))
				return errors.New("stop iteration")
			}
		}
		return nil
	})

	return checkpointID
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

	//nolint:gosec,noctx // test code, args are from test setup, no context needed
	cmd := exec.Command(getTestBinary(), args...)
	cmd.Dir = env.RepoDir

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunAgent runs the agent with the given prompt and returns the result.
func (env *TestEnv) RunAgent(prompt string) (*AgentResult, error) {
	env.T.Helper()
	//nolint:wrapcheck // test helper, caller handles error
	return env.Agent.RunPrompt(context.Background(), env.RepoDir, prompt)
}

// RunAgentWithTools runs the agent with specific tools enabled.
func (env *TestEnv) RunAgentWithTools(prompt string, tools []string) (*AgentResult, error) {
	env.T.Helper()
	//nolint:wrapcheck // test helper, caller handles error
	return env.Agent.RunPromptWithTools(context.Background(), env.RepoDir, prompt, tools)
}
