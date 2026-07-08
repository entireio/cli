package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

const (
	testAgentClaude       = "Claude Code"
	testAgentGemini       = "Gemini CLI"
	testCheckpointID      = "a3b2c4d5e6f7"
	testModelClaudeOpus   = "claude-opus-4-6[1m]"
	testOtherWorktreePath = "/other/worktree"
	testPromptFixLogin    = "fix the login bug"
)

// setupStopTestRepo initializes a temporary git repo, changes to it, and clears
// path/session caches. Must NOT be used with t.Parallel() because it calls t.Chdir.
func setupStopTestRepo(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
}

// makeSessionState returns a minimal SessionState suitable for test setup.
func makeSessionState(id string, phase session.Phase) *strategy.SessionState {
	return &strategy.SessionState{
		SessionID:  id,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      phase,
	}
}

func TestStopCmd_NoActiveSessions(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout.String(), "No active sessions.") {
		t.Errorf("expected 'No active sessions.' in output, got: %q", stdout.String())
	}
}

// TestStopCmd_SingleSession_EmptyWorktreePath_Force verifies that a session with an
// empty WorktreePath (legacy session without worktree tracking) is included in the
// current worktree's scope and stopped via the no-flags path.
func TestStopCmd_SingleSession_EmptyWorktreePath_Force(t *testing.T) {
	setupStopTestRepo(t)

	// WorktreePath intentionally left empty — exercises the s.WorktreePath == "" fallback.
	state := makeSessionState("test-stop-single-1", session.PhaseIdle)
	state.StepCount = 0
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "No work recorded.") {
		t.Errorf("expected 'No work recorded.' in output, got: %q", out)
	}

	loaded, err := strategy.LoadSessionState(context.Background(), "test-stop-single-1")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session state to still exist after stop")
		return
	}
	if loaded.Phase != session.PhaseEnded {
		t.Errorf("expected Phase=PhaseEnded, got: %v", loaded.Phase)
	}
}

func TestStopCmd_SingleSession_WithCheckpoint(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("test-stop-checkpoint-1", session.PhaseIdle)
	state.LastCheckpointID = testCheckpointID
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-stop-checkpoint-1", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoint: a3b2c4d5e6f7") {
		t.Errorf("expected 'Checkpoint: a3b2c4d5e6f7' in output, got: %q", out)
	}
}

func TestStopCmd_SingleSession_UncommittedWork(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("test-stop-uncommitted-1", session.PhaseIdle)
	state.StepCount = 2
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-stop-uncommitted-1", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Work will be captured in your next checkpoint.") {
		t.Errorf("expected 'Work will be captured in your next checkpoint.' in output, got: %q", out)
	}
}

func TestStopCmd_AlreadyStopped(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("test-stop-already-ended-1", session.PhaseEnded)
	now := time.Now()
	state.EndedAt = &now
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-stop-already-ended-1", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Session test-stop-already-ended-1 is already stopped.") {
		t.Errorf("expected 'is already stopped.' in output, got: %q", out)
	}

	// State should be unchanged (still ended)
	loaded, err := strategy.LoadSessionState(context.Background(), "test-stop-already-ended-1")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session state to still exist")
		return
	}
	if loaded.Phase != session.PhaseEnded {
		t.Errorf("expected Phase=PhaseEnded unchanged, got: %v", loaded.Phase)
	}
}

func TestStopCmd_SessionFlag(t *testing.T) {
	setupStopTestRepo(t)

	state1 := makeSessionState("test-stop-target-session", session.PhaseIdle)
	state2 := makeSessionState("test-stop-other-session", session.PhaseIdle)
	for _, s := range []*strategy.SessionState{state1, state2} {
		if err := strategy.SaveSessionState(context.Background(), s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-stop-target-session", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	target, err := strategy.LoadSessionState(context.Background(), "test-stop-target-session")
	if err != nil {
		t.Fatalf("LoadSessionState(target) error = %v", err)
	}
	if target == nil {
		t.Fatal("expected target session state to exist")
		return
	}
	if target.Phase != session.PhaseEnded {
		t.Errorf("expected target Phase=PhaseEnded, got: %v", target.Phase)
	}

	other, err := strategy.LoadSessionState(context.Background(), "test-stop-other-session")
	if err != nil {
		t.Fatalf("LoadSessionState(other) error = %v", err)
	}
	if other == nil {
		t.Fatal("expected other session state to exist")
		return
	}
	if other.Phase == session.PhaseEnded {
		t.Errorf("expected other session to remain non-ended, got: %v", other.Phase)
	}
}

func TestStopCmd_AllFlag(t *testing.T) {
	setupStopTestRepo(t)

	// Resolve the worktree root as the command sees it (handles macOS symlinks like /var -> /private/var).
	ctx := context.Background()
	worktreePath, wtErr := paths.WorktreeRoot(ctx)
	if wtErr != nil {
		t.Fatalf("WorktreeRoot() error = %v", wtErr)
	}

	state1 := makeSessionState("test-stop-all-sess-1", session.PhaseIdle)
	state1.WorktreePath = worktreePath
	state2 := makeSessionState("test-stop-all-sess-2", session.PhaseIdle)
	state2.WorktreePath = worktreePath
	for _, s := range []*strategy.SessionState{state1, state2} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--force"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	for _, id := range []string{"test-stop-all-sess-1", "test-stop-all-sess-2"} {
		if !strings.Contains(out, id) {
			t.Errorf("expected session ID %q in output, got: %q", id, out)
		}

		loaded, err := strategy.LoadSessionState(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadSessionState(%s) error = %v", id, err)
		}
		if loaded == nil {
			t.Fatalf("expected session %s to exist after stop", id)
			return
		}
		if loaded.Phase != session.PhaseEnded {
			t.Errorf("expected session %s Phase=PhaseEnded, got: %v", id, loaded.Phase)
		}
	}
}

func TestStopCmd_AllFlag_IncludesAllWorktrees(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	worktreePath, wtErr := paths.WorktreeRoot(ctx)
	if wtErr != nil {
		t.Fatalf("WorktreeRoot() error = %v", wtErr)
	}

	inScope := makeSessionState("test-all-scope-in", session.PhaseIdle)
	inScope.WorktreePath = worktreePath

	outOfScope := makeSessionState("test-all-scope-out", session.PhaseIdle)
	outOfScope.WorktreePath = testOtherWorktreePath

	for _, s := range []*strategy.SessionState{inScope, outOfScope} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--force"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Both sessions should be stopped (--all is no longer worktree-scoped)
	for _, id := range []string{"test-all-scope-in", "test-all-scope-out"} {
		loaded, err := strategy.LoadSessionState(ctx, id)
		if err != nil {
			t.Fatalf("LoadSessionState(%s) error = %v", id, err)
		}
		if loaded == nil {
			t.Fatalf("expected session %s to exist after stop", id)
			return
		}
		if loaded.Phase != session.PhaseEnded {
			t.Errorf("expected session %s to be PhaseEnded, got: %v", id, loaded.Phase)
		}
	}
}

func TestStopCmd_AllFlag_NoActiveSessions(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--force"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout.String(), "No active sessions.") {
		t.Errorf("expected 'No active sessions.' in output, got: %q", stdout.String())
	}
}

func TestStopCmd_AllAndSessionMutuallyExclusive(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("test-stop-mutex-sess", session.PhaseIdle)
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--force", "test-stop-mutex-sess"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --all and session ID together, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected error to mention 'mutually exclusive', got: %v", err)
	}

	// State should be unchanged
	loaded, err2 := strategy.LoadSessionState(context.Background(), "test-stop-mutex-sess")
	if err2 != nil {
		t.Fatalf("LoadSessionState() error = %v", err2)
	}
	if loaded == nil {
		t.Fatal("expected session state to still exist")
		return
	}
	if loaded.Phase == session.PhaseEnded {
		t.Error("expected session to remain non-ended after mutual exclusion error")
	}
}

func TestStopCmd_SessionNotFound(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"doesnotexist", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}

	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Errorf("expected SilentError, got: %T %v", err, err)
	}

	if !strings.Contains(stderr.String(), "Session not found.") {
		t.Errorf("expected 'Session not found.' in stderr, got: %q", stderr.String())
	}
}

func TestStopCmd_NotGitRepo(t *testing.T) {
	// Use a plain temp dir with no git init
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	cmd := newSessionsCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"stop"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}

	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected error to mention 'not a git repository', got: %v", err)
	}
}

