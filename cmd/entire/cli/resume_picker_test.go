package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
)

func ptrTime(t time.Time) *time.Time { return &t }

func TestFilterResumableSessions(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	running := &strategy.SessionState{SessionID: "running", Phase: session.PhaseActive, StartedAt: base, LastInteractionTime: ptrTime(base.Add(5 * time.Hour))}
	idle := &strategy.SessionState{SessionID: "idle", Phase: session.PhaseIdle, StartedAt: base, LastInteractionTime: ptrTime(base.Add(2 * time.Hour))}
	endedByPhase := &strategy.SessionState{SessionID: "ended-phase", Phase: session.PhaseEnded, StartedAt: base, EndedAt: ptrTime(base.Add(1 * time.Hour))}
	endedByTime := &strategy.SessionState{SessionID: "ended-time", Phase: session.PhaseIdle, StartedAt: base, EndedAt: ptrTime(base.Add(3 * time.Hour))}

	got := filterResumableSessions([]*strategy.SessionState{nil, running, idle, endedByPhase, endedByTime})

	// Everything except the currently-active session is resumable (idle included).
	if len(got) != 3 {
		t.Fatalf("expected 3 resumable sessions, got %d", len(got))
	}
	for _, s := range got {
		if s.Phase == session.PhaseActive {
			t.Fatal("active session should be excluded")
		}
	}
	// Sorted most-recently-active first: ended-time (t+3h), idle (t+2h), ended-phase (t+1h).
	if got[0].SessionID != "ended-time" || got[1].SessionID != "idle" || got[2].SessionID != "ended-phase" {
		t.Fatalf("unexpected order: %s, %s, %s", got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}
}

func TestSessionLastActiveTime(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	interacted := started.Add(time.Hour)
	ended := started.Add(2 * time.Hour)

	if got := sessionLastActiveTime(&strategy.SessionState{StartedAt: started}); !got.Equal(started) {
		t.Errorf("started-only: got %v want %v", got, started)
	}
	if got := sessionLastActiveTime(&strategy.SessionState{StartedAt: started, LastInteractionTime: &interacted}); !got.Equal(interacted) {
		t.Errorf("interaction: got %v want %v", got, interacted)
	}
	if got := sessionLastActiveTime(&strategy.SessionState{StartedAt: started, LastInteractionTime: &interacted, EndedAt: &ended}); !got.Equal(ended) {
		t.Errorf("ended: got %v want %v", got, ended)
	}
}

func TestResumeOptionLabel(t *testing.T) {
	t.Parallel()

	s := &strategy.SessionState{
		SessionID:  "s1",
		AgentType:  "Claude Code",
		LastPrompt: "fix   the\nthing",
		StartedAt:  time.Now().Add(-2 * time.Hour),
	}

	cpID := id.MustCheckpointID("abc123abc123")
	selectable := resumeOptionLabel(resumableSession{state: s, branch: "experiment", checkpointID: cpID})
	if !strings.HasPrefix(selectable, "experiment · ") {
		t.Errorf("selectable label should start with branch, got %q", selectable)
	}
	if !strings.Contains(selectable, "Claude Code") {
		t.Errorf("selectable label should name the agent, got %q", selectable)
	}
	// Whitespace in the prompt is collapsed.
	if !strings.Contains(selectable, "fix the thing") {
		t.Errorf("prompt whitespace should be collapsed, got %q", selectable)
	}

	noBranch := resumeOptionLabel(resumableSession{state: s, branch: "", checkpointID: cpID})
	if !strings.Contains(noBranch, "can't resume") || !strings.Contains(noBranch, "no branch") {
		t.Errorf("no-branch label should say it can't resume (no branch), got %q", noBranch)
	}

	noCheckpoint := resumeOptionLabel(resumableSession{state: s, branch: "experiment"})
	if !strings.Contains(noCheckpoint, "can't resume") || !strings.Contains(noCheckpoint, "no committed checkpoint") {
		t.Errorf("no-checkpoint label should say it can't resume (no committed checkpoint), got %q", noCheckpoint)
	}
}

func TestResumeOptionLabel_EmptyFields(t *testing.T) {
	t.Parallel()

	label := resumeOptionLabel(resumableSession{
		state:  &strategy.SessionState{SessionID: "s1", StartedAt: time.Now()},
		branch: "b",
	})
	if !strings.Contains(label, "(unknown agent)") {
		t.Errorf("missing agent should render placeholder, got %q", label)
	}
	if !strings.Contains(label, "(no prompt recorded)") {
		t.Errorf("missing prompt should render placeholder, got %q", label)
	}
}

func TestBuildResumeOptions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	items := []resumableSession{
		{state: &strategy.SessionState{SessionID: "a", StartedAt: now}, branch: "feat-a", checkpointID: id.MustCheckpointID("abc123abc123")},
		{state: &strategy.SessionState{SessionID: "b", StartedAt: now}, branch: ""},
	}

	options, hasSelectable := buildResumeOptions(items)
	if !hasSelectable {
		t.Fatal("expected at least one selectable option")
	}
	// One option per item plus Cancel.
	if len(options) != len(items)+1 {
		t.Fatalf("expected %d options, got %d", len(items)+1, len(options))
	}
	// Per-item options are keyed by index; the last is Cancel.
	if options[0].Value != strconv.Itoa(0) || options[1].Value != strconv.Itoa(1) {
		t.Errorf("options should be keyed by index, got %q, %q", options[0].Value, options[1].Value)
	}
	if options[len(options)-1].Value != resumePickerCancel {
		t.Errorf("last option should be Cancel, got %q", options[len(options)-1].Value)
	}
}

