//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6/plumbing"
)

// Session-end condensation writes a checkpoint to entire/checkpoints/v1 and only
// afterwards saves the session bookkeeping that records the write. A process
// death in that window (Codex SIGKILLs a hook that overruns its session-end
// budget) used to leave the transcript looking unfinished, so the next retry
// minted a second checkpoint ID over the same transcript range.
//
// These tests drive the real binary through that window. Everything up to the
// crash is real: real hooks, real shadow branch, real condensation, real
// checkpoint. The crash itself is forged with WriteSessionState, because the
// only alternative is a fault-injection kill point in shipped code (see the PR
// description). What is written back is exactly the state the reserving
// MutateSessionState persists — the pre-condense snapshot plus the pending
// attempt — so the retry starts from the same bytes a killed hook would leave.

// listStoredCheckpoints returns every checkpoint in the repo, backend-agnostic
// (the routing store unions git-branch and git-refs), so a test can assert on
// the count without pinning a backend.
func listStoredCheckpoints(t *testing.T, env *TestEnv) []checkpoint.CheckpointInfo {
	t.Helper()

	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatalf("open checkpoint stores: %v", err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	return infos
}

func assertCheckpointCount(t *testing.T, env *TestEnv, want int, why string) []checkpoint.CheckpointInfo {
	t.Helper()

	infos := listStoredCheckpoints(t, env)
	if len(infos) != want {
		ids := make([]string, 0, len(infos))
		for _, info := range infos {
			ids = append(ids, info.CheckpointID.String())
		}
		t.Fatalf("%s: got %d checkpoint(s) %v, want %d", why, len(infos), ids, want)
	}
	return infos
}

// preCondenseSnapshot is everything a hook killed between the durable
// checkpoint write and the bookkeeping save leaves on disk. The shadow branch
// matters as much as the state file: session-end condensation deletes the
// branch only *after* the state save (see the didCondense block at the end of
// CondenseAndMarkFullyCondensed), so a crash in the window leaves it standing.
// A forge that drops it sends `entire doctor` down the discard path instead of
// the retry path, and PrepareCommitMsg finds no content to link.
type preCondenseSnapshot struct {
	state        *strategy.SessionState
	shadowBranch string
	shadowTip    plumbing.Hash
}

// startEagerCondensableSession drives a real session to the state the eager
// session-end condense is built for: shadow-branch content (StepCount > 0) and
// no pending files. FilesTouched is cleared directly because
// CondenseAndMarkFullyCondensed skips any session that still has files —
// PostCommit owns those, for attribution. A review session and an
// IDLE-with-empty-FilesTouched turn both reach the same state naturally.
func startEagerCondensableSession(t *testing.T, env *TestEnv, file, content string) (*Session, preCondenseSnapshot) {
	t.Helper()

	sess := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}
	env.WriteFile(file, content)
	sess.CreateTranscript("Write "+file, []FileChange{{Path: file, Content: content}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("session state missing after stop")
	}
	if state.StepCount <= 0 {
		t.Fatalf("expected shadow-branch content after stop, got StepCount=%d", state.StepCount)
	}
	state.FilesTouched = nil
	if err := env.WriteSessionState(sess.ID, state); err != nil {
		t.Fatalf("WriteSessionState failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchNameForCommit(state.BaseCommit)
	return sess, preCondenseSnapshot{
		state:        state,
		shadowBranch: shadowBranch,
		shadowTip:    resolveBranchTip(t, env, shadowBranch),
	}
}

func resolveBranchTip(t *testing.T, env *TestEnv, branch string) plumbing.Hash {
	t.Helper()

	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve shadow branch %s: %v", branch, err)
	}
	return ref.Hash()
}

// forgeInterruptedCondensation puts the repo back into the crashed state: the
// pre-condense session state plus the reserved attempt ID, and the shadow branch
// the real condense went on to delete. Pass an empty reservedID for the
// pre-reservation (legacy) shape doctor has to reconcile from storage.
func forgeInterruptedCondensation(t *testing.T, env *TestEnv, sessionID string, snap preCondenseSnapshot, reservedID id.CheckpointID) {
	t.Helper()

	crashed := *snap.state
	crashed.Phase = session.PhaseEnded
	crashed.FullyCondensed = false
	crashed.LastCheckpointID = id.EmptyCheckpointID
	if reservedID.IsEmpty() {
		crashed.ClearCondensationAttempt()
	} else {
		crashed.BeginCondensationAttempt(reservedID)
	}
	if err := env.WriteSessionState(sessionID, &crashed); err != nil {
		t.Fatalf("WriteSessionState failed: %v", err)
	}

	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(snap.shadowBranch), snap.shadowTip)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("restore shadow branch %s: %v", snap.shadowBranch, err)
	}
}