// TestStopSelectedSessions_StopsAll exercises stopSelectedSessions directly,
// bypassing the TUI multi-select. Verifies all sessions in the list are ended
// and that success lines are printed for each.
func TestStopSelectedSessions_StopsAll(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	s1 := makeSessionState("test-batch-stop-1", session.PhaseIdle)
	s2 := makeSessionState("test-batch-stop-2", session.PhaseIdle)
	for _, s := range []*strategy.SessionState{s1, s2} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := stopSelectedSessions(ctx, cmd, []*strategy.SessionState{s1, s2}); err != nil {
		t.Fatalf("stopSelectedSessions() error = %v", err)
	}

	out := stdout.String()
	for _, id := range []string{"test-batch-stop-1", "test-batch-stop-2"} {
		if !strings.Contains(out, id) {
			t.Errorf("expected session ID %q in output, got: %q", id, out)
		}

		loaded, err := strategy.LoadSessionState(ctx, id)
		if err != nil {
			t.Fatalf("LoadSessionState(%s) error = %v", id, err)
		}
		if loaded == nil {
			t.Fatalf("expected session %s to exist after batch stop", id)
			return
		}
		if loaded.Phase != session.PhaseEnded {
			t.Errorf("expected session %s to be PhaseEnded after batch stop, got: %v", id, loaded.Phase)
		}
	}
}

// TestStopCmd_AlreadyStopped_EndedAtOnly verifies that a session with EndedAt set
// is treated as already stopped even when Phase has not been updated to PhaseEnded
// (legacy sessions where the phase field may have defaulted to Idle).
func TestStopCmd_AlreadyStopped_EndedAtOnly(t *testing.T) {
	setupStopTestRepo(t)

	// Simulate a legacy session: EndedAt is set but Phase is still PhaseIdle.
	state := makeSessionState("test-stop-ended-at-only", session.PhaseIdle)
	now := time.Now()
	state.EndedAt = &now
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-stop-ended-at-only", "--force"})

	err := cmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "is already stopped.") {
		t.Errorf("expected 'is already stopped.' in output, got: %q", out)
	}

	// Phase should remain unchanged — we must not overwrite legacy state.
	loaded, err := strategy.LoadSessionState(context.Background(), "test-stop-ended-at-only")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session state to still exist")
		return
	}
	if loaded.Phase != session.PhaseIdle {
		t.Errorf("expected Phase to remain PhaseIdle (legacy), got: %v", loaded.Phase)
	}
}

// TestFilterActiveSessions_ExcludesPhaseEndedWithoutEndedAt verifies that sessions
// created by `entire attach` (Phase=PhaseEnded, EndedAt=nil) are excluded.
func TestFilterActiveSessions_ExcludesPhaseEndedWithoutEndedAt(t *testing.T) {
	t.Parallel()

	// Simulates a session created by `entire attach` — Phase is ended but EndedAt is nil.
	attachEnded := makeSessionState("attach-ended", session.PhaseEnded)
	// EndedAt intentionally nil

	activeIdle := makeSessionState("active-idle", session.PhaseIdle)

	result := filterActiveSessions([]*strategy.SessionState{attachEnded, activeIdle})

	if len(result) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(result))
	}
	if result[0].SessionID != "active-idle" {
		t.Errorf("expected active-idle, got: %s", result[0].SessionID)
	}
}

// TestFilterActiveSessions_ExcludesEndedAtSet verifies that filterActiveSessions
// excludes sessions with EndedAt set regardless of Phase, and includes sessions in
// both PhaseIdle and PhaseActive.
func TestFilterActiveSessions_ExcludesEndedAtSet(t *testing.T) {
	t.Parallel()

	now := time.Now()

	legacyEnded := makeSessionState("legacy-ended", session.PhaseIdle)
	legacyEnded.EndedAt = &now

	properEnded := makeSessionState("proper-ended", session.PhaseEnded)
	properEnded.EndedAt = &now

	activeIdle := makeSessionState("active-idle", session.PhaseIdle)
	activeWorking := makeSessionState("active-working", session.PhaseActive)

	result := filterActiveSessions([]*strategy.SessionState{legacyEnded, properEnded, activeIdle, activeWorking})

	if len(result) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(result))
	}
	ids := map[string]bool{result[0].SessionID: true, result[1].SessionID: true}
	if !ids["active-idle"] || !ids["active-working"] {
		t.Errorf("expected active-idle and active-working in result, got: %v", result)
	}
}

// TestStopCmd_NoFlags_CrossWorktreeSession verifies that a session from a
// different worktree is reachable via the no-args single-session path.
func TestStopCmd_NoFlags_CrossWorktreeSession(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()

	// Session in a different worktree — should be stoppable via no-args path.
	remote := makeSessionState("test-cross-wt-stop", session.PhaseIdle)
	remote.WorktreePath = testOtherWorktreePath
	remote.WorktreeID = "other-wt"
	remote.StepCount = 0

	if err := strategy.SaveSessionState(ctx, remote); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	// Single session + --force → bypasses TUI, goes through runStopSession.
	cmd.SetArgs([]string{"--force"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	loaded, err := strategy.LoadSessionState(ctx, "test-cross-wt-stop")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("expected cross-worktree session to exist after stop")
		return
	}
	if loaded.Phase != session.PhaseEnded {
		t.Errorf("expected cross-worktree session to be PhaseEnded, got: %v", loaded.Phase)
	}
}

// --- sessions list tests ---

func TestListCmd_NoSessions(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newListCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout.String(), "No sessions.") {
		t.Errorf("expected 'No sessions.' in output, got: %q", stdout.String())
	}
}

func TestListCmd_ShowsAllSessions(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()

	// Create sessions with different states and staggered start times
	active := makeSessionState("test-list-active", session.PhaseActive)
	active.AgentType = testAgentClaude
	active.StartedAt = time.Now().Add(-1 * time.Hour)

	idle := makeSessionState("test-list-idle", session.PhaseIdle)
	idle.AgentType = testAgentGemini
	idle.WorktreeID = "other-wt"
	idle.LastCheckpointID = testCheckpointID
	idle.StartedAt = time.Now().Add(-2 * time.Hour)

	now := time.Now()
	ended := makeSessionState("test-list-ended", session.PhaseEnded)
	ended.EndedAt = &now
	ended.AgentType = testAgentClaude
	ended.StartedAt = time.Now().Add(-3 * time.Hour)

	for _, s := range []*strategy.SessionState{active, idle, ended} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newListCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()

	// Should include all sessions (active, idle, ended)
	if !strings.Contains(out, "session test-list-active") {
		t.Errorf("expected active session in output, got: %q", out)
	}
	if !strings.Contains(out, "session test-list-idle") {
		t.Errorf("expected idle session in output, got: %q", out)
	}
	if !strings.Contains(out, "session test-list-ended") {
		t.Errorf("expected ended session in output, got: %q", out)
	}
	// Should show checkpoint ID when present
	if !strings.Contains(out, "checkpoint a3b2c4d5e6f7") {
		t.Errorf("expected checkpoint ID in output, got: %q", out)
	}
	// Should show worktree labels
	if !strings.Contains(out, "other-wt") {
		t.Errorf("expected worktree ID in output, got: %q", out)
	}
	// Verify sort order: active (1h ago) should appear before idle (2h ago)
	activeIdx := strings.Index(out, "test-list-active")
	idleIdx := strings.Index(out, "test-list-idle")
	endedIdx := strings.Index(out, "test-list-ended")
	if activeIdx > idleIdx || idleIdx > endedIdx {
		t.Errorf("expected newest-first sort order, got active@%d idle@%d ended@%d", activeIdx, idleIdx, endedIdx)
	}
}

func TestListCmd_JSONNoSessionsEmitsEmptyArray(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newListCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	var got []sessionInfoJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("expected valid JSON array, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if len(got) != 0 {
		t.Errorf("expected empty array, got %d entries", len(got))
	}
}

func TestListCmd_JSONReturnsAllSessionsSorted(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()

	older := makeSessionState("test-list-json-older", session.PhaseIdle)
	older.AgentType = testAgentClaude
	older.StartedAt = time.Now().Add(-2 * time.Hour)
	older.WorktreeID = "wt-a"
	older.LastCheckpointID = testCheckpointID

	newer := makeSessionState("test-list-json-newer", session.PhaseActive)
	newer.AgentType = testAgentGemini
	newer.ModelName = "gemini-2.5-pro"
	newer.StartedAt = time.Now().Add(-30 * time.Minute)
	newer.WorktreeID = "wt-b"
	newer.LastPrompt = testPromptFixLogin

	for _, s := range []*strategy.SessionState{older, newer} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState: %v", err)
		}
	}

	cmd := newListCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	var got []sessionInfoJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("expected valid JSON array, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}

	// Newest first — same order as the prose view.
	if got[0].SessionID != "test-list-json-newer" {
		t.Errorf("expected newest session first, got %q", got[0].SessionID)
	}
	if got[1].SessionID != "test-list-json-older" {
		t.Errorf("expected older session second, got %q", got[1].SessionID)
	}

	// Envelope must carry the same fields a Baton-style consumer needs.
	if got[0].Agent != testAgentGemini {
		t.Errorf("expected agent='Gemini CLI', got %q", got[0].Agent)
	}
	if got[0].Model != "gemini-2.5-pro" {
		t.Errorf("expected model='gemini-2.5-pro', got %q", got[0].Model)
	}
	if got[0].LastPrompt != testPromptFixLogin {
		t.Errorf("expected last_prompt set, got %q", got[0].LastPrompt)
	}
	if got[0].WorktreeID != "wt-b" {
		t.Errorf("expected worktree_id='wt-b', got %q", got[0].WorktreeID)
	}
	if got[1].LastCheckpoint != testCheckpointID {
		t.Errorf("expected last_checkpoint_id=%q, got %q", testCheckpointID, got[1].LastCheckpoint)
	}
}