func TestBuildResumeOptions_NoneSelectable(t *testing.T) {
	t.Parallel()

	// Neither a branch-less entry nor a branch-with-no-checkpoint entry is
	// selectable — both lack something required to resume.
	items := []resumableSession{
		{state: &strategy.SessionState{SessionID: "a", StartedAt: time.Now()}, branch: ""},
		{state: &strategy.SessionState{SessionID: "b", StartedAt: time.Now()}, branch: "has-branch-no-cp"},
	}
	_, hasSelectable := buildResumeOptions(items)
	if hasSelectable {
		t.Error("expected no selectable options when entries lack a branch or a checkpoint")
	}
}

// TestResumableSession_RequiresCheckpoint covers the reviewer's case: a session
// with a stored branch but no committed checkpoint (e.g. an idle session that
// never committed) must not be selectable, while one with both is.
func TestResumableSession_RequiresCheckpoint(t *testing.T) {
	t.Parallel()

	withCheckpoint := resumableSession{branch: "b", checkpointID: id.MustCheckpointID("abc123abc123")}
	if !withCheckpoint.isResumable() {
		t.Error("branch + checkpoint should be resumable")
	}

	branchOnly := resumableSession{branch: "b"} // empty checkpoint ID
	if branchOnly.isResumable() {
		t.Error("a branch with no committed checkpoint must not be resumable")
	}
	if branchOnly.unresumableReason() != "no committed checkpoint" {
		t.Errorf("unexpected reason: %q", branchOnly.unresumableReason())
	}
}

// TestResolveResumableBranches_TwoSessionsSameBranch covers the reviewer's case:
// two sessions sharing one branch must each carry their own checkpoint ID, so
// selecting one resumes that session rather than the branch's latest.
func TestResolveResumableBranches_TwoSessionsSameBranch(t *testing.T) {
	t.Parallel()

	const sharedBranch = "two-sessions-shared"

	tmpDir := t.TempDir()
	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)
	testutil.CreateBranch(t, tmpDir, sharedBranch)

	cpA := id.MustCheckpointID("aa11bb22cc33")
	cpB := id.MustCheckpointID("dd44ee55ff66")
	states := []*strategy.SessionState{
		{SessionID: "sess-a", Branch: sharedBranch, LastCheckpointID: cpA},
		{SessionID: "sess-b", Branch: sharedBranch, LastCheckpointID: cpB},
	}

	items := resolveResumableBranches(repo, states)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for _, it := range items {
		if it.branch != sharedBranch {
			t.Errorf("session %s: branch = %q, want %q", it.state.SessionID, it.branch, sharedBranch)
		}
		if !it.isResumable() {
			t.Errorf("session %s should be resumable", it.state.SessionID)
		}
	}
	// Crucially, each item carries its OWN checkpoint, not a shared/latest one.
	if items[0].checkpointID != cpA || items[1].checkpointID != cpB {
		t.Errorf("checkpoints not carried per session: got %s, %s; want %s, %s",
			items[0].checkpointID, items[1].checkpointID, cpA, cpB)
	}
}

