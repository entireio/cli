//go:build integration

package integration

import (
	"testing"
)

// TestGitPushWithHooks_SyncsCheckpointsToRemote is test A1: a plain `git push` of
// a feature branch, running the installed pre-push hook exactly as git runs it
// (realistic stdin refspecs, remote name/URL argv), lands the committed
// checkpoints on the bare remote WITHOUT any explicit RunPrePush or
// PushCheckpointRefs — and the user push itself is unaffected (the feature branch
// arrives). It runs under both checkpoint backends via ForEachBackend, validating
// the whole I-1/I-2 enabler stack: env injection selects the store, the real hook
// drains it, and the backend-aware assertion finds the result.
func TestGitPushWithHooks_SyncsCheckpointsToRemote(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupBareRemote()

		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		// Sanity: checkpoint exists locally under the selected backend.
		if !env.CheckpointsPresentLocally() {
			t.Fatalf("[%s] checkpoint should exist locally after condensation", backend)
		}

		// Plain push through the real hook — no explicit checkpoint push.
		env.GitPushWithHooks("origin", "HEAD")

		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[%s] checkpoints should be on remote after `git push` via the real pre-push hook", backend)
		}
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("[%s] checkpoint %s should be on remote after `git push` via the real pre-push hook", backend, checkpointID)
		}

		// The user's own push is unaffected: the feature branch itself arrives on
		// the remote alongside the checkpoints.
		if !env.BranchExistsOnRemote(bareDir, "feature/test-branch") {
			t.Fatalf("[%s] user feature branch should arrive on remote via the same push", backend)
		}
	})
}

// TestGitPushWithHooks_NoNewCheckpointsIsNoOp is test A2: once checkpoints are
// synced, a later push carrying only a plain (non-checkpointed) commit runs the
// real hook, but with nothing new to push the remote checkpoint state is
// byte-for-byte unchanged and the user push still succeeds.
func TestGitPushWithHooks_NoNewCheckpointsIsNoOp(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupBareRemote()

		// First push syncs the checkpoint to the remote.
		_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		env.GitPushWithHooks("origin", "HEAD")
		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[%s] checkpoints should be on remote after the first push", backend)
		}

		stateBefore := env.RemoteCheckpointState(bareDir)

		// A plain commit with no session activity, then push again through the real
		// hook. The hook runs but finds nothing new to sync.
		env.WriteFile("README.md", "# updated")
		env.GitAdd("README.md")
		env.GitCommit("Docs tweak (no session)")
		env.GitPushWithHooks("origin", "HEAD")

		stateAfter := env.RemoteCheckpointState(bareDir)
		if stateBefore != stateAfter {
			t.Errorf("[%s] remote checkpoint state should be unchanged by a push with no new checkpoints:\nbefore=%s\nafter=%s",
				backend, stateBefore, stateAfter)
		}
	})
}

// TestGitPushWithHooks_DeleteAndTagPushesAreNoOp is test A3: a branch deletion
// (`git push --delete`, whose stdin carries a zero-sha line) and a tag-only push
// run through the real pre-push hook without crashing and exit 0. With no local
// checkpoints there is nothing to sync, so the remote checkpoint state stays
// empty — the unusual push shapes don't trigger a spurious checkpoint push.
func TestGitPushWithHooks_DeleteAndTagPushesAreNoOp(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupBareRemote()

		// Seed a throwaway branch and a tag on the remote (setup plumbing, hook
		// bypassed) so we have something to delete and a tag to push.
		env.GitCheckoutNewBranch("to-delete")
		env.WriteFile("scratch.txt", "scratch")
		env.GitAdd("scratch.txt")
		env.GitCommit("scratch commit")
		env.GitPush("origin", "to-delete")
		env.GitCheckoutNewBranch("feature/back")

		// Branch delete through the real hook — stdin carries a zero-sha refspec.
		out, err := env.GitPushArgsWithHooks("origin", "--delete", "to-delete")
		if err != nil {
			t.Fatalf("[%s] `git push --delete` via the real hook should exit 0, got: %v\n%s", backend, err, out)
		}

		// Tag-only push through the real hook — stdin carries a tag refspec.
		tagName := "v0.0.1"
		env.GitTag(tagName)
		out, err = env.GitPushArgsWithHooks("origin", "refs/tags/"+tagName)
		if err != nil {
			t.Fatalf("[%s] tag-only push via the real hook should exit 0, got: %v\n%s", backend, err, out)
		}

		// No sessions ran, so no checkpoint push should have been attempted.
		if env.CheckpointsPresentOnRemote(bareDir) {
			t.Errorf("[%s] no checkpoints should be on remote after delete/tag pushes (none were created)", backend)
		}
	})
}