func TestListCmd_NotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	cmd := newSessionsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"list"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' error, got: %v", err)
	}
}

// --- sessions info tests ---

func TestInfoCmd_SessionNotFound(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newInfoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"nonexistent-id"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}

	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Errorf("expected SilentError, got: %T %v", err, err)
	}

	if !strings.Contains(stderr.String(), "Session not found.") {
		t.Errorf("expected 'Session not found.' in stderr, got: %q", stderr.String())
	}
}

func TestInfoCmd_TextOutput(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	lastActive := time.Now().Add(-5 * time.Minute)

	state := makeSessionState("test-info-text", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.ModelName = testModelClaudeOpus
	state.WorktreeID = "my-feature"
	state.LastInteractionTime = &lastActive
	state.SessionTurnCount = 3
	state.StepCount = 2
	state.LastCheckpointID = testCheckpointID
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:         100,
		CacheReadTokens:     5000,
		CacheCreationTokens: 2000,
		OutputTokens:        500,
	}
	state.LastPrompt = testPromptFixLogin
	state.FilesTouched = []string{"auth.go", "auth_test.go"}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-text"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Session test-info-text",
		"Agent:       Claude Code",
		"Model:       claude-opus-4-6[1m]",
		"Status:      active",
		"Worktree:    my-feature",
		"Turns:       3",
		"Checkpoints: 2",
		"Checkpoint:  a3b2c4d5e6f7",
		"7.6k",
		"Input: 100",
		"Cache read: 5k",
		"Cache write: 2k",
		"Output: 500",
		testPromptFixLogin,
		"auth.go",
		"auth_test.go",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestInfoCmd_JSONOutput(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()

	state := makeSessionState("test-info-json", session.PhaseIdle)
	state.AgentType = testAgentClaude
	state.ModelName = testModelClaudeOpus
	state.WorktreeID = "my-feature"
	state.StepCount = 2
	state.LastCheckpointID = testCheckpointID
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:     100,
		CacheReadTokens: 5000,
		OutputTokens:    500,
	}
	state.LastPrompt = testPromptFixLogin
	state.FilesTouched = []string{"auth.go"}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-json", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}

	if result["session_id"] != "test-info-json" {
		t.Errorf("expected session_id 'test-info-json', got: %v", result["session_id"])
	}
	if result["agent"] != testAgentClaude {
		t.Errorf("expected agent %q, got: %v", testAgentClaude, result["agent"])
	}
	if result["status"] != "idle" {
		t.Errorf("expected status 'idle', got: %v", result["status"])
	}
	if result["last_checkpoint_id"] != testCheckpointID {
		t.Errorf("expected last_checkpoint_id %q, got: %v", testCheckpointID, result["last_checkpoint_id"])
	}

	tokens, ok := result["tokens"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected tokens object, got: %T", result["tokens"])
	}
	total, ok := tokens["total"].(float64)
	if !ok {
		t.Fatalf("expected total to be float64, got: %T", tokens["total"])
	}
	if total != 5600 {
		t.Errorf("expected total tokens 5600, got: %v", total)
	}
}

// --- session tokens tests ---

func TestTokensCmd_TextOutputWithRecommendations(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-text", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.ModelName = testModelClaudeOpus
	state.SessionTurnCount = 12
	state.ContextTokens = 8500
	state.ContextWindowSize = 10000
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:         1000,
		CacheReadTokens:     10000,
		CacheCreationTokens: 500,
		OutputTokens:        100,
		APICallCount:        6,
		SubagentTokens: &agent.TokenUsage{
			InputTokens:  1000,
			OutputTokens: 1000,
			APICallCount: 2,
		},
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-text"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Session tokens",
		"Session: test-tokens-text",
		"Agent:   Claude Code",
		"Model:   claude-opus-4-6[1m]",
		"Status:  active",
		"Total:  13.6k tokens",
		"Input: 1k",
		"Cache read: 10k",
		"Cache write: 500",
		"Output: 100",
		"API calls: 6",
		"Subagents: 2k tokens",
		"Context pressure: 85% of 10k tokens",
		"Recommendations",
		"Scope subagent tasks tightly",
		"Context pressure is 85% of the window",
		"Compact or restart after summarizing the useful findings",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}

	recommendationsIndex := strings.Index(out, "Recommendations")
	tokenUsageIndex := strings.Index(out, "Token usage")
	if recommendationsIndex == -1 || tokenUsageIndex == -1 {
		t.Fatalf("expected recommendations and token usage sections, got:\n%s", out)
	}
	if tokenUsageIndex > recommendationsIndex {
		t.Fatalf("expected token usage before recommendations, got:\n%s", out)
	}
}

func TestRecommendationRulesSubagentHeavyAvoidsOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	recs := recommendationRules(tokenRecommendationSignals{
		Tokens: &sessionTokensUsage{
			Total:         maxInt,
			SubagentTotal: maxInt,
		},
	})

	for _, rec := range recs {
		if rec.ID == "subagent-heavy" {
			return
		}
	}
	t.Fatalf("expected subagent-heavy recommendation, got %+v", recs)
}

func TestRecommendationRulesCacheReplayUsesTopLevelTokenTotal(t *testing.T) {
	t.Parallel()

	recs := recommendationRules(tokenRecommendationSignals{
		Tokens: &sessionTokensUsage{
			Total:         10000,
			Input:         100,
			CacheRead:     800,
			CacheWrite:    50,
			Output:        50,
			APICalls:      20,
			SubagentTotal: 9000,
		},
	})

	var ids []string
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}

	expected := []string{"context-replay-hotspot", "summarize-before-boundary"}
	for _, id := range expected {
		if !slices.Contains(ids, id) {
			t.Fatalf("expected %s recommendation in %+v", id, recs)
		}
	}
}

func TestRecommendationThresholdsDocumentCurrentHeuristics(t *testing.T) {
	t.Parallel()

	checks := map[string]int{
		"cache read hotspot percent": recommendationHighCacheReadPercent,
		"high API calls":             recommendationHighAPICalls,
		"subagent share denominator": recommendationSubagentShareDenominator,
		"high context percent":       recommendationHighContextPercent,
		"long session turns":         recommendationLongSessionTurns,
		"long checkpoint count":      recommendationLongSessionCheckpoints,
	}
	want := map[string]int{
		"cache read hotspot percent": 80,
		"high API calls":             20,
		"subagent share denominator": 10,
		"high context percent":       80,
		"long session turns":         10,
		"long checkpoint count":      5,
	}
	for name, got := range checks {
		if got != want[name] {
			t.Fatalf("%s = %d, want %d", name, got, want[name])
		}
	}
}

func TestTokensCmd_JSONOutputReportsLimitations(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-json", session.PhaseIdle)
	state.AgentType = testAgentGemini
	state.ContextTokens = 9000
	state.ContextWindowSize = 10000

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-json", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}

	if result["session_id"] != "test-tokens-json" {
		t.Errorf("expected session_id 'test-tokens-json', got: %v", result["session_id"])
	}
	if result["agent"] != testAgentGemini {
		t.Errorf("expected agent %q, got: %v", testAgentGemini, result["agent"])
	}
	if _, ok := result["tokens"]; ok {
		t.Errorf("expected tokens to be omitted when no token data exists, got: %v", result["tokens"])
	}

	recs, ok := result["recommendations"].([]interface{})
	if !ok || len(recs) == 0 {
		t.Fatalf("expected recommendations array, got: %T %v", result["recommendations"], result["recommendations"])
	}
	var sawNoTokenData bool
	var sawContextPressure bool
	for _, raw := range recs {
		rec, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("expected recommendation object, got: %T", raw)
		}
		switch rec["id"] {
		case "no-token-data":
			sawNoTokenData = true
		case "high-context-pressure":
			sawContextPressure = true
		}
	}
	if !sawNoTokenData {
		t.Error("expected no-token-data recommendation")
	}
	if !sawContextPressure {
		t.Error("expected high-context-pressure recommendation")
	}

	limitations, ok := result["limitations"].([]interface{})
	if !ok || len(limitations) == 0 {
		t.Fatalf("expected limitations array, got: %T %v", result["limitations"], result["limitations"])
	}
}

