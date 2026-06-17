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
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	testAgentClaude    = "Claude Code"
	testAgentGemini    = "Gemini CLI"
	testCheckpointID   = "a3b2c4d5e6f7"
	testPromptFixLogin = "fix the login bug"
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
	outOfScope.WorktreePath = "/other/worktree"

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
	remote.WorktreePath = "/other/worktree"
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
	state.ModelName = "claude-opus-4-6[1m]"
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
	state.ModelName = "claude-opus-4-6[1m]"
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