// TestResumeByCheckpointID_ResumesRequestedSession verifies the action resumes
// the exact selected session's checkpoint, not the latest on the branch — two
// committed checkpoints exist, and resuming one restores only that session.
func TestResumeByCheckpointID_ResumesRequestedSession(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	cpA := id.MustCheckpointID("aa11bb22cc33")
	cpB := id.MustCheckpointID("dd44ee55ff66")
	writeCommittedResumeCheckpoint(t, repo, cpA, "session-a", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	writeCommittedResumeCheckpoint(t, repo, cpB, "session-b", time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC)) // newer

	// Resume the OLDER session-a, even though session-b is newer.
	var out strings.Builder
	if err := resumeByCheckpointID(context.Background(), &out, &out, cpA, false); err != nil {
		t.Fatalf("resumeByCheckpointID(cpA) error: %v\noutput: %s", err, out.String())
	}

	combined := out.String()
	if !strings.Contains(combined, "session-a") {
		t.Errorf("expected resume command for session-a, got:\n%s", combined)
	}
	if strings.Contains(combined, "session-b") {
		t.Errorf("must NOT resume session-b when session-a was requested, got:\n%s", combined)
	}
	// session-a's transcript is restored; session-b's is left untouched.
	if _, err := os.Stat(filepath.Join(claudeDir, "session-a.jsonl")); err != nil {
		t.Errorf("session-a transcript should have been restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "session-b.jsonl")); !os.IsNotExist(err) {
		t.Errorf("session-b transcript should NOT have been restored (err=%v)", err)
	}
}

// TestResolveSessionBranch_Derived covers the fallback that maps a session to a
// branch via its last checkpoint ID found in that branch's commit trailers.
func TestResolveSessionBranch_Derived(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "base.txt", "base")
	testutil.GitAdd(t, tmpDir, "base.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.GitCheckoutNewBranch(t, tmpDir, "experiment")
	cpID, err := id.Generate()
	if err != nil {
		t.Fatalf("generate checkpoint id: %v", err)
	}
	testutil.WriteFile(t, tmpDir, "work.txt", "work")
	testutil.GitAdd(t, tmpDir, "work.txt")
	testutil.GitCommit(t, tmpDir, "do work\n\nEntire-Checkpoint: "+cpID.String())

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	const experimentBranch = "experiment"

	index := buildCheckpointBranchIndex(repo)
	if got := index[cpID.String()]; got != experimentBranch {
		t.Fatalf("checkpoint should index to 'experiment', got %q (index=%v)", got, index)
	}

	// No stored branch → resolves via checkpoint index.
	derived := &strategy.SessionState{SessionID: "s", LastCheckpointID: cpID}
	if got := resolveSessionBranch(repo, derived, index); got != experimentBranch {
		t.Errorf("derived branch: got %q want experiment", got)
	}

	// Stored branch that exists wins without needing the index.
	stored := &strategy.SessionState{SessionID: "s2", Branch: experimentBranch}
	if got := resolveSessionBranch(repo, stored, map[string]string{}); got != experimentBranch {
		t.Errorf("stored branch: got %q want experiment", got)
	}

	// Stored branch that no longer exists and no checkpoint match → unresolvable.
	gone := &strategy.SessionState{SessionID: "s3", Branch: "deleted-branch"}
	if got := resolveSessionBranch(repo, gone, map[string]string{}); got != "" {
		t.Errorf("missing branch should be unresolvable, got %q", got)
	}
}

// TestBuildCheckpointBranchIndex_SkipsInternalRefs verifies that Entire's own
// internal refs (entire/checkpoints/*, shadow branches) are never indexed, so a
// session can't be mis-resolved to a non-resumable internal branch.
func TestBuildCheckpointBranchIndex_SkipsInternalRefs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "base.txt", "base")
	testutil.GitAdd(t, tmpDir, "base.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// A real user branch carrying a checkpoint trailer.
	testutil.GitCheckoutNewBranch(t, tmpDir, "user-branch")
	userCp, err := id.Generate()
	if err != nil {
		t.Fatalf("generate checkpoint id: %v", err)
	}
	testutil.WriteFile(t, tmpDir, "u.txt", "u")
	testutil.GitAdd(t, tmpDir, "u.txt")
	testutil.GitCommit(t, tmpDir, "user work\n\nEntire-Checkpoint: "+userCp.String())

	// An internal entire/ branch that also carries a checkpoint trailer.
	testutil.GitCheckoutNewBranch(t, tmpDir, "entire/deadbeef-abc123")
	internalCp, err := id.Generate()
	if err != nil {
		t.Fatalf("generate checkpoint id: %v", err)
	}
	testutil.WriteFile(t, tmpDir, "i.txt", "i")
	testutil.GitAdd(t, tmpDir, "i.txt")
	testutil.GitCommit(t, tmpDir, "internal\n\nEntire-Checkpoint: "+internalCp.String())

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	index := buildCheckpointBranchIndex(repo)
	if got := index[userCp.String()]; got != "user-branch" {
		t.Errorf("user checkpoint should index to 'user-branch', got %q", got)
	}
	if got, ok := index[internalCp.String()]; ok {
		t.Errorf("internal entire/ branch checkpoint should not be indexed, got %q", got)
	}
}