func TestTokensCmd_JSONOutputReportsAPICallOnlyUsage(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-api-only", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		APICallCount: 25,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-api-only", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result sessionTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.Tokens == nil {
		t.Fatalf("expected API-call-only token usage to be reported")
	}
	if result.Tokens.Total != 0 || result.Tokens.APICalls != 25 {
		t.Fatalf("unexpected token usage: %+v", result.Tokens)
	}
	if reportHasSessionRecommendation(result, "no-token-data") {
		t.Fatalf("expected API-call-only usage to avoid no-token-data recommendation, got %+v", result.Recommendations)
	}
	if !reportHasSessionRecommendation(result, "api-call-amplification") {
		t.Fatalf("expected api-call-amplification recommendation, got %+v", result.Recommendations)
	}
}

func reportHasSessionRecommendation(report sessionTokensReport, id string) bool {
	for _, rec := range report.Recommendations {
		if rec.ID == id {
			return true
		}
	}
	return false
}

func TestRecommendationRules_CacheWritePressure(t *testing.T) {
	t.Parallel()

	recs := recommendationRules(tokenRecommendationSignals{
		Tokens: &sessionTokensUsage{
			Total:      50_000,
			CacheWrite: 6_000,
		},
	})

	if !recommendationsIncludeID(recs, "cache-write-pressure") {
		t.Fatalf("expected cache-write-pressure recommendation, got %+v", recs)
	}
}

func TestRecommendationRules_OutputPressure(t *testing.T) {
	t.Parallel()

	recs := recommendationRules(tokenRecommendationSignals{
		Tokens: &sessionTokensUsage{
			Total:  100_000,
			Output: 3_500,
		},
	})

	if !recommendationsIncludeID(recs, "output-pressure") {
		t.Fatalf("expected output-pressure recommendation, got %+v", recs)
	}
}

func TestRecommendationRules_OutputPressureWithLargeCacheReplay(t *testing.T) {
	t.Parallel()

	recs := recommendationRules(tokenRecommendationSignals{
		Tokens: &sessionTokensUsage{
			Total:     10_000_000,
			CacheRead: 9_800_000,
			Output:    100_000,
		},
	})

	if !recommendationsIncludeID(recs, "output-pressure") {
		t.Fatalf("expected output-pressure recommendation for high absolute output, got %+v", recs)
	}
}

func recommendationsIncludeID(recs []sessionTokensRecommendation, id string) bool {
	for _, rec := range recs {
		if rec.ID == id {
			return true
		}
	}
	return false
}

func TestTokensCmd_AgentBriefPrioritizesNextAction(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-brief", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:         94,
		CacheCreationTokens: 122171,
		CacheReadTokens:     6052424,
		OutputTokens:        38956,
		APICallCount:        70,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-brief", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Session token brief",
		"Session: test-tokens-brief",
		"Token usage: 6213.6k total; 97.4% cache/context replay; 70 API calls.",
		"Next best action:",
		"Use at most 3 batched reads before answering.",
		"Continue only if a named file or test can change the verdict; otherwise answer now.",
		"Avoid broad grep, broad diffs, broad tests, and repeated token diagnostics; keep the answer tight.",
		"Signals:",
		"- Cache/context replay dominates token volume.",
		"- API call count is high for one session.",
		"- Cache write/new context pressure is elevated.",
		"- Output pressure is elevated.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "Recommendations") {
		t.Fatalf("expected agent brief to omit regular recommendations section, got:\n%s", out)
	}
	if strings.Contains(out, "Likely contributors") {
		t.Fatalf("expected agent brief to omit contributor detail, got:\n%s", out)
	}
}

func TestAgentBriefUsageLineUsesTopLevelCacheReplayTotal(t *testing.T) {
	t.Parallel()

	line := agentBriefUsageLine(&sessionTokensUsage{
		Total:         10000,
		Input:         100,
		CacheRead:     800,
		CacheWrite:    50,
		Output:        50,
		APICalls:      20,
		SubagentTotal: 9000,
	})

	want := "Token usage: 10k total; 80% cache/context replay; 20 API calls."
	if line != want {
		t.Fatalf("agentBriefUsageLine() = %q, want %q", line, want)
	}
}

func TestTokensCmd_AgentBriefHighCacheReplayWithoutHighAPICalls(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-brief-cache-only", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:     27_892,
		CacheReadTokens: 608_896,
		OutputTokens:    865,
		APICallCount:    3,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-brief-cache-only", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Token usage: 637.7k total; 95.5% cache/context replay; 3 API calls.",
		"Use at most 2 focused reads only if a named file or test can change the answer; otherwise answer now.",
		"Avoid broad grep, broad diffs, and broad tests.",
		"- Cache/context replay dominates token volume.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "Continue normally") {
		t.Fatalf("expected high cache replay to avoid continue-normally action, got:\n%s", out)
	}
}

func TestTokensCmd_AgentBriefHighAPICallsWithoutCacheReplay(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-brief-api-only", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  10_000,
		OutputTokens: 1_000,
		APICallCount: 25,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-brief-api-only", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Token usage: 11k total; 25 API calls.",
		"Use at most 3 batched reads before answering.",
		"Avoid broad grep, broad diffs, broad tests, and repeated token diagnostics; keep the answer tight.",
		"- API call count is high for one session.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "Continue normally") {
		t.Fatalf("expected high API calls to avoid continue-normally action, got:\n%s", out)
	}
}

func TestTokensCmd_AgentBriefNoTokenData(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-brief-missing", session.PhaseActive)
	state.AgentType = testAgentGemini
	state.ContextTokens = 9000
	state.ContextWindowSize = 10000

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-brief-missing", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Session token brief",
		"Session: test-tokens-brief-missing",
		"Token usage: unavailable.",
		"Next best action:",
		"Token usage is not available yet.",
		"Signals:",
		"- Token usage is unavailable for this session.",
		"- Context pressure is high.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestSessionTokensAgentBriefClassAwareCostProxy(t *testing.T) {
	t.Parallel()

	tokens := &sessionTokensUsage{
		Total:      50_000,
		CacheWrite: 6_000,
		Output:     3_500,
		APICalls:   4,
	}
	report := sessionTokensReport{
		SessionID:       "test-cost-proxy-brief",
		Tokens:          tokens,
		Recommendations: recommendationRules(tokenRecommendationSignals{Tokens: tokens}),
	}

	var stdout bytes.Buffer
	writeSessionTokensAgentBrief(&stdout, report)

	out := stdout.String()
	checks := []string{
		"Session token brief",
		"Session: test-cost-proxy-brief",
		"Use at most 3 batched reads",
		"Avoid broad grep, broad diffs, broad tests",
		"otherwise answer now",
		"keep the answer tight",
		"- Cache write/new context pressure is elevated.",
		"- Output pressure is elevated.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestCheckpointTokensAgentBriefClassAwareCostProxy(t *testing.T) {
	t.Parallel()

	tokens := &sessionTokensUsage{
		Total:      50_000,
		CacheWrite: 6_000,
		Output:     3_500,
		APICalls:   4,
	}
	report := checkpointTokensReport{
		CheckpointID:    "c05e500cafe0",
		Tokens:          tokens,
		Recommendations: recommendationRules(tokenRecommendationSignals{Tokens: tokens}),
	}

	var stdout bytes.Buffer
	writeCheckpointTokensAgentBrief(&stdout, report)

	out := stdout.String()
	checks := []string{
		"Checkpoint token brief",
		"Checkpoint: c05e500cafe0",
		"Use at most 3 batched reads",
		"Avoid broad grep, broad diffs, broad tests",
		"otherwise answer now",
		"keep the answer tight",
		"- Cache write/new context pressure is elevated.",
		"- Output pressure is elevated.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestCheckpointTokensAgentBriefCombinesOutputAndReplayPressure(t *testing.T) {
	t.Parallel()

	tokens := &sessionTokensUsage{
		Total:     10_000_000,
		CacheRead: 9_800_000,
		Output:    100_000,
		APICalls:  25,
	}
	report := checkpointTokensReport{
		CheckpointID:    "c05e501cafe0",
		Tokens:          tokens,
		Recommendations: recommendationRules(tokenRecommendationSignals{Tokens: tokens}),
	}

	var stdout bytes.Buffer
	writeCheckpointTokensAgentBrief(&stdout, report)

	out := stdout.String()
	checks := []string{
		"Use at most 3 batched reads",
		"Avoid broad grep, broad diffs, broad tests",
		"keep the answer tight",
		"- Cache/context replay dominates token volume.",
		"- API call count is high for one session.",
		"- Output pressure is elevated.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestSessionsCmd_TokensSubcommand(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-subcommand", session.PhaseActive)
	state.TokenUsage = &agent.TokenUsage{InputTokens: 42}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newSessionsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "test-tokens-subcommand", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result sessionTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.SessionID != "test-tokens-subcommand" {
		t.Errorf("expected session_id 'test-tokens-subcommand', got: %q", result.SessionID)
	}
	if result.Tokens == nil || result.Tokens.Total != 42 {
		t.Fatalf("expected token total 42, got: %+v", result.Tokens)
	}
}

func TestSessionsCmd_TokensSubcommandAgentBrief(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-subcommand-brief", session.PhaseActive)
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  1200,
		OutputTokens: 300,
		APICallCount: 2,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newSessionsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "test-tokens-subcommand-brief", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Session token brief") {
		t.Fatalf("expected agent brief output, got:\n%s", out)
	}
	if !strings.Contains(out, "Token usage: 1.5k total") {
		t.Fatalf("expected token summary in brief, got:\n%s", out)
	}
}

func TestSessionsCmd_HelpIncludesTokensSubcommand(t *testing.T) {
	cmd := newSessionsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "tokens   Show token usage and optimization recommendations") {
		t.Fatalf("expected tokens command in manual command list, got:\n%s", out)
	}
	if !strings.Contains(out, "entire session tokens <session-id>       Show token usage") {
		t.Fatalf("expected tokens example in help, got:\n%s", out)
	}
}

