//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// =============================================================================
// Single-remote checkpoint sync (ENT-1451)
//
// Checkpoint data syncs to exactly one elected git remote:
// strategy_options.checkpoint_push_remote (fail-closed when the named remote
// is not configured) -> origin -> sole remote -> first
// remote in .git/config order. The branch's tracking config deliberately does
// not participate; see TestCheckpointSyncRemote_BranchTrackingDoesNotReroute.
// The pre-push gate drops checkpoint sync for
// every other remote and for raw-URL pushes, on both backends. The dedicated
// checkpoint_remote URL mode is exempt. These are end-to-end acceptance tests
// through the simulated hook flow; the election precedence itself is
// unit-tested in strategy/checkpoint_sync_remote_test.go.
// =============================================================================

// remoteTarget names a configured remote and the bare repo backing it.
type remoteTarget struct {
	name    string
	bareDir string
}

// queuedCheckpointRefCount returns the git-refs push-discovery queue length
// for the test repo. The queue lives in the git common dir, which for the
// plain (non-worktree) TestEnv repos is .git.
func queuedCheckpointRefCount(t *testing.T, env *TestEnv) int {
	t.Helper()
	refs, err := checkpoint.NewPushQueue(filepath.Join(env.RepoDir, ".git")).Peek()
	if err != nil {
		t.Fatalf("read push queue: %v", err)
	}
	return len(refs)
}

// assertSingleRemoteRouting drives the pre-push flow first against the
// non-elected remote (no checkpoint data may land there; on git-refs the push
// queue must survive), then against the elected remote (the checkpoint lands
// and the queue drains). Shared by the default-election and config-override
// scenarios, which are mirror images of each other.
//
// expectGatedHint asserts the gated pre-push's output: an automatic election
// must tell the user their checkpoints are waiting for the elected remote and
// name checkpoint_push_remote (the gate was previously a silent no-op —
// checkpoints stranded locally with no signal), while an explicit
// checkpoint_push_remote is a decision already made and must stay quiet.
func assertSingleRemoteRouting(t *testing.T, env *TestEnv, checkpointID string, gated, elected remoteTarget, expectGatedHint bool) {
	t.Helper()

	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	if !env.CheckpointsPresentLocally() {
		t.Fatal("checkpoints should exist locally after condensation")
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
		t.Fatal("git-refs push queue should have entries after condensation")
	}

	// Pre-push to the non-elected remote: gated, no checkpoint data escapes.
	gatedOutput := env.RunPrePushOutput(gated.name)
	if hinted := strings.Contains(gatedOutput, "checkpoint_push_remote"); hinted != expectGatedHint {
		t.Errorf("gated pre-push hint presence = %v, want %v; output:\n%s", hinted, expectGatedHint, gatedOutput)
	}
	if expectGatedHint && !strings.Contains(gatedOutput, fmt.Sprintf("%q", elected.name)) {
		t.Errorf("gated pre-push hint should name the elected remote %q; output:\n%s", elected.name, gatedOutput)
	}
	if env.CheckpointsPresentOnRemote(gated.bareDir) {
		t.Errorf("checkpoints should NOT be on non-elected remote %q", gated.name)
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
		t.Error("push queue should be preserved after a gated pre-push")
	}

	// Pre-push to the elected remote: checkpoint data lands, queue drains.
	env.RunPrePush(elected.name)
	if !env.CheckpointExistsOnRemote(elected.bareDir, checkpointID) {
		t.Errorf("checkpoint %s should be on elected remote %q", checkpointID, elected.name)
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) != 0 {
		t.Error("push queue should drain after pushing to the elected remote")
	}
	if env.CheckpointsPresentOnRemote(gated.bareDir) {
		t.Errorf("non-elected remote %q must never receive checkpoint data", gated.name)
	}
}

// setBranchTrackingRemote points the current branch's tracking remote at
// remoteName. SetupNamedBareRemote pushes with `-u`, so the last remote set up
// becomes the branch's declared push destination — and a pre-push to a
// declared destination legitimately captures the election. Tests whose
// scenario needs an UNDECLARED gated remote must undo that side effect.
func setBranchTrackingRemote(t *testing.T, env *TestEnv, remoteName string) {
	t.Helper()
	branch := env.GetCurrentBranch()
	if branch == "" {
		t.Fatal("cannot set tracking remote: no current branch")
	}
	testutil.RunGit(t, env.RepoDir, "config", "branch."+branch+".remote", remoteName)
	// The edit is deliberate test setup, not drift — refresh the baseline the
	// harness compares .git/config against at teardown.
	env.setGitConfigBaseline()
}