// TestBranchCheckedOutElsewhere verifies worktree awareness: a branch checked
// out in another worktree is detected (with its path), while the current
// worktree's own branch and unknown branches are not flagged.
func TestBranchCheckedOutElsewhere(t *testing.T) {
	// Mutates process cwd via t.Chdir — cannot run in parallel.
	mainDir := t.TempDir()
	testutil.InitRepo(t, mainDir)
	testutil.WriteFile(t, mainDir, "base.txt", "base")
	testutil.GitAdd(t, mainDir, "base.txt")
	testutil.GitCommit(t, mainDir, "init")

	mainRepo, err := git.PlainOpen(mainDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	head, err := mainRepo.Head()
	_ = mainRepo.Close()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	currentBranch := head.Name().Short()

	// Create a branch and check it out in a second worktree.
	const sharedBranch = "shared-wt-branch"
	testutil.CreateBranch(t, mainDir, sharedBranch)
	wtDir := filepath.Join(t.TempDir(), "wt")
	runGit(t, mainDir, "worktree", "add", wtDir, sharedBranch)

	t.Chdir(mainDir)
	ctx := context.Background()

	// The branch checked out in the other worktree is detected, with its path.
	gotPath, ok := branchCheckedOutElsewhere(ctx, sharedBranch)
	if !ok {
		t.Fatalf("expected %q to be detected as checked out elsewhere", sharedBranch)
	}
	if normalizeWorktreePath(gotPath) != normalizeWorktreePath(wtDir) {
		t.Errorf("worktree path: got %q, want %q", gotPath, wtDir)
	}

	// The current worktree's own branch is NOT "elsewhere".
	if _, ok := branchCheckedOutElsewhere(ctx, currentBranch); ok {
		t.Errorf("current worktree's branch %q should not be reported as elsewhere", currentBranch)
	}

	// An unknown branch is not flagged.
	if _, ok := branchCheckedOutElsewhere(ctx, "no-such-branch"); ok {
		t.Error("unknown branch should not be reported as checked out elsewhere")
	}
}