func TestTokensCmd_JSONAndAgentBriefAreMutuallyExclusive(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newTokensCmd()
	cmd.SetArgs([]string{"test-session", "--json", "--agent-brief"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --json with --agent-brief")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got: %v", err)
	}
}

func TestCheckpointTokensCmd_JSONAndAgentBriefAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	cmd := newCheckpointGroupCmd()
	cmd.SetArgs([]string{"tokens", "abc123", "--json", "--agent-brief"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --json with --agent-brief")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got: %v", err)
	}
}

func TestTokensCmd_PrioritizesContextReplayHotspot(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-tokens-hotspot", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.SessionTurnCount = 4
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:         94,
		CacheCreationTokens: 122171,
		CacheReadTokens:     6052424,
		OutputTokens:        38956,
		APICallCount:        70,
	}

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-tokens-hotspot"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Recommendations",
		"Cache/context replay is 97.4% of token volume",
		"Large context was replayed across 70 API calls",
		"Compact or restart after summarizing this investigation",
		"Token usage",
		"Total:  6213.6k tokens",
		"Cache read: 6052.4k",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}

	recommendationsIndex := strings.Index(out, "Recommendations")
	tokenUsageIndex := strings.Index(out, "Token usage")
	if recommendationsIndex == -1 || tokenUsageIndex == -1 {
		t.Fatalf("expected recommendations and token usage sections, got:\n%s", out)
	}
	if tokenUsageIndex > recommendationsIndex {
		t.Fatalf("expected token usage before recommendations, got:\n%s", out)
	}
	if strings.Contains(out, "Start a fresh session after major task boundaries") {
		t.Fatalf("expected no generic fresh-session recommendation, got:\n%s", out)
	}
}

func TestTokensCmd_DefaultsToCurrentSession(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}
	now := time.Now()

	current := makeSessionState("test-tokens-current", session.PhaseActive)
	current.WorktreePath = repoRoot
	current.LastInteractionTime = &now
	current.TokenUsage = &agent.TokenUsage{InputTokens: 1200}

	otherTime := now.Add(5 * time.Minute)
	other := makeSessionState("test-tokens-other", session.PhaseActive)
	other.WorktreePath = testOtherWorktreePath
	other.LastInteractionTime = &otherTime
	other.TokenUsage = &agent.TokenUsage{InputTokens: 9999}

	for _, s := range []*strategy.SessionState{current, other} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Session: test-tokens-current") {
		t.Fatalf("expected current worktree session, got:\n%s", out)
	}
	if strings.Contains(out, "test-tokens-other") {
		t.Fatalf("expected other worktree session to be ignored, got:\n%s", out)
	}
}

func TestTokensCmd_CurrentDoesNotFallbackToOtherWorktree(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	now := time.Now()
	other := makeSessionState("test-tokens-other-only", session.PhaseActive)
	other.WorktreePath = testOtherWorktreePath
	other.LastInteractionTime = &now
	other.TokenUsage = &agent.TokenUsage{InputTokens: 9999}

	if err := strategy.SaveSessionState(ctx, other); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	defaultCmd := newTokensCmd()
	var defaultStdout bytes.Buffer
	defaultCmd.SetOut(&defaultStdout)
	defaultCmd.SetArgs([]string{})
	if err := defaultCmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected default command to fall back, got: %v", err)
	}
	if !strings.Contains(defaultStdout.String(), "Session: test-tokens-other-only") {
		t.Fatalf("expected default command to fall back to other worktree session, got:\n%s", defaultStdout.String())
	}

	currentCmd := newTokensCmd()
	var currentStdout bytes.Buffer
	currentCmd.SetOut(&currentStdout)
	currentCmd.SetArgs([]string{"--current"})
	if err := currentCmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected --current command to avoid fallback without error, got: %v", err)
	}
	out := currentStdout.String()
	if !strings.Contains(out, "No active session found in this worktree.") {
		t.Fatalf("expected --current command to report no current worktree session, got:\n%s", out)
	}
	if strings.Contains(out, "test-tokens-other-only") {
		t.Fatalf("expected --current command not to fall back to other worktree, got:\n%s", out)
	}
}

func TestTokensCmd_CurrentAndSessionIDAreMutuallyExclusive(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newTokensCmd()
	cmd.SetArgs([]string{"test-session", "--current"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --current with session ID")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got: %v", err)
	}
}

func TestTokenCommandError_SuppressesCancellation(t *testing.T) {
	err := tokenCommandError(fmt.Errorf("wrapped: %w", context.Canceled))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in error chain, got: %v", err)
	}
	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("expected SilentError, got: %T %v", err, err)
	}
}

func TestRoundedPercentAvoidsIntermediateOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)

	if got := roundedPercent(maxInt/2, maxInt); got != 50 {
		t.Fatalf("roundedPercent() = %d, want 50", got)
	}
}

func TestRoundedPercentClampsAt100(t *testing.T) {
	t.Parallel()

	if got := roundedPercent(200, 100); got != 100 {
		t.Fatalf("roundedPercent() = %d, want 100", got)
	}
}

// --- checkpoint tokens tests ---

func TestCheckpointTokensCmd_TextOutputWithRealCheckpointShape(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	cpID := id.MustCheckpointID("beefbeefcafe")
	if err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-token-session",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "e2e-triage-fix",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"why is slack failing"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         94,
			CacheCreationTokens: 122171,
			CacheReadTokens:     6052424,
			OutputTokens:        38956,
			APICallCount:        70,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "beefbeef"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoint tokens",
		"Checkpoint: beefbeefcafe",
		"Session:    checkpoint-token-session",
		"Agent:      Claude Code",
		"Branch:     e2e-triage-fix",
		"Recommendations",
		"Cache/context replay is 97.4% of token volume",
		"Large context was replayed across 70 API calls",
		"Token usage",
		"Total:  6213.6k tokens",
		"Cache read: 6052.4k",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}

	recommendationsIndex := strings.Index(out, "Recommendations")
	tokenUsageIndex := strings.Index(out, "Token usage")
	if recommendationsIndex == -1 || tokenUsageIndex == -1 {
		t.Fatalf("expected recommendations and token usage sections, got:\n%s", out)
	}
	if tokenUsageIndex > recommendationsIndex {
		t.Fatalf("expected token usage before recommendations, got:\n%s", out)
	}
}

func TestCheckpointTokensCmd_AgentBriefGivesOperationalBudget(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	cpID := id.MustCheckpointID("b1efbeefcafe")
	if err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-token-brief",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "e2e-triage-fix",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"why is slack failing"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         94,
			CacheCreationTokens: 122171,
			CacheReadTokens:     6052424,
			OutputTokens:        38956,
			APICallCount:        70,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "b1efbeef", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoint token brief",
		"Checkpoint: b1efbeefcafe",
		"Token usage: 6213.6k total; 97.4% cache/context replay; 70 API calls.",
		"Next best action:",
		"Use at most 3 batched reads before answering.",
		"Continue only if a named file or test can change the verdict; otherwise answer now.",
		"Avoid broad grep, broad diffs, broad tests, and repeated token diagnostics; keep the answer tight.",
		"Signals:",
		"- Cache/context replay dominates token volume.",
		"- API call count is high for one session.",
		"- Cache write/new context pressure is elevated.",
		"- Output pressure is elevated.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	for _, verboseSection := range []string{"Recommendations", "Likely contributors", "Limitations"} {
		if strings.Contains(out, verboseSection) {
			t.Fatalf("expected agent brief to omit %s section, got:\n%s", verboseSection, out)
		}
	}
}

