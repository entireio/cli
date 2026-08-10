//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Multi-push-URL characterization tests: ONE git remote carrying several push
// URLs (`git remote set-url --add --push origin <url>`, which git allows to
// repeat). This is the "mirror to two forges in one push" / backup-remote setup.
//
// Two git facts drive everything here:
//
//  1. `git push origin` fans out to EVERY push URL.
//  2. git invokes the pre-push hook ONCE PER PUSH URL, passing the same remote
//     NAME as $1 every time and the individual URL as $2. Our hook forwards only
//     $1, so the CLI sees N identical invocations and hands `git push <name>` the
//     name — letting git fan out a second time.
//
// The two backends diverge from there, deliberately. git-branch pushes to the
// remote NAME and inherits git's fan-out, so it has no per-URL control — it can
// only decide *whether* to push for a given hook invocation, not where. git-refs
// resolves the first push URL itself and targets that one destination, because
// its push queue records a ref with no per-destination state.
//
// The backends are covered by separate tests rather than ForEachBackend because
// the interesting failure modes differ: the git-branch v1 branch is a single
// shared ref that can diverge per URL, while git-refs' per-checkpoint refs
// normally only ever fast-forward.

// v1Ref is the fully-qualified git-branch metadata ref.
const v1Ref = "refs/heads/" + paths.MetadataBranchName

// TestMultiPushURL_Branch_FanOutToBothPushURLs establishes the baseline: with two
// push URLs on one remote, a plain `git push` through the real hook lands the v1
// metadata branch on BOTH URLs. Nothing in the CLI arranges this — git's own
// fan-out does it, because the CLI pushes to the remote name.
func TestMultiPushURL_Branch_FanOutToBothPushURLs(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareA := env.SetupBareRemote()
	bareB := env.AddSecondPushURL("origin")

	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	env.GitPushWithHooks("origin", "HEAD")

	if !env.CheckpointExistsOnRemote(bareA, checkpointID) {
		t.Errorf("checkpoint %s should be on the first push URL", checkpointID)
	}
	if !env.CheckpointExistsOnRemote(bareB, checkpointID) {
		t.Errorf("checkpoint %s should be on the second push URL (git fans out to every push URL)", checkpointID)
	}
}

// TestMultiPushURL_Branch_RepeatedPushesKeepBothURLsInSync guards the property a
// mirror user actually depends on: over a normal sequence of checkpoints, every
// push URL stays in sync. Each push fast-forwards all of them, so this works
// today purely because checkpoints are pushed to the remote NAME and git fans
// out.
//
// It exists to fail loudly if checkpoint pushes are ever retargeted at a single
// resolved URL. That looks like a tidy way to give a user who does not want
// checkpoints mirrored what they asked for, but it silently breaks the user who
// configured several push URLs precisely because they DO want everything
// mirrored. A single destination must come from explicit configuration
// (checkpoint_remote), never be inferred from topology.
func TestMultiPushURL_Branch_RepeatedPushesKeepBothURLsInSync(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareA := env.SetupBareRemote()
	bareB := env.AddSecondPushURL("origin")

	files := []struct{ prompt, file, content, msg string }{
		{"Add auth module", "auth.go", "package auth", "Add auth module"},
		{"Add login handler", "login.go", "package login", "Add login handler"},
		{"Add session store", "session.go", "package session", "Add session store"},
	}

	var checkpointIDs []string
	for _, f := range files {
		id := createCheckpointedCommit(t, env, f.prompt, f.file, f.content, f.msg)
		if id == "" {
			t.Fatalf("no checkpoint ID after committing %s", f.file)
		}
		checkpointIDs = append(checkpointIDs, id)
		env.GitPushWithHooks("origin", "HEAD")
	}

	for i, id := range checkpointIDs {
		if !env.CheckpointExistsOnRemote(bareA, id) {
			t.Errorf("checkpoint %d (%s) missing from the first push URL", i+1, id)
		}
		if !env.CheckpointExistsOnRemote(bareB, id) {
			t.Errorf("checkpoint %d (%s) missing from the second push URL", i+1, id)
		}
	}
	if a, b := env.RefHashOnRemote(bareA, v1Ref), env.RefHashOnRemote(bareB, v1Ref); a != b {
		t.Errorf("both push URLs should hold the same v1 tip; first=%s second=%s", a, b)
	}
}

