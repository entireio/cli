//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// Uses the configured defaultAgent (from E2E_AGENT env var or "claude-code").
func (env *TestEnv) RunEntireEnable(strategyName string) {
	env.T.Helper()

	args := []string{
		"enable",
		"--agent", defaultAgent,
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
	prepCmd.Env = append(os.Environ(),
		"ENTIRE_TEST_TTY=1",
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+filepath.Join(env.RepoDir, ".claude"),
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+filepath.Join(env.RepoDir, ".gemini"),
	)
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
	postCmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+filepath.Join(env.RepoDir, ".claude"),
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+filepath.Join(env.RepoDir, ".gemini"),
	)
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
// Returns an error if no checkpoint trailer is found in any commit.
func (env *TestEnv) GetLatestCheckpointIDFromHistory() (string, error) {
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

	if checkpointID == "" {
		return "", errors.New("no commit with Entire-Checkpoint trailer found in history")
	}

	return checkpointID, nil
}

// safeIDPrefix returns first 12 chars of ID or the full ID if shorter.
// Use this when logging checkpoint IDs to avoid index out of bounds panic.
func safeIDPrefix(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
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

// GitStash runs git stash to save uncommitted changes.
func (env *TestEnv) GitStash() {
	env.T.Helper()

	//nolint:noctx // test code, no context needed for git stash
	cmd := exec.Command("git", "stash")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git stash failed: %v\nOutput: %s", err, output)
	}
}

// GitStashPop runs git stash pop to restore stashed changes.
func (env *TestEnv) GitStashPop() {
	env.T.Helper()

	//nolint:noctx // test code, no context needed for git stash pop
	cmd := exec.Command("git", "stash", "pop")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git stash pop failed: %v\nOutput: %s", err, output)
	}
}

// GitCheckoutFile reverts a file to its committed state.
func (env *TestEnv) GitCheckoutFile(path string) {
	env.T.Helper()

	//nolint:gosec,noctx // test code, path is from test setup, no context needed
	cmd := exec.Command("git", "checkout", "--", path)
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git checkout -- %s failed: %v\nOutput: %s", path, err, output)
	}
}

// DeleteFile removes a file from the test repo.
func (env *TestEnv) DeleteFile(path string) {
	env.T.Helper()

	fullPath := filepath.Join(env.RepoDir, path)
	if err := os.Remove(fullPath); err != nil {
		env.T.Fatalf("failed to delete file %s: %v", path, err)
	}
}

// GetCommitCount returns the number of commits on the current branch.
func (env *TestEnv) GetCommitCount() int {
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

	count := 0
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	commitIter.ForEach(func(c *object.Commit) error {
		count++
		return nil
	})

	return count
}

// GetAllCheckpointIDsFromHistory walks backwards from HEAD and returns
// all checkpoint IDs from commits with Entire-Checkpoint trailers.
func (env *TestEnv) GetAllCheckpointIDsFromHistory() []string {
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

	var checkpointIDs []string
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	commitIter.ForEach(func(c *object.Commit) error {
		for _, line := range strings.Split(c.Message, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Entire-Checkpoint:") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "Entire-Checkpoint:"))
				checkpointIDs = append(checkpointIDs, id)
			}
		}
		return nil
	})

	return checkpointIDs
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

// CheckpointValidation contains expected values for checkpoint validation.
type CheckpointValidation struct {
	// CheckpointID is the expected checkpoint ID
	CheckpointID string

	// Strategy is the expected strategy name (optional)
	Strategy string

	// FilesTouched are the expected files in files_touched (optional)
	FilesTouched []string

	// ExpectedPrompts are strings that should appear in prompt.txt (optional)
	ExpectedPrompts []string

	// ExpectedTranscriptContent are strings that should appear in full.jsonl (optional)
	ExpectedTranscriptContent []string
}