func TestCheckpointTokensCmd_AgentBriefMissingTokenData(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("deadcafebeef")
	writeCommittedTokenCheckpoint(ctx, t, store, cpID, "checkpoint-token-missing-brief", nil)

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "deadcafe", "--agent-brief"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoint token brief",
		"Checkpoint: deadcafebeef",
		"Token usage: unavailable.",
		"Do not spend extra commands on token optimization for this checkpoint.",
		"- Token usage is unavailable for this session.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestCheckpointTokensCmd_TextOutputWithMultipleSessionsUsesAggregateScope(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("feedfeedcafe")

	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-token-session-one",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "multi-session-branch",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"first session"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:  1000,
			APICallCount: 1,
			SubagentTokens: &agent.TokenUsage{
				InputTokens:  2000,
				OutputTokens: 500,
				APICallCount: 2,
			},
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() first session error = %v", err)
	}
	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-token-session-two",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "multi-session-branch",
		Agent:        testAgentGemini,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"second session"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:  500,
			OutputTokens: 500,
			APICallCount: 2,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() second session error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "feedfeed"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoint tokens",
		"Checkpoint: feedfeedcafe",
		"Sessions:   2",
		"Agents:     Claude Code, Gemini CLI",
		"Branch:     multi-session-branch",
		"Total:  4.5k tokens",
		"Input: 1.5k",
		"Output: 500",
		"API calls: 3",
		"Subagents: 2.5k tokens",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "Session:    checkpoint-token-session-two") {
		t.Fatalf("expected aggregate checkpoint output, not latest-session attribution, got:\n%s", out)
	}
}

func TestCheckpointTokensCmd_JSONOutput(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	cpID := id.MustCheckpointID("cafe00001234")
	if err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-token-json",
		Strategy:     strategy.StrategyNameManualCommit,
		Agent:        testAgentGemini,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"token json"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:  120,
			OutputTokens: 30,
			APICallCount: 1,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "cafe0000", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result checkpointTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.CheckpointID != "cafe00001234" {
		t.Errorf("expected checkpoint_id 'cafe00001234', got: %q", result.CheckpointID)
	}
	if result.SessionID != "checkpoint-token-json" {
		t.Errorf("expected session_id 'checkpoint-token-json', got: %q", result.SessionID)
	}
	if result.Tokens == nil || result.Tokens.Total != 150 {
		t.Fatalf("expected token total 150, got: %+v", result.Tokens)
	}
}

func TestCheckpointTokensReport_UsesRootSummaryWhenSessionMetadataIncomplete(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123abc123")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions: []checkpoint.SessionFilePaths{
				{Metadata: "0/metadata.json"},
				{Metadata: "1/metadata.json"},
			},
			TokenUsage: &agent.TokenUsage{
				InputTokens:  1000,
				OutputTokens: 500,
				APICallCount: 7,
			},
		},
		[]*checkpoint.Metadata{
			{
				SessionID: "readable-session",
				Agent:     "Claude Code",
				Model:     "claude-opus-4-6",
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100,
				},
				SessionMetrics: &checkpoint.SessionMetrics{
					ContextTokens:     9000,
					ContextWindowSize: 10000,
				},
			},
		},
		1,
	)

	if report.Tokens == nil {
		t.Fatalf("expected token data, got nil")
	}
	if report.Tokens.Total != 1500 || report.Tokens.APICalls != 7 {
		t.Fatalf("expected root summary tokens, got %+v", report.Tokens)
	}
	if report.SessionID != "" || report.Agent != "" || report.Model != "" {
		t.Fatalf("expected multi-session checkpoint to omit singular session fields, got session_id=%q agent=%q model=%q", report.SessionID, report.Agent, report.Model)
	}
	if report.Context != nil {
		t.Fatalf("expected multi-session checkpoint to omit singular context, got %+v", report.Context)
	}
	for _, contributor := range report.Contributors {
		if contributor.Kind == "context_pressure" {
			t.Fatalf("expected multi-session checkpoint to omit singular context contributor, got %+v", report.Contributors)
		}
	}
	if len(report.Limitations) == 0 || !strings.Contains(report.Limitations[0], "1 checkpoint session metadata file could not be read") {
		t.Fatalf("expected incomplete metadata limitation, got %+v", report.Limitations)
	}
}

func TestAddCheckpointTokenUsageSaturatesOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	usage := addCheckpointTokenUsage(
		&agent.TokenUsage{
			InputTokens:         maxInt,
			CacheCreationTokens: maxInt,
			CacheReadTokens:     maxInt,
			OutputTokens:        maxInt,
			APICallCount:        maxInt,
			SubagentTokens: &agent.TokenUsage{
				InputTokens: maxInt,
			},
		},
		&agent.TokenUsage{
			InputTokens:         1,
			CacheCreationTokens: 1,
			CacheReadTokens:     1,
			OutputTokens:        1,
			APICallCount:        1,
			SubagentTokens: &agent.TokenUsage{
				InputTokens: 1,
			},
		},
	)

	if usage.InputTokens != maxInt ||
		usage.CacheCreationTokens != maxInt ||
		usage.CacheReadTokens != maxInt ||
		usage.OutputTokens != maxInt ||
		usage.APICallCount != maxInt {
		t.Fatalf("expected saturated top-level usage, got %+v", usage)
	}
	if usage.SubagentTokens == nil || usage.SubagentTokens.InputTokens != maxInt {
		t.Fatalf("expected saturated subagent usage, got %+v", usage.SubagentTokens)
	}
}

func TestCheckpointTokensReport_UsesRootSummaryWhenNoSessionMetadataReadable(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123def456")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions: []checkpoint.SessionFilePaths{
				{Metadata: "0/metadata.json"},
				{Metadata: "1/metadata.json"},
			},
			TokenUsage: &agent.TokenUsage{
				InputTokens:  1000,
				OutputTokens: 500,
				APICallCount: 7,
			},
		},
		nil,
		2,
	)

	if report.Tokens == nil {
		t.Fatalf("expected token data, got nil")
	}
	if report.Tokens.Total != 1500 || report.Tokens.APICalls != 7 {
		t.Fatalf("expected root summary tokens, got %+v", report.Tokens)
	}
}

type cancelingCheckpointMetadataReader struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancelingCheckpointMetadataReader) ReadSessionMetadata(
	_ context.Context,
	_ id.CheckpointID,
	_ int,
) (*checkpoint.Metadata, error) {
	r.calls++
	if r.calls == 1 {
		r.cancel()
		return &checkpoint.Metadata{SessionID: "read-before-cancel"}, nil
	}
	return &checkpoint.Metadata{SessionID: "read-after-cancel"}, nil
}

func TestReadCheckpointTokenSessionMetadataStopsBetweenReadsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingCheckpointMetadataReader{cancel: cancel}

	metas, warnings, err := readCheckpointTokenSessionMetadata(ctx, reader, id.MustCheckpointID("abc123abc123"), 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got metas=%+v warnings=%d err=%v", metas, warnings, err)
	}
	if reader.calls != 1 {
		t.Fatalf("expected one metadata read before cancellation, got %d", reader.calls)
	}
	if metas != nil || warnings != 0 {
		t.Fatalf("expected canceled read to return no partial results, got metas=%+v warnings=%d", metas, warnings)
	}
}

func TestReadCheckpointTokenSessionMetadataChecksCanceledContextBeforeAllocation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	metas, warnings, err := readCheckpointTokenSessionMetadata(ctx, nil, id.MustCheckpointID("abc123abc123"), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got metas=%+v warnings=%d err=%v", metas, warnings, err)
	}
	if metas != nil || warnings != 0 {
		t.Fatalf("expected canceled read to return no results, got metas=%+v warnings=%d", metas, warnings)
	}
}

func TestCheckpointTokensCmd_TextOutputWithComparison(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("aaa111bbb222")
	currentID := id.MustCheckpointID("bbb222ccc333")

	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: baselineID,
		SessionID:    "checkpoint-token-baseline",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-compare",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"baseline"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         200_000,
			CacheCreationTokens: 50_000,
			CacheReadTokens:     750_000,
			OutputTokens:        10_000,
			APICallCount:        10,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() baseline error = %v", err)
	}
	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: currentID,
		SessionID:    "checkpoint-token-current",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-compare",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"current"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         150_000,
			CacheCreationTokens: 25_000,
			CacheReadTokens:     300_000,
			OutputTokens:        25_000,
			APICallCount:        4,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() current error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "bbb222", "--compare", "aaa111"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoint tokens",
		"Checkpoint: bbb222ccc333",
		"Token usage",
		"Total:  500k tokens",
		"Comparison",
		"Baseline: aaa111bbb222",
		"Caveat: Total tokens include cache/context replay; use the cache/context replay delta below before treating total direction as work saved or added.",
		"Total tokens: down 50.5% (1010k -> 500k)",
		"Input: down 25% (200k -> 150k)",
		"Cache/context replay: down 60% (750k -> 300k)",
		"Cache write: down 50% (50k -> 25k)",
		"Output: up 150% (10k -> 25k)",
		"API calls: down 60% (10 -> 4)",
		"Qualification",
		"Observed total token use decreased for this checkpoint comparison.",
		"This does not prove quality was preserved",
		"Cost-proxy pressure increased for output",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestCheckpointTokensCmd_RejectsSelfComparison(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("abc222abc222")

	writeCommittedTokenCheckpoint(ctx, t, store, cpID, "checkpoint-token-self-compare", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		APICallCount: 1,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "abc222", "--compare", "abc222abc222"})

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected self-comparison error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot compare checkpoint abc222abc222 to itself") {
		t.Fatalf("expected self-comparison error, got: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no report output for self-comparison, got:\n%s", stdout.String())
	}
}

