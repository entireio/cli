//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// The adopt ownership guard, composed through the real binary.
//
// The in-process tests in cmd/entire/cli cover the decision logic thoroughly,
// and deliberately: ancestry ranking and the ambiguity rules are pure
// decisions best exercised with real proclive identities in memory. What they
// cannot cover is the composition, and two parts of it matter here.
//
// First, they build adoptOptions directly, so nothing between cobra and the
// guard is exercised — a misregistered --allow-foreign-session passes every
// one of them (verified by renaming the flag and watching them all stay
// green).
//
// Second, they get their non-interactive verdict from testing.Testing(), which
// is gate 2 of interactive.CanPromptInteractively. An agent in the field trips
// gate 3 or gate 5 instead. RunCLIWithError spawns through
// execx.NonInteractive — a new session with no controlling terminal — so the
// child reaches the /dev/tty probe and refuses for the production reason. That
// is the exact shape of the incident this guard exists for: an agent running
// `session adopt` non-interactively on an ID it read out of `session current`.
//
// Deliberately NOT re-tested here: the nearest-owner comparison, the
// ambiguity outcomes, and the confirmation prompt. The first two are decision
// logic with better in-process coverage, and the third needs a PTY.

// seedAdoptableSession writes a live, adoptable session into repo's own
// session store.
//
// TranscriptPath is left empty on purpose. validateAdoptSourceTranscript
// short-circuits on an empty path, whereas a populated one would be validated
// against the agent's session directory as resolved by the CHILD process —
// which runs with the target repo's ENTIRE_TEST_CLAUDE_PROJECT_DIR, not the
// source's. Seeding a path here would therefore fail transcript validation for
// a reason that has nothing to do with the ownership gate under test.
func seedAdoptableSession(t *testing.T, repoDir, sessionID string) {
	t.Helper()
	lastInteraction := time.Now().Add(-1 * time.Minute)
	store := session.NewStateStoreWithDir(filepath.Join(repoDir, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		WorktreePath:        repoDir,
	}); err != nil {
		t.Fatalf("seed session %s: %v", sessionID, err)
	}
}

func loadSeededSession(t *testing.T, repoDir, sessionID string) *session.State {
	t.Helper()
	store := session.NewStateStoreWithDir(filepath.Join(repoDir, ".git", session.SessionStateDirName))
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session %s: %v", sessionID, err)
	}
	return state
}

// A non-interactive caller that cannot be shown to own the session is refused,
// the remedy reaches the caller's output, and the source session is untouched.
func TestSessionAdoptGuard_RefusesUnownedSessionNonInteractively(t *testing.T) {
	t.Parallel()

	source := NewRepoWithCommit(t)
	target := NewRepoWithCommit(t)

	const sessionID = "integration-unowned-session"
	seedAdoptableSession(t, source.RepoDir, sessionID)

	// No agent variable: nothing can identify this command's caller, and the
	// seeded session records no owner for ancestry to match either.
	output, err := target.RunCLIWithError("session", "adopt", sessionID, "--from", source.RepoDir, "--force")
	if err == nil {
		t.Fatalf("adopt succeeded on a session it cannot show is the caller's\nOutput: %s", output)
	}

	// The remedy must be in the OUTPUT, not only on the error: the refusal is
	// a SilentError and main.go prints nothing for those.
	for _, want := range []string{
		"Cannot confirm it is yours",
		"There is no terminal to confirm on",
		"--allow-foreign-session",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("refusal output missing %q\nOutput: %s", want, output)
		}
	}

	state := loadSeededSession(t, source.RepoDir, sessionID)
	if state == nil {
		t.Fatal("refused adoption deleted the source session")
	}
	if state.WorktreePath != source.RepoDir || state.Phase != session.PhaseActive {
		t.Errorf("refused adoption mutated the source session: worktree=%q phase=%q",
			state.WorktreePath, state.Phase)
	}
}

// The supported path: the agent publishes the session ID it owns, so the same
// command succeeds without a prompt and without the override flag. This is
// what stops the guard being a wall — and it is the only test that proves the
// caller-session variable survives into a spawned binary at all.
func TestSessionAdoptGuard_AdoptsTheCallersOwnSessionNonInteractively(t *testing.T) {
	t.Parallel()

	source := NewRepoWithCommit(t)
	target := NewRepoWithCommit(t)

	const sessionID = "integration-owned-session"
	seedAdoptableSession(t, source.RepoDir, sessionID)

	// Set for the CHILD only. TestMain unsets every caller-session variable
	// process-wide so the developer's real session cannot leak in, which is
	// why this has to be handed over explicitly rather than inherited.
	target.ExtraEnv = append(target.ExtraEnv, "CLAUDE_CODE_SESSION_ID="+sessionID)
	target.WriteFile("feature.txt", "agent change\n")

	output, err := target.RunCLIWithError("session", "adopt", sessionID, "--from", source.RepoDir, "--force")
	if err != nil {
		t.Fatalf("adopt refused the caller's own session: %v\nOutput: %s", err, output)
	}
	if !strings.Contains(output, "Adopted session") {
		t.Errorf("expected an adoption confirmation, got: %s", output)
	}
	if strings.Contains(output, "Cannot confirm it is yours") {
		t.Errorf("adopting the caller's own session should be silent, got: %s", output)
	}

	if state := loadSeededSession(t, target.RepoDir, sessionID); state == nil {
		t.Fatal("session was not adopted into the target repository")
	} else if state.WorktreePath != target.RepoDir {
		t.Errorf("adopted session records worktree %q, want the target %q", state.WorktreePath, target.RepoDir)
	}
}

// --force must not grant the ownership waiver through the real flag parser.
// Every in-process test constructs adoptOptions directly, so this is the only
// place the three flags' separateness is exercised end to end.
func TestSessionAdoptGuard_ForceDoesNotGrantForeignAdoption(t *testing.T) {
	t.Parallel()

	source := NewRepoWithCommit(t)
	target := NewRepoWithCommit(t)

	const sessionID = "integration-force-session"
	seedAdoptableSession(t, source.RepoDir, sessionID)

	forced, forcedErr := target.RunCLIWithError("session", "adopt", sessionID, "--from", source.RepoDir, "--force", "--yes")
	if forcedErr == nil {
		t.Fatalf("--force --yes waived the ownership check\nOutput: %s", forced)
	}
	if !strings.Contains(forced, "--allow-foreign-session") {
		t.Errorf("refusal should name the flag that does grant it\nOutput: %s", forced)
	}

	// And the flag that does grant it works, so the refusal above is about
	// ownership rather than something else failing.
	granted, grantedErr := target.RunCLIWithError("session", "adopt", sessionID,
		"--from", source.RepoDir, "--force", "--allow-foreign-session")
	if grantedErr != nil {
		t.Fatalf("--allow-foreign-session did not permit the adoption: %v\nOutput: %s", grantedErr, granted)
	}
	if !strings.Contains(granted, "Adopted session") {
		t.Errorf("expected an adoption confirmation, got: %s", granted)
	}
}
