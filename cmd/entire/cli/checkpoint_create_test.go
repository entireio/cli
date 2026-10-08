package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// runCheckpointCreate executes `checkpoint create` with args and returns its
// stdout and error.
func runCheckpointCreate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newCheckpointCreateCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

// saveIdleSession records a session state with no checkpointable content.
func saveIdleSession(t *testing.T, sessionID, worktree string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	if err := strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        worktree,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}); err != nil {
		t.Fatalf("SaveSessionState(%q): %v", sessionID, err)
	}
}

// With no argument the caller's own session is checkpointed, and --json
// carries the ID plus the provenance that picked the session.
func TestCheckpointCreate_CallerSessionJSON(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "test.txt", "initial")
	testutil.GitAdd(t, dir, "test.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	sessionID := "2026-09-25-create-caller"
	saveNoFileChangesSession(t, dir, sessionID,
		`{"type":"human","message":{"content":"make a checkpoint"}}`+"\n"+
			`{"type":"assistant","message":{"content":"done"}}`+"\n")
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessionID)

	stdout, err := runCheckpointCreate(t, "--json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got checkpointCreateJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	if got.SessionID != sessionID {
		t.Errorf("session_id = %q, want %q", got.SessionID, sessionID)
	}
	if got.Resolution != string(strategy.ResolutionCallerEnv) {
		t.Errorf("resolution = %q, want %q", got.Resolution, strategy.ResolutionCallerEnv)
	}

	checkpointID, err := id.NewCheckpointID(got.CheckpointID)
	if err != nil {
		t.Fatalf("checkpoint_id %q: %v", got.CheckpointID, err)
	}
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("read checkpoint %s: %v", checkpointID, err)
	}
	if !strings.Contains(string(content.Transcript), "make a checkpoint") {
		t.Errorf("checkpoint transcript missing the session's prompt: %q", content.Transcript)
	}
}

// Without an argument, only a resolution that identified the caller is acted
// on. The most-recent-session tiers can name someone else's work, so they are
// refused rather than checkpointed.
func TestCheckpointCreate_RefusesUnidentifiedCaller(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)
	saveIdleSession(t, "most-recent-here", dir)

	_, err := runCheckpointCreate(t)
	if err == nil {
		t.Fatal("expected an error for a worktree-tier guess, got nil")
	}
	if !strings.Contains(err.Error(), "session's ID") || !strings.Contains(err.Error(), string(strategy.ResolutionWorktree)) {
		t.Errorf("error = %q, want it to name the %q resolution and ask for a session ID", err, strategy.ResolutionWorktree)
	}
	// The guess must not be handed out: an agent would pass it straight back.
	if strings.Contains(err.Error(), "most-recent-here") {
		t.Errorf("error = %q names the guessed session", err)
	}
}

func TestCheckpointCreate_UntrackedCallerIsNotSubstituted(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)
	saveIdleSession(t, "unrelated-session", dir)
	t.Setenv("PI_SESSION_ID", "no-state-for-me")

	_, err := runCheckpointCreate(t)
	if err == nil {
		t.Fatal("expected an error for an untracked caller, got nil")
	}
	if !strings.Contains(err.Error(), "no-state-for-me") || strings.Contains(err.Error(), "unrelated-session") {
		t.Errorf("error = %q, want it to name the untracked caller and not the decoy", err)
	}
}

func TestCheckpointCreate_ExplicitSessionErrors(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)
	saveIdleSession(t, "idle-no-content", dir)
	if err := os.MkdirAll(filepath.Join(dir, paths.SessionMetadataDirFromSessionID("idle-no-content")), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, arg, want string
	}{
		{"invalid ID", "../escape", "invalid session ID"},
		{"unknown session", "does-not-exist", "session not found"},
		{"nothing to checkpoint", "idle-no-content", "no transcript or files to checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel: the parent changed process CWD.
			_, err := runCheckpointCreate(t, tc.arg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// saveNoFileChangesSession records a session that changed no files: no steps,
// no shadow branch, only a live transcript — the session this command is for.
func saveNoFileChangesSession(t *testing.T, dir, sessionID, transcript string) {
	t.Helper()
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	transcriptPath := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID:           sessionID,
		BaseCommit:          head.Hash().String(),
		WorktreePath:        dir,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
		Phase:               session.PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
}

// A session with uncommitted file changes is refused with a message that says
// where its work goes, so an agent does not retry with another session's ID.
func TestCheckpointCreate_RefusesPendingFileChanges(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "test.txt", "initial")
	testutil.GitAdd(t, dir, "test.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	sessionID := "2026-10-03-create-pending-files"
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
	testutil.WriteFile(t, dir, filepath.Join(metadataDir, paths.TranscriptFileName),
		`{"type":"human","message":{"content":"edit test.txt"}}`+"\n")
	testutil.WriteFile(t, dir, "test.txt", "agent content")
	if err := GetStrategy(context.Background()).SaveStep(context.Background(), strategy.StepContext{
		SessionID:     sessionID,
		ModifiedFiles: []string{"test.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "Checkpoint 1",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
	}); err != nil {
		t.Fatalf("SaveStep: %v", err)
	}

	stdout, err := runCheckpointCreate(t, sessionID)
	if err == nil || !strings.Contains(err.Error(), "checkpointed when you commit") {
		t.Errorf("error = %v, want the uncommitted-file-changes refusal", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// Stdout is the checkpoint ID that callers capture, so a disabled repository
// must fail with nothing on stdout rather than print a notice and exit 0.
func TestCheckpointCreate_DisabledRepoFailsWithEmptyStdout(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)
	testutil.WriteFile(t, dir, EntireSettingsFile, `{"enabled": false}`)

	stdout, err := runCheckpointCreate(t, "--json")
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("err = %v (%T), want a SilentError so the command fails", err, err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: a caller would read the notice as an ID", stdout)
	}
}
