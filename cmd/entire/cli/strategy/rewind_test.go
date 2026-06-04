package strategy

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // Register agent for ResolveAgentForRewind tests
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"  // Register agent for ResolveAgentForRewind tests
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestShadowStrategy_PreviewRewind(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create checkpoint with app.js file
	appFile := filepath.Join(dir, "app.js")
	if err := os.WriteFile(appFile, []byte("console.log('hello');\n"), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}

	if _, err := worktree.Add("app.js"); err != nil {
		t.Fatalf("failed to add app.js: %v", err)
	}

	// Create metadata directory structure first
	sessionID := "test-session-123"
	metadataDir := filepath.Join(dir, entireDir, "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Create session state to track untracked files at start
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             sessionID,
		BaseCommit:            initialCommit.String(),
		StartedAt:             time.Now(),
		UntrackedFilesAtStart: []string{"existing-untracked.txt"},
		StepCount:             1,
		WorktreePath:          dir,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("failed to save session state: %v", err)
	}

	// Create checkpoint commit with session trailer
	checkpointMsg := "Checkpoint\n\nEntire-Session: " + sessionID
	checkpointHash, err := worktree.Commit(checkpointMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset to initial commit to simulate moving forward in time
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Create files that would be deleted on rewind:
	// 1. A new untracked file (created after checkpoint)
	extraFile := filepath.Join(dir, "extra.js")
	if err := os.WriteFile(extraFile, []byte("console.log('extra');\n"), 0o644); err != nil {
		t.Fatalf("failed to write extra.js: %v", err)
	}

	// 2. An untracked file that existed at session start (should NOT be deleted)
	existingFile := filepath.Join(dir, "existing-untracked.txt")
	if err := os.WriteFile(existingFile, []byte("I existed before session\n"), 0o644); err != nil {
		t.Fatalf("failed to write existing-untracked.txt: %v", err)
	}

	// Create rewind point
	point := RewindPoint{
		ID:          checkpointHash.String(),
		Message:     "Checkpoint",
		MetadataDir: metadataDir,
		Date:        time.Now(),
	}

	// Test PreviewRewind
	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	require.NotNil(t, preview, "PreviewRewind() returned nil preview")

	// Should restore app.js
	foundApp := false
	for _, f := range preview.FilesToRestore {
		if f == "app.js" {
			foundApp = true
			break
		}
	}
	if !foundApp {
		t.Errorf("FilesToRestore missing app.js, got: %v", preview.FilesToRestore)
	}

	// Should delete extra.js
	foundExtra := false
	for _, f := range preview.FilesToDelete {
		if f == "extra.js" {
			foundExtra = true
			break
		}
	}
	if !foundExtra {
		t.Errorf("FilesToDelete missing extra.js, got: %v", preview.FilesToDelete)
	}

	// Should NOT delete existing-untracked.txt (existed at session start)
	for _, f := range preview.FilesToDelete {
		if f == "existing-untracked.txt" {
			t.Errorf("FilesToDelete incorrectly includes existing-untracked.txt, got: %v", preview.FilesToDelete)
		}
	}
}

func TestShadowStrategy_PreviewRewind_LogsOnly(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Logs-only point should return empty preview
	point := RewindPoint{
		ID:           "abc123",
		Message:      "Committed",
		IsLogsOnly:   true,
		CheckpointID: "a1b2c3d4e5f6",
		Date:         time.Now(),
	}

	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	require.NotNil(t, preview, "PreviewRewind() returned nil preview")

	if len(preview.FilesToDelete) > 0 {
		t.Errorf("Logs-only preview should have no files to delete, got: %v", preview.FilesToDelete)
	}

	if len(preview.FilesToRestore) > 0 {
		t.Errorf("Logs-only preview should have no files to restore, got: %v", preview.FilesToRestore)
	}
}

func TestRestoreLogsOnly_UsesV11Transcript(t *testing.T) {
	repo := setupV1CustomRefRepo(t, `"1.1"`)
	repoRoot, err := os.Getwd()
	require.NoError(t, err)

	agentName := types.AgentName("restore-logs-v11-agent")
	agentType := types.AgentType("Restore Logs V11 Agent")
	sessionDir := filepath.Join(repoRoot, "restored-sessions")
	agent.Register(agentName, func() agent.Agent {
		return &restoreLogsOnlyAgent{
			name:       agentName,
			agentType:  agentType,
			sessionDir: sessionDir,
		}
	})

	ctx := context.Background()
	cpID := id.MustCheckpointID("abc987abc987")
	sessionID := "restore-v11-session"
	v11Transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"restore from v11"}]}}` + "\n")
	staleV1Transcript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"stale v1"}]}}` + "\n")

	writeCommittedRewindCheckpoint(t, repo, cpID, sessionID, agentType, v11Transcript, time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), v1Ref.Hash())))

	writeCommittedRewindCheckpoint(t, repo, cpID, sessionID, agentType, staleV1Transcript, time.Date(2025, 1, 3, 10, 0, 0, 0, time.UTC))

	var stdout, stderr bytes.Buffer
	restored, err := NewManualCommitStrategy().RestoreLogsOnly(ctx, &stdout, &stderr, RewindPoint{
		IsLogsOnly:   true,
		CheckpointID: cpID,
	}, true)
	require.NoError(t, err, "stderr: %s", stderr.String())
	require.Len(t, restored, 1, "stdout: %s\nstderr: %s", stdout.String(), stderr.String())

	restoredPath := filepath.Join(sessionDir, sessionID+".jsonl")
	got, err := os.ReadFile(restoredPath)
	require.NoError(t, err)
	require.Equal(t, string(v11Transcript), string(got))
}

