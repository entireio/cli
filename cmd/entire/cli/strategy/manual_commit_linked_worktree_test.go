package strategy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestFindSessionsForWorktree_LinkedWorktreeFallsBackToMain verifies that a
// hook firing from inside a linked worktree (such as Claude Code's
// .claude/worktrees/<branch> agent feature) falls back to sessions whose
// WorktreePath was registered against the main worktree when no session is
// registered against the linked worktree itself.
//
// Regression coverage for the path where prepare-commit-msg / post-commit
// silently bail with "no active sessions" — strict path equality on
// WorktreePath was returning no matches because the session was created from
// the main worktree (where the user's prompt and UserPromptSubmit hook fired)
// but the commit fires from inside an agent-spawned linked worktree.
func TestFindSessionsForWorktree_LinkedWorktreeFallsBackToMain(t *testing.T) {
	mainDir := t.TempDir()
	// EvalSymlinks so /var → /private/var on macOS — git rev-parse returns the
	// canonical form, and we need to compare like-for-like in the test below.
	resolved, err := filepath.EvalSymlinks(mainDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainDir = resolved
	initTestRepo(t, mainDir)

	linkedDir := filepath.Join(mainDir, ".claude", "worktrees", "feature-x")
	if err := createWorktree(mainDir, linkedDir, "feature-x"); err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { removeWorktree(mainDir, linkedDir) })

	t.Chdir(linkedDir)

	strat := NewManualCommitStrategy()
	ctx := context.Background()

	// Active session registered against the MAIN worktree path. Phase=Active so
	// listAllSessionStates doesn't prune it for lacking a shadow branch.
	mainSession := &session.State{
		SessionID:    "11111111-1111-1111-1111-111111111111",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: mainDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, mainSession); err != nil {
		t.Fatalf("saveSessionState (main): %v", err)
	}

	// Session registered against an unrelated sibling worktree — must NOT
	// match a commit firing from linkedDir.
	siblingDir := filepath.Join(mainDir, ".claude", "worktrees", "sibling")
	otherSession := &session.State{
		SessionID:    "22222222-2222-2222-2222-222222222222",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: siblingDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, otherSession); err != nil {
		t.Fatalf("saveSessionState (sibling): %v", err)
	}

	matched, err := strat.findSessionsForWorktree(ctx, linkedDir)
	if err != nil {
		t.Fatalf("findSessionsForWorktree: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, s := range matched {
		gotIDs[s.SessionID] = true
	}
	if !gotIDs[mainSession.SessionID] {
		t.Errorf("expected main-worktree session %q to be returned from linked worktree, got %+v", mainSession.SessionID, gotIDs)
	}
	if gotIDs[otherSession.SessionID] {
		t.Errorf("sibling-worktree session %q should NOT match from linked worktree %q", otherSession.SessionID, linkedDir)
	}
}

// TestFindSessionsForWorktree_MainWorktreeUnchanged verifies the widening is
// strictly one-directional: from the main worktree we must not start
// returning sessions whose WorktreePath points at a linked worktree.
// Otherwise a commit on main would silently pick up an unrelated agent
// session running in .claude/worktrees/<branch> and link the commit to it.
func TestFindSessionsForWorktree_MainWorktreeUnchanged(t *testing.T) {
	mainDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(mainDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainDir = resolved
	initTestRepo(t, mainDir)
	t.Chdir(mainDir)

	strat := NewManualCommitStrategy()
	ctx := context.Background()

	mainSession := &session.State{
		SessionID:    "33333333-3333-3333-3333-333333333333",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: mainDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, mainSession); err != nil {
		t.Fatalf("saveSessionState (main): %v", err)
	}

	linkedDir := filepath.Join(mainDir, ".claude", "worktrees", "feature-x")
	linkedSession := &session.State{
		SessionID:    "44444444-4444-4444-4444-444444444444",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: linkedDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, linkedSession); err != nil {
		t.Fatalf("saveSessionState (linked): %v", err)
	}

	matched, err := strat.findSessionsForWorktree(ctx, mainDir)
	if err != nil {
		t.Fatalf("findSessionsForWorktree: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, s := range matched {
		gotIDs[s.SessionID] = true
	}
	if !gotIDs[mainSession.SessionID] {
		t.Errorf("main-worktree session must be returned when looking up from main, got %+v", gotIDs)
	}
	if gotIDs[linkedSession.SessionID] {
		t.Errorf("linked-worktree session %q must NOT bleed into main-worktree lookup, got %+v", linkedSession.SessionID, gotIDs)
	}
}

// TestFindSessionsForWorktree_LinkedWorktreeWithOwnSessionIgnoresMain verifies
// that the linked → main fallback only fires when the linked worktree has no
// session of its own. If the user has independently started an agent in a
// linked worktree (e.g. `git worktree add ../wt && cd ../wt && agent`), a
// commit there must link only to that worktree's session — bundling in main's
// session would silently attach unrelated session context to the commit.
func TestFindSessionsForWorktree_LinkedWorktreeWithOwnSessionIgnoresMain(t *testing.T) {
	mainDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(mainDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainDir = resolved
	initTestRepo(t, mainDir)

	linkedDir := filepath.Join(mainDir, ".claude", "worktrees", "feature-x")
	if err := createWorktree(mainDir, linkedDir, "feature-x"); err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { removeWorktree(mainDir, linkedDir) })

	t.Chdir(linkedDir)

	strat := NewManualCommitStrategy()
	ctx := context.Background()

	// Session registered against the linked worktree — the user started an
	// agent inside the worktree directly.
	linkedSession := &session.State{
		SessionID:    "55555555-5555-5555-5555-555555555555",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: linkedDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, linkedSession); err != nil {
		t.Fatalf("saveSessionState (linked): %v", err)
	}

	// Unrelated session also active in main. Must NOT be bundled with a
	// commit happening in the linked worktree when the linked worktree has
	// its own session.
	mainSession := &session.State{
		SessionID:    "66666666-6666-6666-6666-666666666666",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: mainDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, mainSession); err != nil {
		t.Fatalf("saveSessionState (main): %v", err)
	}

	matched, err := strat.findSessionsForWorktree(ctx, linkedDir)
	if err != nil {
		t.Fatalf("findSessionsForWorktree: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, s := range matched {
		gotIDs[s.SessionID] = true
	}
	if !gotIDs[linkedSession.SessionID] {
		t.Errorf("linked-worktree session %q must be returned from its own worktree, got %+v", linkedSession.SessionID, gotIDs)
	}
	if gotIDs[mainSession.SessionID] {
		t.Errorf("main-worktree session %q must NOT be bundled when the linked worktree has its own session, got %+v", mainSession.SessionID, gotIDs)
	}
}

// TestFindSessionsForWorktree_LinkedFallbackSkipsSiblings verifies that when a
// linked worktree falls back for lack of a local session, the fallback finds
// only the main-worktree session — never a sibling linked worktree's session.
// Without this, a commit in worktree A could silently adopt a session from a
// concurrent agent run in worktree B.
func TestFindSessionsForWorktree_LinkedFallbackSkipsSiblings(t *testing.T) {
	mainDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(mainDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainDir = resolved
	initTestRepo(t, mainDir)

	linkedDir := filepath.Join(mainDir, ".claude", "worktrees", "feature-x")
	if err := createWorktree(mainDir, linkedDir, "feature-x"); err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { removeWorktree(mainDir, linkedDir) })

	t.Chdir(linkedDir)

	strat := NewManualCommitStrategy()
	ctx := context.Background()

	siblingDir := filepath.Join(mainDir, ".claude", "worktrees", "sibling")
	siblingSession := &session.State{
		SessionID:    "77777777-7777-7777-7777-777777777777",
		BaseCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		WorktreePath: siblingDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}
	if err := strat.saveSessionState(ctx, siblingSession); err != nil {
		t.Fatalf("saveSessionState (sibling): %v", err)
	}

	// No session for linkedDir, no session for mainDir — fallback should
	// resolve to an empty list, not to siblingSession.
	matched, err := strat.findSessionsForWorktree(ctx, linkedDir)
	if err != nil {
		t.Fatalf("findSessionsForWorktree: %v", err)
	}
	if len(matched) != 0 {
		var ids []string
		for _, s := range matched {
			ids = append(ids, s.SessionID)
		}
		t.Errorf("expected no matches (no local session, no main session); got %v", ids)
	}
}