// TestMultiPushURL_Branch_SecondPushURLDivergedDifferently is the scenario that
// motivated these tests, and it documents a real gap.
//
// Both push URLs hold v1, then each moves independently (two machines pushing
// checkpoints to two mirrors is enough to cause this). The next local push is
// rejected by both, so the CLI runs its fetch+rebase recovery — but recovery
// fetches from the remote NAME, and `git fetch origin` reads only the remote's
// FETCH url. So the local branch is reconciled against the first URL only, the
// retry lands there, and the second URL is left rejected with its divergence
// never merged. Because checkpoint push failures are deliberately swallowed so
// they cannot break the user's push, this is silent: the user's `git push`
// succeeds and the second mirror simply stops receiving checkpoints.
//
// The assertions after the t.Skip below state the DESIRED behavior — both push
// URLs converge — so implementing per-URL reconciliation turns this into an
// enforcing test by deleting one line. Everything before the skip asserts what
// already works today.
func TestMultiPushURL_Branch_SecondPushURLDivergedDifferently(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareA := env.SetupBareRemote()
	bareB := env.AddSecondPushURL("origin")

	// Round 1: both URLs receive the same v1.
	first := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	env.GitPushWithHooks("origin", "HEAD")
	if !env.CheckpointExistsOnRemote(bareA, first) || !env.CheckpointExistsOnRemote(bareB, first) {
		t.Fatalf("setup: checkpoint %s should be on both push URLs before diverging", first)
	}

	// Both remotes move, differently — distinct labels give distinct hashes, so
	// the two mirrors can never accidentally converge.
	divergedA := env.DivergeRemoteRef(bareA, v1Ref, "on-A")
	divergedB := env.DivergeRemoteRef(bareB, v1Ref, "on-B")
	if divergedA == divergedB {
		t.Fatal("setup: the two push URLs must diverge to different commits")
	}

	// Round 2: a new checkpoint, pushed into that divergence.
	second := createCheckpointedCommit(t, env, "Add login handler", "login.go", "package login", "Add login handler")

	// The user's own push succeeds: checkpoint push failures are swallowed by
	// design so they cannot break it. That is what makes the divergence below
	// silent — the only trace is a "failed to push ... after sync" warning on
	// stderr that does not name the URL that rejected it.
	if err := env.GitPushWithHooksAllowError("origin", "HEAD"); err != nil {
		t.Fatalf("the user's push should succeed even though a checkpoint push failed: %v", err)
	}

	// The first push URL converges: recovery fetched from it, replayed the local
	// commits on top, and the retry was a fast-forward.
	if !env.CheckpointExistsOnRemote(bareA, second) {
		t.Errorf("checkpoint %s should be on the first push URL after fetch+rebase recovery", second)
	}
	if !env.IsAncestor(bareA, divergedA, v1Ref) {
		t.Errorf("first push URL's own divergent commit %s should be preserved as an ancestor of v1", divergedA)
	}

	// Diagnostics for why the second URL cannot self-heal within this push: there
	// is ONE remote-tracking ref per remote NAME, and git advanced it to the hash
	// the successful URL accepted. So pushRefIfNeeded's "does this ref have
	// unpushed changes?" check reports "in sync with origin" while one of origin's
	// URLs is still on its old divergent commit, and the second hook invocation
	// (git runs one per push URL) short-circuits without attempting anything.
	// Logged rather than asserted: this is the mechanism of the bug, not behavior
	// worth locking in.
	t.Logf("local v1=%s  refs/remotes/origin/%s=%s  urlA v1=%s  urlB v1=%s (still at its divergence %s)",
		env.RefHashOnRemote(env.RepoDir, v1Ref),
		paths.MetadataBranchName,
		env.RefHashOnRemote(env.RepoDir, "refs/remotes/origin/"+paths.MetadataBranchName),
		env.RefHashOnRemote(bareA, v1Ref),
		env.RefHashOnRemote(bareB, v1Ref),
		divergedB)

	t.Skip("KNOWN BUG: fetch+rebase recovery reconciles only the remote's fetch URL, so a second push URL that diverged independently never converges and is never retried")

	// DESIRED: every push URL of the remote converges, each reconciled against
	// its own divergence. Delete the t.Skip above once that is implemented.
	if !env.CheckpointExistsOnRemote(bareB, second) {
		t.Errorf("checkpoint %s should also reach the second push URL", second)
	}
	if got := env.RefHashOnRemote(bareB, v1Ref); got == divergedB {
		t.Errorf("second push URL's v1 should have advanced past its divergence %s", divergedB)
	}
	if !env.IsAncestor(bareB, divergedB, v1Ref) {
		t.Errorf("second push URL's own divergent commit %s should be preserved as an ancestor of v1", divergedB)
	}
}