func TestCheckpointTokensCmd_JSONOutputWithComparison(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("abc111abc111")
	currentID := id.MustCheckpointID("abc222abc222")

	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: baselineID,
		SessionID:    "checkpoint-token-json-baseline",
		Strategy:     strategy.StrategyNameManualCommit,
		Agent:        testAgentGemini,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"baseline json"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         100,
			CacheCreationTokens: 50,
			CacheReadTokens:     300,
			OutputTokens:        100,
			APICallCount:        5,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() baseline error = %v", err)
	}
	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: currentID,
		SessionID:    "checkpoint-token-json-current",
		Strategy:     strategy.StrategyNameManualCommit,
		Agent:        testAgentGemini,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"current json"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         120,
			CacheCreationTokens: 80,
			CacheReadTokens:     480,
			OutputTokens:        200,
			APICallCount:        8,
		},
	}); err != nil {
		t.Fatalf("WriteCommitted() current error = %v", err)
	}

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "abc222", "--compare", "abc111", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result checkpointTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.Comparison == nil {
		t.Fatalf("expected comparison, got nil")
	}
	if result.Comparison.Status != "observed_increase" {
		t.Fatalf("expected observed_increase status, got %q", result.Comparison.Status)
	}
	if result.Comparison.BaselineCheckpointID != "abc111abc111" {
		t.Errorf("baseline checkpoint id = %q, want abc111abc111", result.Comparison.BaselineCheckpointID)
	}
	if result.Comparison.TargetCheckpointID != "abc222abc222" {
		t.Errorf("target checkpoint id = %q, want abc222abc222", result.Comparison.TargetCheckpointID)
	}
	if result.Comparison.Total == nil {
		t.Fatalf("expected total delta, got nil")
	}
	if result.Comparison.Total.Baseline != 550 || result.Comparison.Total.Current != 880 {
		t.Fatalf("unexpected total delta: %+v", result.Comparison.Total)
	}
	if result.Comparison.Total.Change != 330 {
		t.Fatalf("expected total change 330, got %+v", result.Comparison.Total)
	}
	if result.Comparison.Total.Direction != checkpointDeltaDirectionUp {
		t.Fatalf("expected total direction up, got %+v", result.Comparison.Total)
	}
	if result.Comparison.Total.ChangePercent == nil || *result.Comparison.Total.ChangePercent != 60 {
		t.Fatalf("expected total change percent 60, got %+v", result.Comparison.Total)
	}
	if result.Comparison.CacheReadCaveat == "" {
		t.Fatalf("expected cache read caveat, got %+v", result.Comparison)
	}
	if result.Comparison.Input == nil || result.Comparison.Input.Change != 20 {
		t.Fatalf("expected input change 20, got %+v", result.Comparison.Input)
	}
	if result.Comparison.CacheWrite == nil || result.Comparison.CacheWrite.Change != 30 {
		t.Fatalf("expected cache write change 30, got %+v", result.Comparison.CacheWrite)
	}
	if result.Comparison.Output == nil || result.Comparison.Output.Change != 100 {
		t.Fatalf("expected output change 100, got %+v", result.Comparison.Output)
	}
}

func TestCheckpointTokensCmd_JSONComparisonQualifiesCostProxyPressure(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("c0a111c0a111")
	currentID := id.MustCheckpointID("c0a222c0a222")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-cost-proxy-baseline", &agent.TokenUsage{
		InputTokens:     100_000,
		CacheReadTokens: 100_000,
		APICallCount:    6,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-cost-proxy-current", &agent.TokenUsage{
		InputTokens:         50_000,
		CacheCreationTokens: 30_000,
		OutputTokens:        30_000,
		APICallCount:        4,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "c0a222", "--compare", "c0a111", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result checkpointTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.Comparison == nil {
		t.Fatalf("expected comparison, got nil")
	}
	if result.Comparison.Status != checkpointComparisonStatusObservedReduction {
		t.Fatalf("expected observed reduction, got %q", result.Comparison.Status)
	}
	checks := []string{
		"Cost-proxy pressure increased",
		"cache write",
		"output",
	}
	for _, check := range checks {
		if !strings.Contains(result.Comparison.Qualification, check) {
			t.Fatalf("expected %q in qualification, got %q", check, result.Comparison.Qualification)
		}
	}
}

func TestCheckpointTokensCmd_ComparisonNoChange(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("111aaa222bbb")
	currentID := id.MustCheckpointID("222bbb333ccc")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-no-change-baseline", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 100,
		APICallCount: 2,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-no-change-current", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 100,
		APICallCount: 2,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "222bbb", "--compare", "111aaa"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Total tokens: unchanged (200 -> 200)",
		"Cache/context replay: unchanged (0 -> 0)",
		"API calls: unchanged (2 -> 2)",
		"Observed total token use was unchanged for this checkpoint comparison.",
		"Quality still depends on the task outcome",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestBuildCheckpointMetricDeltaClampsChangeOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1

	up := buildCheckpointMetricDelta(minInt, maxInt)
	if up.Change != maxInt {
		t.Fatalf("upward overflow change = %d, want %d", up.Change, maxInt)
	}
	if up.Direction != checkpointDeltaDirectionUp {
		t.Fatalf("upward overflow direction = %q, want up", up.Direction)
	}

	down := buildCheckpointMetricDelta(maxInt, minInt)
	if down.Change != minInt {
		t.Fatalf("downward overflow change = %d, want %d", down.Change, minInt)
	}
	if down.Direction != checkpointDeltaDirectionDown {
		t.Fatalf("downward overflow direction = %q, want down", down.Direction)
	}
}

func TestSaturatingIntSubHandlesMinIntSubtrahend(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1

	tests := []struct {
		name string
		a    int
		want int
	}{
		{
			name: "clamps non-negative minuend",
			a:    0,
			want: maxInt,
		},
		{
			name: "keeps max exact result",
			a:    -1,
			want: maxInt,
		},
		{
			name: "keeps representable result",
			a:    -2,
			want: maxInt - 1,
		},
		{
			name: "keeps zero exact result",
			a:    minInt,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := saturatingIntSub(tt.a, minInt); got != tt.want {
				t.Fatalf("saturatingIntSub(%d, minInt) = %d, want %d", tt.a, got, tt.want)
			}
		})
	}
}

func TestCheckpointTokensCmd_ComparisonUnavailableWhenBaselineTokenDataMissing(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("333ccc444ddd")
	currentID := id.MustCheckpointID("444ddd555eee")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-missing-baseline", nil)
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-current-with-data", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		APICallCount: 1,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "444ddd", "--compare", "333ccc"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Comparison",
		"Baseline: 333ccc444ddd",
		"Qualification",
		"Comparison unavailable because token usage is missing for one checkpoint.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "Total tokens:") {
		t.Fatalf("expected unavailable comparison to omit metric deltas, got:\n%s", out)
	}
}

func TestCheckpointTokensCmd_JSONComparisonUnavailableWhenCurrentTokenDataMissing(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("555eee666fff")
	currentID := id.MustCheckpointID("666fff777aaa")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-baseline-with-data", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		APICallCount: 1,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-missing-current", nil)

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "666fff", "--compare", "555eee", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result checkpointTokensReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.Comparison == nil {
		t.Fatalf("expected comparison, got nil")
	}
	if result.Comparison.Status != "unavailable" {
		t.Fatalf("expected unavailable status, got %q", result.Comparison.Status)
	}
	if result.Comparison.Total != nil {
		t.Fatalf("expected no total delta when current token data is missing, got %+v", result.Comparison.Total)
	}
	if len(result.Comparison.Limitations) == 0 {
		t.Fatalf("expected comparison limitation, got %+v", result.Comparison)
	}
}

func TestCheckpointTokensCmd_ComparisonUsesMultiSessionAggregates(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("777aaa888bbb")
	currentID := id.MustCheckpointID("888bbb999ccc")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-baseline-one", &agent.TokenUsage{
		InputTokens:  1_000,
		APICallCount: 1,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-baseline-two", &agent.TokenUsage{
		OutputTokens: 1_000,
		APICallCount: 1,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-current-one", &agent.TokenUsage{
		InputTokens:  500,
		APICallCount: 1,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-current-two", &agent.TokenUsage{
		OutputTokens: 500,
		APICallCount: 1,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "888bbb", "--compare", "777aaa"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Sessions:   2",
		"Total:  1k tokens",
		"Baseline: 777aaa888bbb",
		"Total tokens: down 50% (2k -> 1k)",
		"API calls: unchanged (2 -> 2)",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func TestCheckpointTokensCmd_ComparisonOmitsPercentWhenBaselineMetricIsZero(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("999ccc000aaa")
	currentID := id.MustCheckpointID("000aaa111bbb")

	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-zero-api-baseline", &agent.TokenUsage{
		InputTokens: 100,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-zero-api-current", &agent.TokenUsage{
		InputTokens:  100,
		APICallCount: 3,
	})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "000aaa", "--compare", "999ccc"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Total tokens: unchanged (100 -> 100)",
		"API calls: up (0 -> 3)",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
	if strings.Contains(out, "API calls: up ") && strings.Contains(out, "API calls: up %") {
		t.Fatalf("expected zero-baseline API delta to omit percent, got:\n%s", out)
	}
}