func TestResolveAgentForRewind(t *testing.T) {
	t.Parallel()

	t.Run("empty type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("")
		if err == nil {
			t.Error("expected error for empty agent type")
		}
	})

	t.Run("Claude Code type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Claude Code")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameClaudeCode {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameClaudeCode)
		}
	})

	t.Run("Gemini CLI type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Gemini CLI")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameGemini {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameGemini)
		}
	})

	t.Run("unknown type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("Nonexistent Agent")
		if err == nil {
			t.Error("expected error for unknown agent type")
		}
	})

	t.Run("dynamically registered agent resolves by type", func(t *testing.T) {
		t.Parallel()

		// Simulate what external.DiscoverAndRegister does: register an agent at runtime.
		testName := types.AgentName("test-external-rewind-agent")
		testType := types.AgentType("Entire Test External Rewind Agent")
		agent.Register(testName, func() agent.Agent {
			return &fakeExternalAgent{name: testName, agentType: testType}
		})

		ag, err := ResolveAgentForRewind(testType)
		if err != nil {
			t.Fatalf("expected dynamically registered agent to resolve, got error: %v", err)
		}
		if ag.Type() != testType {
			t.Errorf("Type() = %q, want %q", ag.Type(), testType)
		}
		if ag.Name() != testName {
			t.Errorf("Name() = %q, want %q", ag.Name(), testName)
		}
	})
}

func TestPromptOverwriteNewerLogs_NonInteractiveRequiresForce(t *testing.T) {
	var errW bytes.Buffer
	_, err := PromptOverwriteNewerLogs(&errW, []SessionRestoreInfo{
		{
			SessionID:      "test-session",
			Status:         StatusLocalNewer,
			LocalTime:      time.Now(),
			CheckpointTime: time.Now().Add(-time.Minute),
			Prompt:         "test prompt",
		},
	})
	if err == nil {
		t.Fatal("expected non-interactive prompt error")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected error to mention --force, got %v", err)
	}
}

