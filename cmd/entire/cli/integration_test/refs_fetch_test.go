//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestGitRefsClone_ExplainFetchesExactRef is test C2: after cloning a git-refs
// repo without any checkpoint refs, a read command (`entire explain`) triggers the
// on-demand RefFetcher, which fetches EXACTLY the one ref it needs — not the whole
// namespace. The remote is seeded with two checkpoints; reading the first fetches
// only its ref, leaving the second's ref absent until a read asks for it too.
func TestGitRefsClone_ExplainFetchesExactRef(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareDir := env.SetupBareRemote()

	// Two independent checkpoints on the feature branch.
	cp1 := createCheckpointedCommit(t, env, "Add first module", "one.go", "package one", "Add first module")
	cp2 := createCheckpointedCommit(t, env, "Add second module", "two.go", "package two", "Add second module")
	if cp1 == "" || cp2 == "" || cp1 == cp2 {
		t.Fatalf("expected two distinct checkpoint IDs, got %q and %q", cp1, cp2)
	}

	// Push both per-checkpoint refs to the remote via the hook path.
	env.RunPrePush("origin")
	if !env.CheckpointExistsOnRemote(bareDir, cp1) || !env.CheckpointExistsOnRemote(bareDir, cp2) {
		t.Fatal("both checkpoint refs should be on the remote after push")
	}

	// A plain clone carries refs/heads/* and tags, never refs/entire/*.
	clone := env.CloneFrom(bareDir)
	if refExists(t, clone.RepoDir, checkpointRefName(cp1)) || refExists(t, clone.RepoDir, checkpointRefName(cp2)) {
		t.Fatal("clone should not have any per-checkpoint refs before an on-demand read")
	}

	// Reading cp1 fetches only cp1's ref.
	out := clone.RunCLI("checkpoint", "explain", "--checkpoint", cp1)
	if !strings.Contains(out, "Add first module") {
		t.Errorf("explain cp1 should surface its prompt, got:\n%s", out)
	}
	if !refExists(t, clone.RepoDir, checkpointRefName(cp1)) {
		t.Errorf("cp1 ref should be fetched locally after explaining cp1")
	}
	if refExists(t, clone.RepoDir, checkpointRefName(cp2)) {
		t.Errorf("cp2 ref should NOT be fetched when only cp1 was read (RefFetcher over-fetched)")
	}

	// Reading cp2 then fetches cp2's ref on demand.
	out = clone.RunCLI("checkpoint", "explain", "--checkpoint", cp2)
	if !strings.Contains(out, "Add second module") {
		t.Errorf("explain cp2 should surface its prompt, got:\n%s", out)
	}
	if !refExists(t, clone.RepoDir, checkpointRefName(cp2)) {
		t.Errorf("cp2 ref should be fetched locally after explaining cp2")
	}
}

// TestGitRefsClone_UnreachableRemoteMissingRefSurfacesRealError is test C3, a
// regression guard for 7bbdad09c: under git-refs, when a checkpoint's ref is
// missing locally AND the remote is unreachable, the read must surface the real
// fetch failure rather than masking it as "checkpoint not found" (which would tell
// the user the checkpoint doesn't exist when it may well exist on a reachable
// remote).
func TestGitRefsClone_UnreachableRemoteMissingRefSurfacesRealError(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareDir := env.SetupBareRemote()

	cp := createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
	if cp == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	env.RunPrePush("origin")

	clone := env.CloneFrom(bareDir)
	if refExists(t, clone.RepoDir, checkpointRefName(cp)) {
		t.Fatal("clone should not have the per-checkpoint ref before a read")
	}

	// Point origin at a nonexistent path so the on-demand fetch fails.
	clone.SetGitConfig("remote.origin.url", clone.RepoDir+"/nonexistent-remote.git")

	out, err := clone.RunCLIWithError("checkpoint", "explain", "--checkpoint", cp)
	if err == nil {
		t.Fatalf("explain should fail when the ref is missing and the remote is unreachable, got success:\n%s", out)
	}

	// The store layer preserves the real fetch error (7bbdad09c), and explain's
	// git-refs prefix-match remote fallback must not re-mask it as a plain
	// "checkpoint not found" — that would report an unreachable remote
	// identically to a genuinely absent checkpoint.
	if strings.Contains(out, "checkpoint not found:") {
		t.Errorf("unreachable-remote fetch failure masked as 'checkpoint not found':\n%s", out)
	}
	if !strings.Contains(out, "fetching from remote failed") {
		t.Errorf("explain should surface the real fetch failure, got:\n%s", out)
	}
}