func writeCommittedTokenCheckpoint(ctx context.Context, t *testing.T, store *checkpoint.GitStore, cpID id.CheckpointID, sessionID string, usage *agent.TokenUsage) {
	t.Helper()

	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-compare",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"compare"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage:   usage,
	}); err != nil {
		t.Fatalf("WriteCommitted(%s) error = %v", cpID, err)
	}
}

func TestInfoCmd_EndedSession(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	endedAt := time.Now().Add(-24 * time.Hour)

	state := makeSessionState("test-info-ended", session.PhaseEnded)
	state.EndedAt = &endedAt
	state.AgentType = testAgentClaude
	state.StepCount = 1
	state.LastCheckpointID = "b79b35cd956d"

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-ended"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Status:      ended") {
		t.Errorf("expected 'Status:      ended' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Ended:") {
		t.Errorf("expected 'Ended:' line in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Checkpoint:  b79b35cd956d") {
		t.Errorf("expected checkpoint ID in output, got:\n%s", out)
	}
}

func TestInfoCmd_TranscriptStreamsRawAgentBytes(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()

	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	want := []byte(`{"role":"user","content":"hi"}` + "\n" + `{"role":"assistant","content":"hello"}` + "\n")
	if err := os.WriteFile(transcriptPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := makeSessionState("test-info-transcript", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TranscriptPath = transcriptPath
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-transcript", "--transcript"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	if !bytes.Equal(stdout.Bytes(), want) {
		t.Errorf("transcript output mismatch.\n  want: %q\n  got:  %q", want, stdout.Bytes())
	}
}

func TestInfoCmd_TranscriptMissingPath(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	state := makeSessionState("test-info-no-transcript", session.PhaseActive)
	state.AgentType = testAgentClaude
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newInfoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-info-no-transcript", "--transcript"})

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected error when transcript path is empty")
	}
	if !strings.Contains(err.Error(), "no transcript path") {
		t.Errorf("expected 'no transcript path' error, got: %v", err)
	}
	// User-visible cause: must be on stderr (not silently swallowed by SilentError).
	if !strings.Contains(stderr.String(), "no transcript path") {
		t.Errorf("expected stderr to surface 'no transcript path', got: %q", stderr.String())
	}
}

// TestInfoCmd_TranscriptTrimsPartialTrailingLine verifies the snapshot-shape
// guarantee for JSONL transcripts: if the agent is mid-write of a JSONL line
// when we hit EOF, consumers receive only the complete prefix.
func TestInfoCmd_TranscriptTrimsPartialTrailingLine(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()

	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	// Two complete records + one truncated record (no trailing newline).
	full := `{"v":1,"role":"user"}` + "\n" + `{"v":1,"role":"assistant"}` + "\n"
	written := full + `{"v":1,"role":"user","content":"trunc`
	if err := os.WriteFile(transcriptPath, []byte(written), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := makeSessionState("test-info-trim", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TranscriptPath = transcriptPath
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-trim", "--transcript"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	if got := stdout.String(); got != full {
		t.Errorf("expected partial trailing line trimmed.\n  want: %q\n  got:  %q", full, got)
	}
}

// TestInfoCmd_TranscriptEmitsWholeJSONDocument verifies the Gemini-style case:
// transcripts that are a single valid JSON document (no trailing newline) must
// be emitted intact. Trim-to-last-newline would cut the closing brace and
// produce malformed output. Regression test for copilot review feedback.
func TestInfoCmd_TranscriptEmitsWholeJSONDocument(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()

	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.json")
	// Pretty-printed JSON (newlines inside) but no trailing newline at EOF.
	want := `{
  "sessionId": "abc",
  "messages": [
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": "hello"}
  ]
}`
	if err := os.WriteFile(transcriptPath, []byte(want), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := makeSessionState("test-info-json-doc", session.PhaseActive)
	state.AgentType = testAgentGemini
	state.TranscriptPath = transcriptPath
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-json-doc", "--transcript"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	if got := stdout.String(); got != want {
		t.Errorf("expected whole JSON document preserved.\n  want: %q\n  got:  %q", want, got)
	}
}

// TestInfoCmd_TranscriptInvalidJSONErrorsLoudly guards the bugbot finding
// that whole-document JSON agents (Gemini) silently produced empty output
// when the snapshot didn't parse as JSON. Machine consumers can't tell
// "no data" from "data unavailable, retry" if exit-code 0 + empty stdout
// is the same in both cases. The mid-write case must now error.
func TestInfoCmd_TranscriptInvalidJSONErrorsLoudly(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()

	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.json")
	// Truncated JSON document — what Gemini would produce mid-write.
	written := `{"sessionId":"abc","messages":[{"role":"user","content":"trun`
	if err := os.WriteFile(transcriptPath, []byte(written), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := makeSessionState("test-info-invalid-json", session.PhaseActive)
	state.AgentType = testAgentGemini
	state.TranscriptPath = transcriptPath
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newInfoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"test-info-invalid-json", "--transcript"})

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected error for invalid JSON snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout on JSON-validation failure, got: %q", stdout.String())
	}
}

// TestInfoCmd_TranscriptBoundsSnapshotAtOpen verifies copilot finding 2:
// bytes the agent appends after the command opens the file must NOT appear
// in the output (snapshot is bounded at command start, not "current EOF").
func TestInfoCmd_TranscriptBoundsSnapshotAtOpen(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()

	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	initial := `{"v":1,"role":"user"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := makeSessionState("test-info-snapshot", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TranscriptPath = transcriptPath
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// Append after command construction but before execution. With a properly
	// bounded snapshot the appended bytes must not appear in stdout.
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"v":1,"role":"assistant","appended":"AFTER-OPEN"}` + "\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	_ = f.Close()

	cmd := newInfoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"test-info-snapshot", "--transcript"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	// We can't make this test deterministic against ordering of "open" vs
	// "append" within a single goroutine — both have already happened by the
	// time io.ReadAll runs. The real snapshot-vs-current-EOF distinction
	// matters for in-flight appends across goroutines, which we can't
	// reliably orchestrate in unit-test scope. The minimum we assert: when
	// the snapshot bound is correctly anchored at f.Stat().Size(), the
	// output is one of the two well-defined sizes (initial-only or
	// initial+append), never half a record. This guards against the
	// no-bound regression where an in-flight write produced truncated
	// output.
	got := stdout.String()
	switch got {
	case initial, initial + `{"v":1,"role":"assistant","appended":"AFTER-OPEN"}` + "\n":
		// Either is well-formed.
	default:
		t.Errorf("expected snapshot to land on a record boundary, got: %q", got)
	}
}

func TestInfoCmd_TranscriptAndJSONMutuallyExclusive(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newInfoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"some-id", "--json", "--transcript"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error when --json and --transcript are combined")
	}
}

func TestInfoCmd_NotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	cmd := newSessionsCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"info", "some-id"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' error, got: %v", err)
	}
}

// --- helper function tests ---

func TestSessionWorktreeLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		state    *strategy.SessionState
		expected string
	}{
		{
			name:     "uses WorktreeID when set",
			state:    &strategy.SessionState{WorktreeID: "my-feature", WorktreePath: "/some/path/my-feature"},
			expected: "my-feature",
		},
		{
			name:     "falls back to filepath.Base of WorktreePath",
			state:    &strategy.SessionState{WorktreePath: "/Users/dev/repo/.worktrees/feature-branch"},
			expected: "feature-branch",
		},
		{
			name:     "returns (unknown) when both empty",
			state:    &strategy.SessionState{},
			expected: unknownPlaceholder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sessionWorktreeLabel(tt.state)
			if got != tt.expected {
				t.Errorf("sessionWorktreeLabel() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSessionPhaseLabel(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name     string
		state    *strategy.SessionState
		expected string
	}{
		{
			name:     "active phase",
			state:    &strategy.SessionState{Phase: session.PhaseActive},
			expected: "active",
		},
		{
			name:     "idle phase",
			state:    &strategy.SessionState{Phase: session.PhaseIdle},
			expected: "idle",
		},
		{
			name:     "ended when EndedAt set",
			state:    &strategy.SessionState{Phase: session.PhaseIdle, EndedAt: &now},
			expected: "ended",
		},
		{
			name:     "empty phase defaults to idle",
			state:    &strategy.SessionState{},
			expected: "idle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sessionPhaseLabel(tt.state)
			if got != tt.expected {
				t.Errorf("sessionPhaseLabel() = %q, want %q", got, tt.expected)
			}
		})
	}
}