// TestShadowStrategy_Rewind_FromSubdirectory verifies that Rewind() writes files
// to the correct repo-root-relative locations when CWD is a subdirectory.
// This is a regression test for the bug where f.Name (repo-relative) was used
// directly with os.WriteFile, causing files to be written relative to CWD instead
// of the repo root.
func TestShadowStrategy_Rewind_FromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	author := &object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}

	// Create initial commit with README.md
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create files in nested directories for the checkpoint
	srcDir := filepath.Join(dir, "src")
	libDir := filepath.Join(dir, "src", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("failed to create lib dir: %v", err)
	}

	appContent := "const app = 'hello';\n"
	utilsContent := "export function utils() {}\n"

	if err := os.WriteFile(filepath.Join(srcDir, "app.js"), []byte(appContent), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "utils.js"), []byte(utilsContent), 0o644); err != nil {
		t.Fatalf("failed to write utils.js: %v", err)
	}

	if _, err := worktree.Add("src/app.js"); err != nil {
		t.Fatalf("failed to add src/app.js: %v", err)
	}
	if _, err := worktree.Add("src/lib/utils.js"); err != nil {
		t.Fatalf("failed to add src/lib/utils.js: %v", err)
	}

	// Create checkpoint commit
	checkpointHash, err := worktree.Commit("Checkpoint with nested files", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset back to initial commit so the nested files are gone
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Verify src directory is gone after reset
	if _, err := os.Stat(filepath.Join(dir, "src")); !os.IsNotExist(err) {
		t.Fatalf("expected src/ to not exist after reset, but it does")
	}

	// Create a subdirectory and chdir into it (simulating agent running from subdirectory)
	frontendDir := filepath.Join(dir, "frontend")
	if err := os.MkdirAll(frontendDir, 0o755); err != nil {
		t.Fatalf("failed to create frontend dir: %v", err)
	}
	t.Chdir(frontendDir)
	paths.ClearWorktreeRootCache()

	// Call Rewind from the subdirectory
	s := NewManualCommitStrategy()
	point := RewindPoint{
		ID:      checkpointHash.String(),
		Message: "Checkpoint with nested files",
		Date:    time.Now(),
	}

	if err := s.Rewind(context.Background(), io.Discard, io.Discard, point); err != nil {
		t.Fatalf("Rewind() error = %v", err)
	}

	// Verify files are restored at the REPO ROOT (not relative to CWD)
	restoredApp := filepath.Join(dir, "src", "app.js")
	content, err := os.ReadFile(restoredApp)
	if err != nil {
		t.Fatalf("expected src/app.js to exist at repo root, but got error: %v", err)
	}
	if string(content) != appContent {
		t.Errorf("src/app.js content = %q, want %q", string(content), appContent)
	}

	restoredUtils := filepath.Join(dir, "src", "lib", "utils.js")
	content, err = os.ReadFile(restoredUtils)
	if err != nil {
		t.Fatalf("expected src/lib/utils.js to exist at repo root, but got error: %v", err)
	}
	if string(content) != utilsContent {
		t.Errorf("src/lib/utils.js content = %q, want %q", string(content), utilsContent)
	}

	// Verify README.md is also restored at repo root
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to exist at repo root, but got error: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md content = %q, want %q", string(content), "# Test\n")
	}

	// Verify files are NOT written at CWD-relative paths (the bug behavior)
	wrongApp := filepath.Join(frontendDir, "src", "app.js")
	if _, err := os.Stat(wrongApp); !os.IsNotExist(err) {
		t.Errorf("src/app.js should NOT exist under frontend/ (CWD-relative), but it does at %s", wrongApp)
	}

	wrongUtils := filepath.Join(frontendDir, "src", "lib", "utils.js")
	if _, err := os.Stat(wrongUtils); !os.IsNotExist(err) {
		t.Errorf("src/lib/utils.js should NOT exist under frontend/ (CWD-relative), but it does at %s", wrongUtils)
	}
}

// TestShadowStrategy_Rewind_FromRepoRoot verifies the normal case where Rewind()
// restores files correctly when CWD is the repo root. This ensures the subdirectory
// fix did not break the happy path.
func TestShadowStrategy_Rewind_FromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	author := &object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}

	// Create initial commit with README.md
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create files in nested directories for the checkpoint
	srcDir := filepath.Join(dir, "src")
	libDir := filepath.Join(dir, "src", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("failed to create lib dir: %v", err)
	}

	appContent := "const app = 'hello';\n"
	utilsContent := "export function utils() {}\n"

	if err := os.WriteFile(filepath.Join(srcDir, "app.js"), []byte(appContent), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "utils.js"), []byte(utilsContent), 0o644); err != nil {
		t.Fatalf("failed to write utils.js: %v", err)
	}

	if _, err := worktree.Add("src/app.js"); err != nil {
		t.Fatalf("failed to add src/app.js: %v", err)
	}
	if _, err := worktree.Add("src/lib/utils.js"); err != nil {
		t.Fatalf("failed to add src/lib/utils.js: %v", err)
	}

	// Create checkpoint commit
	checkpointHash, err := worktree.Commit("Checkpoint with nested files", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset back to initial commit so the nested files are gone
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Verify src directory is gone after reset
	if _, err := os.Stat(filepath.Join(dir, "src")); !os.IsNotExist(err) {
		t.Fatalf("expected src/ to not exist after reset, but it does")
	}

	// Call Rewind from the repo root
	s := NewManualCommitStrategy()
	point := RewindPoint{
		ID:      checkpointHash.String(),
		Message: "Checkpoint with nested files",
		Date:    time.Now(),
	}

	if err := s.Rewind(context.Background(), io.Discard, io.Discard, point); err != nil {
		t.Fatalf("Rewind() error = %v", err)
	}

	// Verify files are restored correctly at repo root
	restoredApp := filepath.Join(dir, "src", "app.js")
	content, err := os.ReadFile(restoredApp)
	if err != nil {
		t.Fatalf("expected src/app.js to exist, but got error: %v", err)
	}
	if string(content) != appContent {
		t.Errorf("src/app.js content = %q, want %q", string(content), appContent)
	}

	restoredUtils := filepath.Join(dir, "src", "lib", "utils.js")
	content, err = os.ReadFile(restoredUtils)
	if err != nil {
		t.Fatalf("expected src/lib/utils.js to exist, but got error: %v", err)
	}
	if string(content) != utilsContent {
		t.Errorf("src/lib/utils.js content = %q, want %q", string(content), utilsContent)
	}

	// Verify README.md is also restored
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to exist, but got error: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md content = %q, want %q", string(content), "# Test\n")
	}
}