// TestMultiPushURL_Branch_DoesNotPublishV1ToEmptySecondPushURL shows that adding
// a push URL defeats the empty-remote guard.
//
// deferCheckpointPushOnEmptyRemote exists because entire/checkpoints/v1 is a real
// refs/heads branch, so a forge would make it the default branch of an empty
// repository. The guard asks whether the remote NAME has any local
// remote-tracking refs — which it does, thanks to the established first URL — so
// it permits the push, and the fan-out then publishes v1 into the brand-new
// second URL that has no branches at all. The hook runs before git transfers the
// user's branch, so v1 gets there first and becomes that repo's default branch.
//
// The assertion after the t.Skip states the desired behavior: a push URL with no
// branches should get the same deferral an empty remote gets.
func TestMultiPushURL_Branch_DoesNotPublishV1ToEmptySecondPushURL(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	env.SetupBareRemote()
	bareB := env.AddSecondPushURL("origin")

	createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	// Run only the hook — exactly the point in a real `git push` at which the
	// user's own branch has not been transferred yet.
	env.RunPrePush("origin")

	if env.BranchExistsOnRemote(bareB, env.GetCurrentBranch()) {
		t.Fatalf("setup: the user branch should not be on the second push URL yet")
	}

	t.Skip("KNOWN BUG: the empty-remote guard checks remote-tracking refs per remote NAME, so a brand-new second push URL receives v1 before the user's first branch and would adopt it as the default branch")

	// DESIRED: v1 is withheld from a push URL that has no branches yet, exactly
	// as it is withheld from an empty remote. Delete the t.Skip above once the
	// guard is URL-aware.
	if env.BranchExistsOnRemote(bareB, paths.MetadataBranchName) {
		t.Errorf("v1 should not be the first branch published to a fresh push URL")
	}
}

// TestMultiPushURL_Refs_GoesToFirstPushURLOnly pins the git-refs destination
// rule: with several push URLs on one remote, checkpoint refs go to the FIRST
// push URL and the rest are deliberately skipped.
//
// The push-discovery queue records only a ref, with no per-destination state, so
// "this ref is pushed" has to mean one place. git's fan-out cannot provide that:
// one failing URL fails the whole invocation so nothing unqueues even when other
// URLs took the refs, and an unreachable FIRST URL makes git die() before
// reaching any later URL (see ..._Refs_UnreachableFirstPushURL_ReachesNothing).
//
// The trade — checkpoints live in exactly one repository, and cloning a different
// mirror of the same code will not find them — is why the user is warned on
// stderr, and why checkpoint_remote remains the way to name the repository
// explicitly. Note the git-branch backend deliberately still fans out; see
// ..._Branch_RepeatedPushesKeepBothURLsInSync.
func TestMultiPushURL_Refs_GoesToFirstPushURLOnly(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareA := env.SetupBareRemote()
	bareB := env.AddSecondPushURL("origin")

	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	if len(env.QueuedCheckpointRefs()) == 0 {
		t.Fatal("setup: the checkpoint write should have enqueued a ref for push")
	}

	env.GitPushWithHooks("origin", "HEAD")

	if !env.CheckpointExistsOnRemote(bareA, checkpointID) {
		t.Errorf("checkpoint ref for %s should be on the first push URL", checkpointID)
	}
	if env.CheckpointExistsOnRemote(bareB, checkpointID) {
		t.Errorf("checkpoint ref for %s should NOT be on the second push URL: refs target the first push URL only", checkpointID)
	}
	// The destination took them, so they unqueue — the property the whole rule
	// exists to make possible.
	if queued := env.QueuedCheckpointRefs(); len(queued) != 0 {
		t.Errorf("queue should be empty after a confirmed push, still holds %v", queued)
	}
}

