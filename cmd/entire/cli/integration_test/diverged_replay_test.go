//go:build integration

package integration

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestResume_StaleLocalMetadata_RemoteHasNewerCheckpoint reproduces the
// user-visible PR #1252 bug end-to-end through the CLI binary: when the
// local entire/checkpoints/v1 ref is behind refs/remotes/origin/... (another
// machine pushed while we were offline), `entire session resume <branch>`
// printed "session log not available" for the newer checkpoint.
//
// The unit-level coverage in resume_test.go
// (TestResumeFromCurrentBranch_FastForwardsStaleLocalMetadata) exercises the
// same path with a direct function call. This test adds value by going
// through the real subprocess, a real bare remote, and a real `git fetch`
// round-trip — catching wiring regressions the unit test would miss.
func TestResume_StaleLocalMetadata_RemoteHasNewerCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()
	branchName := env.GetCurrentBranch()

	// Repo A: produce the first checkpoint and push everything.
	firstCheckpoint := createCheckpointedCommit(t, env, "Build A", "a.go", "package a", "Build A")
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone B clones at the first-checkpoint state. Pull the metadata branch
	// explicitly so Clone B has a local refs/heads/entire/checkpoints/v1 —
	// the PR #1252 bug requires the local ref to *exist* (early-return guard);
	// a fresh `git clone` only populates refs/remotes/origin/*.
	cloneB := env.CloneFrom(bareDir)
	cloneB.FetchMetadataBranch(bareDir)

	// Repo A: produce a second checkpoint on top of the first and push.
	// Origin's metadata branch advances; Clone B doesn't know yet.
	secondCheckpoint := createCheckpointedCommit(t, env, "Build B", "b.go", "package b", "Build B")
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone B does a vanilla `git fetch origin`. The default refspec writes
	// new state into refs/remotes/origin/* but leaves refs/heads/* alone —
	// Clone B's local refs/heads/entire/checkpoints/v1 is now stale (at the
	// first checkpoint) while refs/remotes/origin/entire/checkpoints/v1 sits
	// at the second.
	gitInDir(t, cloneB.RepoDir, "fetch", "origin")

	// Pull the feature branch fast-forward so Clone B's HEAD has the commit
	// whose trailer points at the second checkpoint.
	gitInDir(t, cloneB.RepoDir, "pull", "--ff-only", "origin", branchName)

	// Sanity: confirm we set up the "stale local + advanced remote" shape
	// PR #1252 was designed to repair.
	localMeta := gitRevParse(t, cloneB.RepoDir, "refs/heads/"+paths.MetadataBranchName)
	remoteMeta := gitRevParse(t, cloneB.RepoDir, "refs/remotes/origin/"+paths.MetadataBranchName)
	if localMeta == remoteMeta {
		t.Fatalf("test setup expected local stale vs. origin advanced; both at %s — `git fetch origin` may now auto-update the orphan branch", localMeta)
	}

	// Run resume through the CLI binary; it must not report missing logs.
	output, err := cloneB.RunCLIWithError("session", "resume", "--force", branchName)
	t.Logf("resume output:\n%s", output)
	if err != nil {
		t.Fatalf("session resume failed: %v\noutput: %s", err, output)
	}

	if strings.Contains(output, "session log not available") {
		t.Errorf("resume reported missing log for the second checkpoint (%s) when origin already had its metadata; output:\n%s", secondCheckpoint, output)
	}

	// The first checkpoint's metadata should still be accessible too — sanity
	// that we didn't break the easy case.
	explain, err := cloneB.RunCLIWithError("checkpoint", "explain", "--checkpoint", firstCheckpoint)
	if err != nil {
		t.Errorf("checkpoint explain for the first checkpoint (%s) failed after resume: %v\n%s", firstCheckpoint, err, explain)
	}
}

// TestPushAfterDiverged_NoDoubleReplayCommits guards against the architectural
// concern raised in the config-perm review (finding H1): if both
// fetchMetadataFromOrigin → SafelyAdvanceLocalRef AND pushCheckpointBranch →
// ReconcileDisconnectedMetadataBranch run during a single push retry, the
// merged history could contain a replayed commit twice. The contract is:
// after a push that recovers from a non-fast-forward by fetching and
// rebasing, the resulting metadata branch must contain exactly one commit
// per real checkpoint — not duplicates.
//
// The existing TestConcurrentPush_SecondPusherRebasesAndRetries verifies the
// flow completes and ends up linear; this test adds a commit-count check
// that would fail if the same local commit got cherry-picked twice (once on
// fetch, once on reconcile).
func TestPushAfterDiverged_NoDoubleReplayCommits(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	cloneA := env.CloneFrom(bareDir)
	cloneA.GitCheckoutNewBranch("feature/clone-a")

	cloneB := env.CloneFrom(bareDir)
	cloneB.GitCheckoutNewBranch("feature/clone-b")

	checkpointA := createCheckpointedCommit(t, cloneA, "Work in clone A", "a.go", "package a", "Work from A")
	checkpointB := createCheckpointedCommit(t, cloneB, "Work in clone B", "b.go", "package b", "Work from B")

	// A pushes first, B's push must fetch-and-retry.
	cloneA.RunPrePush("origin")
	cloneB.RunPrePush("origin")

	// Count commits on B's local metadata branch. Pre-divergence base + A's
	// commit + B's (rebased) commit = 3 expected. A double-replay would
	// produce 4 (or more) with B applied twice.
	localCount := gitRevListCount(t, cloneB.RepoDir, "refs/heads/"+paths.MetadataBranchName)
	if localCount != 3 {
		t.Errorf("clone B's metadata branch should have exactly 3 commits after divergence-then-push (root + A + B); got %d. Double-replay produces 4. checkpoints A=%s B=%s",
			localCount, checkpointA, checkpointB)
	}

	// And the remote should match — same count, same shape.
	remoteCount := gitRevListCount(t, bareDir, "refs/heads/"+paths.MetadataBranchName)
	if remoteCount != 3 {
		t.Errorf("origin's metadata branch should have exactly 3 commits; got %d", remoteCount)
	}
}

// ---- shared helpers (file-local) ------------------------------------------

func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s failed in %s: %v", ref, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func gitRevListCount(t *testing.T, dir, ref string) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-list", "--count", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-list --count %s failed in %s: %v", ref, dir, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing rev-list --count output %q: %v", out, err)
	}
	return count
}