// forkRemote is the non-origin remote these tests push to — the fork topology
// that motivated the captured election.
const forkRemote = "fork"

// setRemoteURL repoints a configured remote. Used to take a remote dark
// mid-test by aiming it at a path that does not exist, which fails the transfer
// while leaving the remote configured and its tracking refs in place.
func setRemoteURL(t *testing.T, env *TestEnv, remote, url string) {
	t.Helper()
	testutil.RunGit(t, env.RepoDir, "remote", "set-url", remote, url)
	// Deliberate test setup, not drift — refresh the teardown baseline.
	env.setGitConfigBaseline()
}

// capturedSyncRemotesOnDisk reads the captured-election state file for the
// test repo; nil when no capture has happened.
func capturedSyncRemotesOnDisk(t *testing.T, env *TestEnv) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "entire-checkpoint-sync-remotes.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read captured sync remotes: %v", err)
	}
	var f struct {
		Remotes []string `json:"remotes"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse captured sync remotes: %v", err)
	}
	return f.Remotes
}

// TestCheckpointSyncRemote_DefaultElection_OnlyOriginReceivesCheckpoints
// verifies that with two configured remotes and no override, checkpoint data
// syncs only to origin (the default election): a pre-push for "publish"
// carries nothing, a pre-push for "origin" delivers the checkpoint. The
// branch's tracking is reset to origin so publish stays an undeclared one-off
// destination — the shape that must never sync or capture.
func TestCheckpointSyncRemote_DefaultElection_OnlyOriginReceivesCheckpoints(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")
		setBranchTrackingRemote(t, env, "origin")
		checkpointID := createCheckpointedCommit(t, env, "Add gate module", "gate.go", "package gate", "Add gate module")

		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "publish", bareDir: barePublish},
			remoteTarget{name: "origin", bareDir: bareOrigin},
			false)

		if got := capturedSyncRemotesOnDisk(t, env); got != nil {
			t.Errorf("an undeclared one-off push must not capture the election, got %v", got)
		}
	})
}

// TestCheckpointSyncRemote_ConfigOverride_RoutesToNamedRemote verifies the
// mirror image: with checkpoint_push_remote set to "publish", origin is the
// gated remote and publish receives the checkpoint data.
func TestCheckpointSyncRemote_ConfigOverride_RoutesToNamedRemote(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "publish",
			},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add router module", "router.go", "package router", "Add router module")

		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "origin", bareDir: bareOrigin},
			remoteTarget{name: "publish", bareDir: barePublish},
			false)

		// Status reflects the override election.
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "publish" || st.CheckpointSyncRemoteSource != "config" {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				"publish", "config", st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_TrackingAloneDoesNotReroute pins the regression
// that removed the static tracking tier from the election (74e239a9): a
// branch tracking a non-origin remote must NOT move checkpoint sync there
// while that remote never receives a push. Electing from config at rest made
// every push to a different remote a silent no-op — caught by
// TestAlternates_RelativeObjectAlternate_CheckpointSync, which clones with
// `-o base` and pushes checkpoints to a separately added origin. Capture must
// not resurrect that bug: declaration without a confirming push elects
// nothing.
func TestCheckpointSyncRemote_TrackingAloneDoesNotReroute(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		// SetupNamedBareRemote pushes with `-u`, so the branch ends up
		// tracking "base" — but base never sees a pre-push. Origin stays
		// elected and keeps receiving checkpoints.
		bareOrigin := env.SetupBareRemote()
		bareBase := env.SetupNamedBareRemote("base")

		checkpointID := createCheckpointedCommit(t, env, "Add fork module", "fork.go", "package fork", "Add fork module")

		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Errorf("checkpoint %s should reach origin while the tracked remote is never pushed", checkpointID)
		}
		if env.CheckpointsPresentOnRemote(bareBase) {
			t.Error("the tracked-but-never-pushed remote must not receive checkpoint data")
		}
		if got := capturedSyncRemotesOnDisk(t, env); got != nil {
			t.Errorf("tracking config alone must not capture the election, got %v", got)
		}

		const wantRemote, wantSource = "origin", "default"
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != wantRemote || st.CheckpointSyncRemoteSource != wantSource {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				wantRemote, wantSource, st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_TrackedPushCapturesElection verifies the capture
// path end to end — the fork topology that produced the first user report
// after v0.10.0 ("the remote I push to isn't called origin → checkpoints no
// longer push"): origin exists but the branch pushes its fork. The pre-push
// to the declared destination captures the election and the SAME push carries
// the checkpoint; origin never receives data and a later origin push is
// gated. Status reports the captured source.
func TestCheckpointSyncRemote_TrackedPushCapturesElection(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		bareFork := env.SetupNamedBareRemote(forkRemote) // `-u`: branch declares fork

		checkpointID := createCheckpointedCommit(t, env, "Add capture module", "capture.go", "package capture", "Add capture module")

		env.RunPrePush(forkRemote)
		if got := capturedSyncRemotesOnDisk(t, env); len(got) != 1 || got[0] != forkRemote {
			t.Errorf("declared-destination push should capture the election, got %v", got)
		}
		if !env.CheckpointExistsOnRemote(bareFork, checkpointID) {
			t.Errorf("checkpoint %s should land on the captured remote in the capturing push", checkpointID)
		}
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) != 0 {
			t.Error("push queue should drain into the captured remote")
		}

		// The election moved: origin is now the gated remote.
		env.RunPrePush("origin")
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Error("origin must not receive checkpoint data after the election was captured")
		}

		const wantRemote, wantSource = forkRemote, "observed"
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != wantRemote || st.CheckpointSyncRemoteSource != wantSource {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				wantRemote, wantSource, st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_DeferredPushDoesNotCaptureElection covers the
// gate-to-delivery gap on the most likely first push there is: the user adds a
// brand-new empty fork and pushes their branch to it for the first time. That
// push declares fork AND is the one the empty-remote defer suppresses, because
// publishing entire/checkpoints/v1 to a remote with no branches would let a
// forge adopt it as the default branch.
//
// So the election must not move: capturing here announced a destination that
// received nothing, and since the election is permanent and one-shot the
// checkpoints could afterwards only drain to fork — which is still empty and
// still deferring. The next push, once the user's branch exists on fork, is the
// one that both delivers and captures.
//
// git-branch only: the defer protects a refs/heads branch, and the git-refs
// backend publishes under refs/entire/, which no forge will adopt.
func TestCheckpointSyncRemote_DeferredPushDoesNotCaptureElection(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareOrigin := env.SetupBareRemote()
	// A fork that exists in config but has never been fetched or pushed: no
	// tracking refs, which is what the defer reads as "empty".
	freshFork := initUnregisteredBareRepo(t)
	testutil.AddRemote(t, env.RepoDir, forkRemote, freshFork)
	env.setGitConfigBaseline()
	setBranchTrackingRemote(t, env, forkRemote)

	checkpointID := createCheckpointedCommit(t, env, "Add deferred module", "deferred.go", "package deferred", "Add deferred module")

	env.RunPrePush(forkRemote)

	if got := capturedSyncRemotesOnDisk(t, env); got != nil {
		t.Errorf("a push whose checkpoint delivery was deferred must not capture the election, got %v", got)
	}
	if env.CheckpointsPresentOnRemote(freshFork) {
		t.Error("the defer must hold: no checkpoint data may reach the empty fork")
	}

	// The election is still the default, so it remains available to whichever
	// push first actually delivers.
	const wantRemote, wantSource = "origin", "default"
	st := statusSyncJSONOutput(t, env)
	if st.CheckpointSyncRemote != wantRemote || st.CheckpointSyncRemoteSource != wantSource {
		t.Errorf(`status should still report origin from "default", got %q from %q`,
			st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
	}

	// Publish the user's branch to fork, which retires the defer, and push
	// again: this push delivers, so this is the push that captures.
	env.GitPush(forkRemote, env.GetCurrentBranch())
	env.RunPrePush(forkRemote)

	if got := capturedSyncRemotesOnDisk(t, env); len(got) != 1 || got[0] != forkRemote {
		t.Errorf("the first push that actually delivers should capture the election, got %v", got)
	}
	if !env.CheckpointExistsOnRemote(freshFork, checkpointID) {
		t.Errorf("checkpoint %s should reach fork once the defer no longer applies", checkpointID)
	}
	if env.CheckpointsPresentOnRemote(bareOrigin) {
		t.Error("origin must never receive checkpoint data in this flow")
	}
}

// TestCheckpointSyncRemote_RejectedPushDoesNotCaptureElection covers the other
// end of the gate-to-delivery gap: the push is attempted and the remote refuses
// it. Unlike the deferred case this reaches the network, so it exercises each
// backend's own delivery point — the git-branch ref-push loop and the git-refs
// queue flush — and the git-refs flush is fail-soft, so its error path would
// otherwise never be asserted against capture at all.
//
// Capturing here was the worst shape of the old behavior: the election moved to
// a remote that had just refused the data, and because it is one-shot the
// checkpoints could then only ever be retried against that same remote.
func TestCheckpointSyncRemote_RejectedPushDoesNotCaptureElection(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		// `-u`: the branch declares fork, and this push leaves tracking refs
		// behind — which retires the empty-remote defer, so the flow gets past
		// it and actually attempts the transfer.
		bareFork := env.SetupNamedBareRemote(forkRemote)

		checkpointID := createCheckpointedCommit(t, env, "Add rejected module", "rejected.go", "package rejected", "Add rejected module")

		// fork goes dark only now, after its tracking refs exist.
		setRemoteURL(t, env, forkRemote, env.RepoDir+"/nonexistent-remote")

		// git-branch surfaces the failed checkpoint delivery to the caller;
		// git-refs is fail-soft and leaves the refs queued. Either is fine here
		// — what must hold is that neither moved the election.
		if err := env.RunPrePushWithError(forkRemote); err != nil {
			t.Logf("pre-push reported the failed checkpoint delivery: %v", err)
		}

		if got := capturedSyncRemotesOnDisk(t, env); got != nil {
			t.Errorf("a push whose checkpoint delivery was rejected must not capture the election, got %v", got)
		}
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Error("the gated remote must not receive checkpoint data")
		}

		// Still the default election, so the checkpoints are not trapped: they
		// remain routable once the user fixes the remote.
		const wantRemote, wantSource = "origin", "default"
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != wantRemote || st.CheckpointSyncRemoteSource != wantSource {
			t.Errorf("status should still report %q from %q, got %q from %q",
				wantRemote, wantSource, st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}

		// Repair the remote and push again: the retry both delivers and captures,
		// so one rejected push costs the user nothing permanent.
		setRemoteURL(t, env, forkRemote, bareFork)
		env.RunPrePush(forkRemote)

		if got := capturedSyncRemotesOnDisk(t, env); len(got) != 1 || got[0] != forkRemote {
			t.Errorf("the retry that delivers should capture the election, got %v", got)
		}
		if !env.CheckpointExistsOnRemote(bareFork, checkpointID) {
			t.Errorf("checkpoint %s should reach fork on the successful retry", checkpointID)
		}
	})
}

// TestCheckpointSyncRemote_EmptyPushDoesNotCaptureElection covers the third way
// a push can deliver nothing: there was nothing to deliver. The branch declares
// the fork and the push succeeds, but no checkpoint exists yet, so the ref set /
// queue is empty and no checkpoint data reaches the remote.
//
// Capturing here is SAFE — nothing is stranded when there was nothing to strand
// — but it announces "Checkpoints now sync to fork" on a push that moved no
// data, which is the class of claim this path exists to stop making. So the
// election waits for the push that carries a checkpoint. Trail finding
// 01M0AK6PZCWE.
func TestCheckpointSyncRemote_EmptyPushDoesNotCaptureElection(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		env.SetupBareRemote()
		bareFork := env.SetupNamedBareRemote(forkRemote) // `-u`: the branch declares fork

		// No createCheckpointedCommit: there is deliberately nothing to sync.
		env.RunPrePush(forkRemote)

		if got := capturedSyncRemotesOnDisk(t, env); got != nil {
			t.Errorf("a push with no checkpoint to carry must not capture the election, got %v", got)
		}
		if env.CheckpointsPresentOnRemote(bareFork) {
			t.Error("no checkpoint data should exist on the remote")
		}

		// And the election is still available to the first push that does carry one.
		checkpointID := createCheckpointedCommit(t, env, "Add later module", "later.go", "package later", "Add later module")
		env.RunPrePush(forkRemote)

		if got := capturedSyncRemotesOnDisk(t, env); len(got) != 1 || got[0] != forkRemote {
			t.Errorf("the first push carrying a checkpoint should capture, got %v", got)
		}
		if !env.CheckpointExistsOnRemote(bareFork, checkpointID) {
			t.Errorf("checkpoint %s should reach fork on that push", checkpointID)
		}
	})
}

// TestCheckpointSyncRemote_MisconfiguredSettingFailsClosed verifies that when
// checkpoint_push_remote names a remote that is not configured, checkpoint
// sync is disabled for every remote (fail-closed) while the user's own push
// flow keeps working: the real pre-push hook exits zero and the branch push
// succeeds.
func TestCheckpointSyncRemote_MisconfiguredSettingFailsClosed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "gone",
			},
		})

		_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if !env.CheckpointsPresentLocally() {
			t.Fatal("checkpoints should exist locally after condensation")
		}

		// The user's own push must succeed with the real pre-push hook
		// installed — the misconfiguration silently skips checkpoint sync
		// but never breaks the push itself.
		env.GitPushWithHooks("origin", "HEAD")
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Error("checkpoints should NOT reach origin when checkpoint_push_remote is misconfigured")
		}

		if err := env.RunPrePushWithError("publish"); err != nil {
			t.Errorf("pre-push must not fail on a fail-closed misconfiguration: %v", err)
		}
		if env.CheckpointsPresentOnRemote(barePublish) {
			t.Error("checkpoints should NOT reach publish when checkpoint_push_remote is misconfigured")
		}

		// git-refs: the refs stay queued for whenever the setting is fixed.
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
			t.Error("push queue should be preserved while checkpoint sync is fail-closed")
		}

		// Status is the user's signal that sync is silently disabled: the
		// fail-closed error names the setting and no remote is elected.
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncError == "" || !strings.Contains(st.CheckpointSyncError, "checkpoint_push_remote") {
			t.Errorf("checkpoint_sync_error should mention checkpoint_push_remote, got %q", st.CheckpointSyncError)
		}
		if st.CheckpointSyncRemote != "" {
			t.Errorf("checkpoint_sync_remote should be empty when fail-closed, got %q", st.CheckpointSyncRemote)
		}
	})
}

// TestCheckpointSyncRemote_RawURLPushCarriesNoCheckpointData verifies that a
// push whose destination is a raw path/URL (git passes it verbatim as the
// hook's remote arg, and no such destination can be the elected remote) never
// carries checkpoint data — and that the elected remote still receives it on
// the next push.
func TestCheckpointSyncRemote_RawURLPushCarriesNoCheckpointData(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		rawDir := initUnregisteredBareRepo(t)

		checkpointID := createCheckpointedCommit(t, env, "Add worker module", "worker.go", "package worker", "Add worker module")

		env.RunPrePush(rawDir)
		if env.CheckpointsPresentOnRemote(rawDir) {
			t.Error("checkpoints should NOT land on a raw-URL push destination")
		}
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
			t.Error("push queue should be preserved after a raw-URL push")
		}

		// The elected remote still receives the checkpoint afterwards.
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Errorf("checkpoint %s should reach origin after the raw-URL push was gated", checkpointID)
		}
	})
}

// TestCheckpointSyncRemote_StatusReportsDestinationAndUnpushed verifies that
// after a gated push (nothing synced) `entire status` names the elected
// destination and counts the unpushed checkpoint, in both text and --json,
// and that the counter clears once the elected remote receives the data.
func TestCheckpointSyncRemote_StatusReportsDestinationAndUnpushed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		_ = env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("publish")
		setBranchTrackingRemote(t, env, "origin")
		_ = createCheckpointedCommit(t, env, "Add status module", "status.go", "package status", "Add status module")

		// Gated push: publish is neither elected nor the branch's declared
		// destination, so nothing syncs and nothing captures.
		env.RunPrePush("publish")

		text := env.RunCLI("status")
		if !strings.Contains(text, "Checkpoints sync to: origin") {
			t.Errorf("status should name the elected sync remote, got:\n%s", text)
		}
		if !strings.Contains(text, "not yet on origin") {
			t.Errorf("status should show an unpushed counter after a gated push, got:\n%s", text)
		}

		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "origin" {
			t.Errorf("checkpoint_sync_remote = %q, want %q", st.CheckpointSyncRemote, "origin")
		}
		if st.CheckpointSyncRemoteSource != "default" {
			t.Errorf("checkpoint_sync_remote_source = %q, want %q", st.CheckpointSyncRemoteSource, "default")
		}
		if st.UnpushedCheckpoints < 1 {
			t.Errorf("unpushed_checkpoints = %d, want >= 1", st.UnpushedCheckpoints)
		}

		// Push to the elected remote: the counter clears.
		env.RunPrePush("origin")

		text = env.RunCLI("status")
		if !strings.Contains(text, "Checkpoints sync to: origin") {
			t.Errorf("status should still name the sync remote after pushing, got:\n%s", text)
		}
		if strings.Contains(text, "not yet on origin") {
			t.Errorf("unpushed counter should clear after pushing to origin, got:\n%s", text)
		}

		st = statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "origin" {
			t.Errorf("checkpoint_sync_remote = %q after push, want %q", st.CheckpointSyncRemote, "origin")
		}
		if st.UnpushedCheckpoints != 0 {
			t.Errorf("unpushed_checkpoints = %d after push, want 0", st.UnpushedCheckpoints)
		}
	})
}

// TestCheckpointSyncRemote_DedicatedCheckpointRemoteExemptFromGate verifies
// the one exemption from the single-remote gate: a dedicated checkpoint_remote
// URL is a metadata store addressed directly, so a push to a non-elected
// remote still syncs checkpoint data — to the dedicated store, and only there.
//
// git-branch only: checkpoint_remote URL routing for git-refs per-checkpoint
// refs is separate future work (test plan B5), same scoping as
// TestHTTPS_CheckpointRemoteRoutesToSeparateRepo. The URL is derived from the
// push remote's HTTPS URL, so this reuses the smart-HTTP fixture.
// TestCheckpointSyncRemote_InheritedCheckpointRemoteDoesNotBypassGate is the
// end-to-end guard for the "cloned the base repo, added my fork" contributor
// topology.
//
// It matters at this level because of how the pre-push gate is written:
//
//	if !ps.hasCheckpointURL() && !checkpointSyncAllowedForRemote(ctx, ps.remote)
//
// A dedicated checkpoint_remote SKIPS the gate. So while the ownership check
// accepted an inherited committed setting, a contributor pushing to their own
// fork sailed straight past the gate and delivered their session transcripts to
// the upstream project's checkpoint repo. Ownership now also requires the push
// destination's owner to match, which makes hasCheckpointURL false here and lets
// the gate do its job.
func TestCheckpointSyncRemote_InheritedCheckpointRemoteDoesNotBypassGate(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "acme/app", "contributor/app", "acme/checkpoints")
	env := NewFeatureBranchEnv(t)

	upstreamBare := srv.BareDirs["acme/app"]
	forkBare := srv.BareDirs["contributor/app"]
	checkpointBare := srv.BareDirs["acme/checkpoints"]

	// origin is the UPSTREAM base repo the contributor cloned; myfork is their
	// own. Note origin's owner ("acme") matches the checkpoint repo's owner,
	// which is precisely why an origin-only ownership check was fooled.
	seedBareRepo(t, env, upstreamBare, srv.URL+"/acme/app.git")
	testutil.AddRemote(t, env.RepoDir, "myfork", srv.URL+"/contributor/app.git")
	env.setGitConfigBaseline()
	env.ExtraEnv = srv.tokenEnv("fork-provenance-token")

	// The setting is committed in .entire/settings.json — i.e. inherited by
	// cloning, not chosen by this developer.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "acme/checkpoints",
			},
		},
	})

	_ = createCheckpointedCommit(t, env, "Add contrib module", "contrib.go", "package contrib", "Add contrib module")

	// The contributor's actual workflow: push to their fork. myfork is not the
	// elected sync remote (origin wins the default election), so with the
	// inherited setting correctly ignored the gate drops checkpoint sync
	// entirely — nothing reaches upstream's checkpoint repo.
	env.RunPrePush("myfork")

	if env.BranchExistsOnRemote(checkpointBare, paths.MetadataBranchName) {
		t.Error("upstream's checkpoint repo must not receive a contributor's session data from an inherited setting")
	}
	if env.BranchExistsOnRemote(upstreamBare, paths.MetadataBranchName) {
		t.Error("upstream must not receive the checkpoint branch")
	}
	if env.BranchExistsOnRemote(forkBare, paths.MetadataBranchName) {
		t.Error("the fork is not the elected remote, so the gate must drop checkpoint sync there too")
	}
}

func TestCheckpointSyncRemote_DedicatedCheckpointRemoteExemptFromGate(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "testorg/main-repo", "testorg/publish-repo", "testorg/checkpoints")
	env := NewFeatureBranchEnv(t)

	mainBare := srv.BareDirs["testorg/main-repo"]
	publishBare := srv.BareDirs["testorg/publish-repo"]
	checkpointBare := srv.BareDirs["testorg/checkpoints"]

	// origin -> main repo over HTTPS; publish -> a second HTTPS remote that is
	// NOT the elected sync remote (origin wins the default election).
	seedBareRepo(t, env, mainBare, srv.URL+"/testorg/main-repo.git")
	testutil.AddRemote(t, env.RepoDir, "publish", srv.URL+"/testorg/publish-repo.git")
	env.setGitConfigBaseline()
	env.ExtraEnv = srv.tokenEnv("gate-exemption-token")

	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "testorg/checkpoints",
			},
		},
	})

	checkpointID := createCheckpointedCommit(t, env, "Add dedicated module", "dedicated.go", "package dedicated", "Add dedicated module")

	// Pre-push for the non-elected remote: the dedicated URL exemption applies,
	// so the checkpoint syncs to the dedicated store. The gated-push hint must
	// stay absent — the checkpoints DID sync, and telling a dedicated-mode
	// user to set checkpoint_push_remote would break their setup.
	if out := env.RunPrePushOutput("publish"); strings.Contains(out, "checkpoint_push_remote") {
		t.Errorf("dedicated checkpoint_remote mode must not show the gated-push hint; output:\n%s", out)
	}

	if !env.BranchExistsOnRemote(checkpointBare, paths.MetadataBranchName) {
		t.Fatal("dedicated checkpoint store should have the checkpoint branch")
	}
	if !fileExistsOnRemoteBranch(t, checkpointBare, CheckpointSummaryPath(checkpointID)) {
		t.Errorf("checkpoint %s should be on the dedicated checkpoint store", checkpointID)
	}
	if env.BranchExistsOnRemote(publishBare, paths.MetadataBranchName) {
		t.Error("the pushed-to remote must not receive the checkpoint branch")
	}
	if env.BranchExistsOnRemote(mainBare, paths.MetadataBranchName) {
		t.Error("origin must not receive the checkpoint branch in dedicated mode")
	}
}

// initUnregisteredBareRepo creates a bare repo that is deliberately NOT added
// as a remote of any test repo, for raw-URL push scenarios.
func initUnregisteredBareRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.RunGit(t, dir, "init", "--bare")
	return dir
}

// statusSyncJSON is the checkpoint-sync subset of `entire status --json`.
type statusSyncJSON struct {
	CheckpointSyncRemote       string `json:"checkpoint_sync_remote"`
	CheckpointSyncRemoteSource string `json:"checkpoint_sync_remote_source"`
	CheckpointSyncError        string `json:"checkpoint_sync_error"`
	UnpushedCheckpoints        int    `json:"unpushed_checkpoints"`
}

// statusSyncJSONOutput runs `entire status --json` and parses stdout (stderr
// is kept separate so hints can't corrupt the JSON).
func statusSyncJSONOutput(t *testing.T, env *TestEnv) statusSyncJSON {
	t.Helper()

	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "status", "--json")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status --json failed: %v\nStderr: %s", err, stderr.String())
	}

	var parsed statusSyncJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse status --json: %v\nOutput: %s", err, out)
	}
	return parsed
}