// ValidateCheckpoint performs comprehensive validation of a checkpoint on the metadata branch.
func (env *TestEnv) ValidateCheckpoint(v CheckpointValidation) {
	env.T.Helper()

	metadataBranch := "entire/checkpoints/v1"

	// Compute sharded path: <id[:2]>/<id[2:]>/
	if len(v.CheckpointID) < 3 {
		env.T.Fatalf("Checkpoint ID too short: %s", v.CheckpointID)
	}
	shardedPath := v.CheckpointID[:2] + "/" + v.CheckpointID[2:]

	// Validate root metadata.json exists and has expected fields
	summaryPath := shardedPath + "/metadata.json"
	summaryContent, found := env.ReadFileFromBranch(metadataBranch, summaryPath)
	if !found {
		env.T.Errorf("CheckpointSummary not found at %s", summaryPath)
	} else {
		var summary map[string]any
		if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
			env.T.Errorf("Failed to parse CheckpointSummary: %v", err)
		} else {
			// Validate checkpoint_id
			if cpID, ok := summary["checkpoint_id"].(string); !ok || cpID != v.CheckpointID {
				env.T.Errorf("CheckpointSummary.checkpoint_id = %v, want %s", summary["checkpoint_id"], v.CheckpointID)
			}
			// Validate strategy if specified
			if v.Strategy != "" {
				if strategy, ok := summary["strategy"].(string); !ok || strategy != v.Strategy {
					env.T.Errorf("CheckpointSummary.strategy = %v, want %s", summary["strategy"], v.Strategy)
				}
			}
			// Validate sessions array exists
			if sessions, ok := summary["sessions"].([]any); !ok || len(sessions) == 0 {
				env.T.Error("CheckpointSummary.sessions should have at least one entry")
			}
			// Validate files_touched if specified
			if len(v.FilesTouched) > 0 {
				if filesTouched, ok := summary["files_touched"].([]any); ok {
					touchedSet := make(map[string]bool)
					for _, f := range filesTouched {
						if s, ok := f.(string); ok {
							touchedSet[s] = true
						}
					}
					for _, expected := range v.FilesTouched {
						if !touchedSet[expected] {
							env.T.Errorf("CheckpointSummary.files_touched missing %q", expected)
						}
					}
				}
			}
		}
	}

	// Validate session metadata.json exists
	sessionMetadataPath := shardedPath + "/0/metadata.json"
	sessionContent, found := env.ReadFileFromBranch(metadataBranch, sessionMetadataPath)
	if !found {
		env.T.Errorf("Session metadata not found at %s", sessionMetadataPath)
	} else {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(sessionContent), &metadata); err != nil {
			env.T.Errorf("Failed to parse session metadata: %v", err)
		} else {
			// Validate checkpoint_id matches
			if cpID, ok := metadata["checkpoint_id"].(string); !ok || cpID != v.CheckpointID {
				env.T.Errorf("Session metadata.checkpoint_id = %v, want %s", metadata["checkpoint_id"], v.CheckpointID)
			}
			// Validate created_at exists
			if _, ok := metadata["created_at"].(string); !ok {
				env.T.Error("Session metadata.created_at should exist")
			}
		}
	}

	// Validate transcript is valid JSONL
	transcriptPath := shardedPath + "/0/full.jsonl"
	transcriptContent, found := env.ReadFileFromBranch(metadataBranch, transcriptPath)
	if !found {
		env.T.Errorf("Transcript not found at %s", transcriptPath)
	} else {
		// Check each line is valid JSON
		lines := strings.Split(transcriptContent, "\n")
		validLines := 0
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			validLines++
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				env.T.Errorf("Transcript line %d is not valid JSON: %v", i+1, err)
			}
		}
		if validLines == 0 {
			env.T.Error("Transcript is empty (no valid JSONL lines)")
		}
		// Check expected content
		for _, expected := range v.ExpectedTranscriptContent {
			if !strings.Contains(transcriptContent, expected) {
				env.T.Errorf("Transcript should contain %q", expected)
			}
		}
	}

	// Validate prompt.txt contains expected prompts
	if len(v.ExpectedPrompts) > 0 {
		promptPath := shardedPath + "/0/prompt.txt"
		promptContent, found := env.ReadFileFromBranch(metadataBranch, promptPath)
		if !found {
			env.T.Errorf("Prompt file not found at %s", promptPath)
		} else {
			for _, expected := range v.ExpectedPrompts {
				if !strings.Contains(promptContent, expected) {
					env.T.Errorf("Prompt file should contain %q", expected)
				}
			}
		}
	}

	// Validate content hash exists and matches transcript
	hashPath := shardedPath + "/0/content_hash.txt"
	hashContent, found := env.ReadFileFromBranch(metadataBranch, hashPath)
	if !found {
		env.T.Errorf("Content hash not found at %s", hashPath)
	} else if transcriptContent != "" {
		// Verify hash matches
		hash := sha256.Sum256([]byte(transcriptContent))
		expectedHash := "sha256:" + hex.EncodeToString(hash[:])
		storedHash := strings.TrimSpace(hashContent)
		if storedHash != expectedHash {
			env.T.Errorf("Content hash mismatch:\n  stored:   %s\n  expected: %s", storedHash, expectedHash)
		}
	}
}