// TestGitPushWithHooks_UnreachableCheckpointRemoteContinues is test A4, the
// real-hook variant of graceful degradation: when checkpoints are routed to a
// genuinely unreachable target, the user's push must still succeed.
//
// A local-path origin can't express this — checkpoint_remote URL derivation fails
// on a file path and silently falls back to origin (which is reachable). So this
// runs over the in-process HTTPS server: origin is the (existing) main-repo, while
// checkpoint_remote points at a sibling repo that does NOT exist on the server.
// The CLI's checkpoint push (token-authenticated) fails at the missing repo and
// degrades gracefully; the user's own push, authenticated via credentials
// embedded in the origin URL, lands the feature branch on main-repo.
func TestGitPushWithHooks_UnreachableCheckpointRemoteContinues(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		srv := startGitHTTPSServer(t, "testorg/main-repo")
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		mainBare := srv.BareDirs["testorg/main-repo"]

		// Seed main-repo over the file path, then point origin at the plain HTTPS
		// URL. The go-git backend requires a non-empty Authorization header for
		// receive-pack; real git only sends one after a challenge, so set a static
		// http.extraHeader that git sends proactively on every request.
		httpsURL := srv.URL + "/testorg/main-repo.git"
		seedBareRepo(t, env, mainBare, httpsURL)
		env.SetGitConfig("http.extraHeader", "Authorization: Basic dGVzdDp0ZXN0")

		// checkpoint_remote points at a repo that does not exist on the server.
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_remote": map[string]any{
					"provider": "github",
					"repo":     "testorg/does-not-exist",
				},
			},
		})
		env.ExtraEnv = srv.tokenEnv("a4-token")

		_ = createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
		if !env.CheckpointsPresentLocally() {
			t.Fatalf("[%s] should have local checkpoints after condensation", backend)
		}

		out, err := env.GitPushArgsWithHooks("origin", "HEAD")
		if err != nil {
			t.Fatalf("[%s] user push should succeed even when the checkpoint remote is unreachable, got: %v\n%s", backend, err, out)
		}

		if !env.BranchExistsOnRemote(mainBare, "feature/test-branch") {
			t.Errorf("[%s] user feature branch should land on origin (main-repo)", backend)
		}
		// Checkpoints were routed to the missing sibling repo, so nothing landed
		// on main-repo.
		if env.CheckpointsPresentOnRemote(mainBare) {
			t.Errorf("[%s] checkpoints should NOT land on main-repo when routed to a missing checkpoint_remote", backend)
		}
	})
}

// TestGitPushWithHooks_GitRefsDrainsPushQueue is the git-refs slice of A6: a real
// `git push` drains the per-checkpoint push queue that git-refs writes on each
// checkpoint. After the hook-driven push the ref is on the bare remote and the
// queue file (in the git common dir) is emptied, proving the hook consumed the
// queue rather than leaving entries to accumulate.
//
// The v1-branch alternates scenario (TestAlternates_RelativeObjectAlternate_
// CheckpointSync) has no git-refs equivalent — git-refs writes independent
// fast-forward refs with no v1 rebase reading alternate-resident commits — so
// this asserts the queue-drain property instead of the rebase path.
func TestGitPushWithHooks_GitRefsDrainsPushQueue(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareDir := env.SetupBareRemote()

	checkpointID := createCheckpointedCommit(t, env, "Add service", "svc.go", "package svc", "Add service")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	// The checkpoint write enqueued its ref for push.
	if got := env.PushQueueRefs(); len(got) == 0 {
		t.Fatal("git-refs: push queue should hold the new checkpoint ref before push")
	}

	env.GitPushWithHooks("origin", "HEAD")

	if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
		t.Fatalf("git-refs: checkpoint %s should be on remote after the hook-driven push", checkpointID)
	}
	if got := env.PushQueueRefs(); len(got) != 0 {
		t.Errorf("git-refs: push queue should be drained after a successful push, still holds: %v", got)
	}
}