// TestSessionEndCondensation_NormalFlowIsOneCheckpointPerSession is the control:
// the dedup machinery must not collapse two genuinely different sessions, and an
// uninterrupted session end must still clear its reservation.
func TestSessionEndCondensation_NormalFlowIsOneCheckpointPerSession(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	first, _ := startEagerCondensableSession(t, env, "first.go", "package main\n\nfunc First() {}\n")
	if err := env.SimulateSessionEnd(first.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	firstCheckpoints := assertCheckpointCount(t, env, 1, "after first session end")

	state, err := env.GetSessionState(first.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil {
		if !state.FullyCondensed {
			t.Error("session should be FullyCondensed after an uninterrupted session end")
		}
		if pendingID := state.PendingCondensationID(); !pendingID.IsEmpty() {
			t.Errorf("reservation should be cleared after a successful write, got %s", pendingID)
		}
		if state.LastCheckpointID != firstCheckpoints[0].CheckpointID {
			t.Errorf("LastCheckpointID = %s, want %s", state.LastCheckpointID, firstCheckpoints[0].CheckpointID)
		}
	}

	second, _ := startEagerCondensableSession(t, env, "second.go", "package main\n\nfunc Second() {}\n")
	if err := env.SimulateSessionEnd(second.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	infos := assertCheckpointCount(t, env, 2, "after a second, unrelated session end")
	if infos[0].CheckpointID == infos[1].CheckpointID {
		t.Fatal("two unrelated sessions must not share a checkpoint ID")
	}
}

// TestSessionEndCondensation_InterruptedWriteRetriedByDoctor is the headline
// regression test for the doctor retry path: `entire doctor --force`, run
// through the real binary, must reuse the interrupted checkpoint instead of
// minting a second one.
func TestSessionEndCondensation_InterruptedWriteRetriedByDoctor(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess, snap := startEagerCondensableSession(t, env, "feature.go", "package main\n\nfunc Feature() {}\n")

	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	written := assertCheckpointCount(t, env, 1, "after session end")[0].CheckpointID

	forgeInterruptedCondensation(t, env, sess.ID, snap, written)

	env.RunCLI("doctor", "--force")

	infos := assertCheckpointCount(t, env, 1, "doctor must not duplicate the interrupted checkpoint")
	if infos[0].CheckpointID != written {
		t.Errorf("doctor wrote checkpoint %s, want the interrupted %s", infos[0].CheckpointID, written)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil {
		if state.LastCheckpointID != written {
			t.Errorf("LastCheckpointID = %s, want %s", state.LastCheckpointID, written)
		}
		if pendingID := state.PendingCondensationID(); !pendingID.IsEmpty() {
			t.Errorf("reservation should be cleared after the retry, got %s", pendingID)
		}
	}
}

// TestSessionEndCondensation_UnrelatedCommitDoesNotLinkInterruptedSession pins
// why doctor is the retry path that matters. An interrupted eager condense
// leaves an ENDED session with no FilesTouched, and PrepareCommitMsg's content
// detection deliberately refuses to link that to a commit of unrelated files
// (stagedFilesOverlapWithContent has nothing to overlap). So the session is not
// swept up by the next commit, and it must not be duplicated either.
func TestSessionEndCondensation_UnrelatedCommitDoesNotLinkInterruptedSession(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess, snap := startEagerCondensableSession(t, env, "feature.go", "package main\n\nfunc Feature() {}\n")

	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	written := assertCheckpointCount(t, env, 1, "after session end")[0].CheckpointID

	forgeInterruptedCondensation(t, env, sess.ID, snap, written)

	env.WriteFile("unrelated.txt", "hand-written\n")
	env.GitCommitWithShadowHooksAsAgent("Unrelated hand-written change", "unrelated.txt")

	if trailer := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()); trailer != "" {
		t.Errorf("unrelated commit got checkpoint trailer %q, want none", trailer)
	}
	if infos := assertCheckpointCount(t, env, 1, "an unrelated commit must not add a checkpoint"); infos[0].CheckpointID != written {
		t.Errorf("checkpoint %s survived, want %s", infos[0].CheckpointID, written)
	}
}

// TestSessionEndCondensation_AgentCommitLeavesInterruptedSessionForDoctor pins
// what a commit does and does not do to an interrupted session, so that the
// division of labour is not changed by accident.
//
// PostCommit is not this session's retry path and never was: ENDED + GitCommit
// carries ActionCondenseIfFilesTouched, and an interrupted eager condense leaves
// no FilesTouched, so nothing is condensed under any checkpoint ID. The new
// reserved-ID guard in postCommitProcessSessionLocked therefore does not change
// the outcome here — it is `entire doctor` that recovers this session either way.
//
// What must hold is that the commit neither duplicates the interrupted
// checkpoint nor adopts its reserved ID for unrelated work: the live session
// gets its own checkpoint, and the reserved one is left intact for the retry.
func TestSessionEndCondensation_AgentCommitLeavesInterruptedSessionForDoctor(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// The interrupted session: real condense, then rewound to the crash state.
	interrupted, snap := startEagerCondensableSession(t, env, "feature.go", "package main\n\nfunc Feature() {}\n")
	if err := env.SimulateSessionEnd(interrupted.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	reserved := assertCheckpointCount(t, env, 1, "after session end")[0].CheckpointID
	forgeInterruptedCondensation(t, env, interrupted.ID, snap, reserved)

	// A second, live agent session commits its own work in the same worktree. It
	// stays ACTIVE and non-empty so tryAgentCommitFastPath actually fires.
	live := env.NewSession()
	const liveContent = "package main\n\nfunc Live() {}\n"
	env.WriteFile("live.go", liveContent)
	live.CreateTranscript("Write live.go", []FileChange{{Path: "live.go", Content: liveContent}})
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(live.ID, live.TranscriptPath); err != nil {
		t.Fatalf("live session user-prompt-submit failed: %v", err)
	}
	liveState, err := env.GetSessionState(live.ID)
	if err != nil {
		t.Fatalf("GetSessionState(live) failed: %v", err)
	}
	if liveState == nil || liveState.Phase != session.PhaseActive {
		t.Fatalf("live session must be ACTIVE for the agent fast path to fire, got %+v", liveState)
	}

	env.GitCommitWithShadowHooksAsAgent("Agent commit from the live session", "live.go")

	trailer := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if trailer == "" {
		t.Fatal("agent commit got no checkpoint trailer; the fast path did not fire")
	}
	if trailer == reserved.String() {
		t.Errorf("the live session's commit adopted the interrupted session's reserved ID %s; "+
			"unrelated work must not land in a checkpoint reserved for another transcript range", reserved)
	}

	// The reserved checkpoint must still be there, untouched, for doctor.
	infos := listStoredCheckpoints(t, env)
	reservedStillPresent := false
	for _, info := range infos {
		if info.CheckpointID == reserved {
			reservedStillPresent = true
		}
	}
	if !reservedStillPresent {
		t.Errorf("the reserved checkpoint %s disappeared; the retry has nothing to reuse", reserved)
	}
	if len(infos) != 2 {
		t.Errorf("got %d checkpoints, want 2 (the reserved one plus the live session's own)", len(infos))
	}

	state, err := env.GetSessionState(interrupted.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("the interrupted session state was deleted without being condensed")
	}
	if pendingID := state.PendingCondensationID(); pendingID != reserved {
		t.Errorf("reservation = %s, want %s preserved for the doctor retry",
			pendingID, reserved)
	}

	// And doctor still finishes the job afterwards, reusing the reserved ID.
	env.RunCLI("doctor", "--force")
	after := listStoredCheckpoints(t, env)
	if len(after) != 2 {
		t.Errorf("doctor produced %d checkpoints, want 2 — it must reuse the reserved one", len(after))
	}
}

// TestSessionEndCondensation_ReservedRetryKeepsAdvancedContent is the negative
// control for reuse. Reusing a reserved checkpoint ID is only safe if the retry
// still stores everything the session has produced by then: if the session came
// back and did more work after the interrupted write, the reused checkpoint must
// carry the newer transcript, not the stale range it was reserved for.
//
// One checkpoint is the right answer here rather than two — the reservation is a
// promise that this ID still owes a write, and the retry's range is a superset,
// so the two would-be checkpoints merge instead of duplicating. The unreserved
// (legacy) equivalent, where reconciliation has to judge from stored content
// alone, mints a second checkpoint instead and is covered by
// TestCondenseSessionByID_DoesNotReuseCheckpointAfterSessionAdvances.
func TestSessionEndCondensation_ReservedRetryKeepsAdvancedContent(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess, snap := startEagerCondensableSession(t, env, "feature.go", "package main\n\nfunc Feature() {}\n")

	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	reserved := assertCheckpointCount(t, env, 1, "after session end")[0].CheckpointID

	forgeInterruptedCondensation(t, env, sess.ID, snap, reserved)

	// The session comes back and does more work beyond the interrupted range.
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("second user-prompt-submit failed: %v", err)
	}
	const advancedMarker = "Add the More helper"
	advanced := "package main\n\nfunc Feature() {}\n\nfunc More() {}\n"
	env.WriteFile("feature.go", advanced)
	sess.CreateTranscript(advancedMarker, []FileChange{{Path: "feature.go", Content: advanced}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("second stop failed: %v", err)
	}

	// Back to the eager-condense precondition, then end the session for real.
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	state.FilesTouched = nil
	if err := env.WriteSessionState(sess.ID, state); err != nil {
		t.Fatalf("WriteSessionState failed: %v", err)
	}
	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("second SimulateSessionEnd failed: %v", err)
	}

	infos := assertCheckpointCount(t, env, 1, "the reserved retry must reuse its checkpoint")
	if infos[0].CheckpointID != reserved {
		t.Fatalf("checkpoint %s survived, want the reserved %s", infos[0].CheckpointID, reserved)
	}

	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatalf("open checkpoint stores: %v", err)
	}
	content, err := stores.Persistent.ReadSessionContent(context.Background(), reserved, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent: %v", err)
	}
	if !strings.Contains(string(content.Transcript), advancedMarker) {
		t.Errorf("reused checkpoint %s does not carry the work done after the interrupted write", reserved)
	}
}