// TestMultiPushURL_Refs_UnreachableFirstPushURL_ReachesNothing covers the case
// that motivated targeting one URL explicitly rather than leaning on git's
// fan-out.
//
// git iterates a remote's push URLs in order, and a transport failure (missing
// repo, auth, bad host) is fatal — it die()s rather than returning, so URLs after
// the failing one are never attempted at all. A rejection (non-fast-forward) by
// contrast returns, and git carries on to the later URLs. So under fan-out a dead
// mirror in FIRST position blocks checkpoint sync completely, while the same
// mirror in last position does not: order silently decided the outcome.
//
// Targeting the first push URL makes that explicit instead of emergent, and the
// refs stay queued either way, so nothing is lost.
func TestMultiPushURL_Refs_UnreachableFirstPushURL_ReachesNothing(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareA := env.SetupBareRemote()
	env.AddUnreachableFirstPushURL("origin")

	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if len(env.QueuedCheckpointRefs()) == 0 {
		t.Fatal("setup: the checkpoint write should have enqueued a ref for push")
	}

	if err := env.GitPushWithHooksAllowError("origin", "HEAD"); err == nil {
		t.Fatal("setup: git push should fail when the first push URL is unreachable")
	}

	if env.CheckpointsPresentOnRemote(bareA) {
		t.Errorf("no checkpoint should reach the reachable URL: the unreachable first push URL is the target")
	}
	wantRef := checkpointRefName(checkpointID)
	if queued := env.QueuedCheckpointRefs(); !slices.Contains(queued, wantRef) {
		t.Errorf("ref %s should stay queued when the destination is unreachable; queue holds %v", wantRef, queued)
	}
}

// TestMultiPushURL_Refs_UnreachableSecondPushURL_IsIgnored is the counterpart:
// a broken mirror in a LATER position is now simply irrelevant to checkpoint
// refs, because they target the first push URL only.
//
// Under git's fan-out this same topology failed the whole push and wedged the
// queue indefinitely — the refs had reached the healthy URL but nothing could
// unqueue them, so every later push retried and failed again. Targeting one URL
// removes that failure mode entirely.
func TestMultiPushURL_Refs_UnreachableSecondPushURL_IsIgnored(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareA := env.SetupBareRemote()
	env.AddUnreachableSecondPushURL("origin")

	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if len(env.QueuedCheckpointRefs()) == 0 {
		t.Fatal("setup: the checkpoint write should have enqueued a ref for push")
	}

	// The user's own branch push still fails — that is git's fan-out over a dead
	// URL and not something the CLI can or should mask.
	if err := env.GitPushWithHooksAllowError("origin", "HEAD"); err == nil {
		t.Fatal("setup: git push should fail when one of the push URLs is unreachable")
	}

	if !env.CheckpointExistsOnRemote(bareA, checkpointID) {
		t.Errorf("checkpoint ref for %s should reach the first push URL", checkpointID)
	}
	if queued := env.QueuedCheckpointRefs(); len(queued) != 0 {
		t.Errorf("refs should unqueue once the destination took them, even though a later push URL is dead; queue holds %v", queued)
	}
}

// TestMultiPushURL_DestinationNoteSurfaces checks that the ambiguity is
// announced rather than left for a reader of the source: `entire doctor` reports
// it, and it stays silent on an ordinary single-destination repo so the common
// output is unchanged.
func TestMultiPushURL_DestinationNoteSurfaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		setup  func(env *TestEnv)
		want   []string
		absent []string
	}{
		{
			name:   "one remote, one push URL",
			setup:  func(*TestEnv) {},
			absent: []string{"Checkpoint destination"},
		},
		{
			name:  "remote with several push URLs",
			setup: func(env *TestEnv) { env.AddSecondPushURL("origin") },
			want:  []string{"Checkpoint destination", "pushes to 2 URLs", "first URL only", "checkpoint_remote"},
		},
		{
			name:  "several remotes",
			setup: func(env *TestEnv) { env.SetupNamedBareRemote("backup") },
			want:  []string{"2 remotes", "always looks at origin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = StoreGitRefs
			env.SetupBareRemote()
			tt.setup(env)

			out := env.RunCLI("doctor")
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("doctor output should mention %q:\n%s", want, out)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(out, absent) {
					t.Errorf("doctor output should not mention %q for this repo:\n%s", absent, out)
				}
			}
		})
	}
}