func writeCommittedRewindCheckpoint(
	t *testing.T,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	agentType types.AgentType,
	transcript []byte,
	createdAt time.Time,
) {
	t.Helper()

	err := cpkg.NewGitStore(repo).WriteCommitted(context.Background(), cpkg.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		CreatedAt:    createdAt,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      []string{"restore prompt"},
		Agent:        agentType,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})
	require.NoError(t, err)
}

type restoreLogsOnlyAgent struct {
	name       types.AgentName
	agentType  types.AgentType
	sessionDir string
}

var _ agent.Agent = (*restoreLogsOnlyAgent)(nil)

func (a *restoreLogsOnlyAgent) Name() types.AgentName                          { return a.name }
func (a *restoreLogsOnlyAgent) Type() types.AgentType                          { return a.agentType }
func (a *restoreLogsOnlyAgent) Description() string                            { return "restore logs test agent" }
func (a *restoreLogsOnlyAgent) IsPreview() bool                                { return false }
func (a *restoreLogsOnlyAgent) DetectPresence(_ context.Context) (bool, error) { return true, nil }
func (a *restoreLogsOnlyAgent) ProtectedDirs() []string                        { return nil }
func (a *restoreLogsOnlyAgent) ReadTranscript(string) ([]byte, error)          { return nil, nil }
func (a *restoreLogsOnlyAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}
func (a *restoreLogsOnlyAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out, nil
}
func (a *restoreLogsOnlyAgent) GetSessionID(*agent.HookInput) string { return "" }
func (a *restoreLogsOnlyAgent) GetSessionDir(string) (string, error) { return a.sessionDir, nil }
func (a *restoreLogsOnlyAgent) ResolveSessionFile(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".jsonl")
}
func (a *restoreLogsOnlyAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // Not used by this test agent.
}
func (a *restoreLogsOnlyAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return err
	}
	return os.WriteFile(session.SessionRef, session.NativeData, 0o600)
}
func (a *restoreLogsOnlyAgent) FormatResumeCommand(sessionID string) string {
	return "restore-logs " + sessionID
}

// fakeExternalAgent is a minimal Agent implementation for testing dynamic registration.
// It simulates an external agent that was discovered and registered at runtime.
type fakeExternalAgent struct {
	name      types.AgentName
	agentType types.AgentType
}

func (f *fakeExternalAgent) Name() types.AgentName                          { return f.name }
func (f *fakeExternalAgent) Type() types.AgentType                          { return f.agentType }
func (f *fakeExternalAgent) Description() string                            { return "Fake external agent" }
func (f *fakeExternalAgent) IsPreview() bool                                { return false }
func (f *fakeExternalAgent) DetectPresence(_ context.Context) (bool, error) { return false, nil }
func (f *fakeExternalAgent) ProtectedDirs() []string                        { return nil }
func (f *fakeExternalAgent) ReadTranscript(_ string) ([]byte, error)        { return nil, nil }
func (f *fakeExternalAgent) ChunkTranscript(_ context.Context, _ []byte, _ int) ([][]byte, error) {
	return nil, nil
}
func (f *fakeExternalAgent) ReassembleTranscript(_ [][]byte) ([]byte, error) { return nil, nil }
func (f *fakeExternalAgent) GetSessionID(_ *agent.HookInput) string          { return "" }
func (f *fakeExternalAgent) GetSessionDir(_ string) (string, error)          { return "", nil }
func (f *fakeExternalAgent) ResolveSessionFile(_, _ string) string           { return "" }
func (f *fakeExternalAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // test stub
}
func (f *fakeExternalAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error { return nil }
func (f *fakeExternalAgent) FormatResumeCommand(_ string) string                         { return "" }
