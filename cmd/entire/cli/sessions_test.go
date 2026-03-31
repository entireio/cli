package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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
	}
	if loaded.Phase != session.PhaseEnded {
		t.Errorf("expected Phase=PhaseEnded, got: %v", loaded.Phase)
	}
}

func TestStopCmd_SingleSession_WithCheckpoint(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("test-stop-checkpoint-1", session.PhaseIdle)
	state.LastCheckpointID = "a3b2c4d5e6f7"
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
		}
		if loaded.Phase != session.PhaseEnded {
			t.Errorf("expected session %s Phase=PhaseEnded, got: %v", id, loaded.Phase)
		}
	}
}

func TestStopCmd_AllFlag_ExcludesOtherWorktrees(t *testing.T) {
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

	stopped, err := strategy.LoadSessionState(ctx, "test-all-scope-in")
	if err != nil {
		t.Fatalf("LoadSessionState(in-scope) error = %v", err)
	}
	if stopped == nil || stopped.Phase != session.PhaseEnded {
		t.Errorf("expected in-scope session to be PhaseEnded, got: %v", stopped.Phase)
	}

	untouched, err := strategy.LoadSessionState(ctx, "test-all-scope-out")
	if err != nil {
		t.Fatalf("LoadSessionState(out-of-scope) error = %v", err)
	}
	if untouched == nil || untouched.Phase == session.PhaseEnded {
		t.Errorf("expected out-of-scope session to remain non-ended, got: %v", untouched.Phase)
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

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})

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
		if loaded == nil || loaded.Phase != session.PhaseEnded {
			t.Errorf("expected session %s to be PhaseEnded after batch stop", id)
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
	}
	if loaded.Phase != session.PhaseIdle {
		t.Errorf("expected Phase to remain PhaseIdle (legacy), got: %v", loaded.Phase)
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

// TestStopCmd_WorktreeScoping_NoFlags verifies that the no-flags path scopes session
// listing to the current worktree, so sessions from other worktrees are invisible.
func TestStopCmd_WorktreeScoping_NoFlags(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}

	// One session in the current worktree, one in a foreign worktree.
	inScope := makeSessionState("test-stop-scope-in", session.PhaseIdle)
	inScope.WorktreePath = worktreePath

	outOfScope := makeSessionState("test-stop-scope-out", session.PhaseIdle)
	outOfScope.WorktreePath = "/some/other/worktree"

	for _, s := range []*strategy.SessionState{inScope, outOfScope} {
		if err := strategy.SaveSessionState(ctx, s); err != nil {
			t.Fatalf("SaveSessionState() error = %v", err)
		}
	}

	cmd := newStopCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	// Single in-scope session → confirm + stop path (bypasses TUI selector).
	cmd.SetArgs([]string{"--force"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// In-scope session must be stopped.
	stopped, err := strategy.LoadSessionState(ctx, "test-stop-scope-in")
	if err != nil {
		t.Fatalf("LoadSessionState(in-scope) error = %v", err)
	}
	if stopped == nil {
		t.Fatal("expected in-scope session to exist")
	}
	if stopped.Phase != session.PhaseEnded {
		t.Errorf("expected in-scope session Phase=PhaseEnded, got: %v", stopped.Phase)
	}

	// Out-of-scope session must be untouched.
	untouched, err := strategy.LoadSessionState(ctx, "test-stop-scope-out")
	if err != nil {
		t.Fatalf("LoadSessionState(out-of-scope) error = %v", err)
	}
	if untouched == nil {
		t.Fatal("expected out-of-scope session to exist")
	}
	if untouched.Phase == session.PhaseEnded {
		t.Errorf("expected out-of-scope session to remain non-ended, got PhaseEnded")
	}
}
