package strategy

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metadataOriginRemoteRef() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
}

func TestReconcileDisconnected_NoRemote(t *testing.T) {
	t.Parallel()

	// Local-only repo with metadata branch, no remote tracking branch
	tmpDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test User")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")

	// Create orphan metadata branch
	run("checkout", "--orphan", paths.MetadataBranchName)
	run("rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(tmpDir, "metadata.json"), []byte(`{"test":true}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "checkpoint")
	run("checkout", "main")

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Should be a no-op (no remote)
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcileDisconnected_NoLocal(t *testing.T) {
	t.Parallel()

	// Clone from bare with remote metadata but no local metadata branch
	bareDir := initBareWithMetadataBranch(t)
	cloneDir, _ := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// No local branch → no-op
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcileDisconnected_SameHash(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, _ := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Create local branch from remote (same hash)
	if err := EnsureMetadataBranch(t.Context(), repo); err != nil {
		t.Fatalf("EnsureMetadataBranch failed: %v", err)
	}

	// Same hash → no-op
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcileDisconnected_SharedAncestry(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Create local branch from remote (shared base)
	if err := EnsureMetadataBranch(t.Context(), repo); err != nil {
		t.Fatalf("EnsureMetadataBranch failed: %v", err)
	}

	// Add a local commit on top (diverged, but shared ancestry)
	run("checkout", paths.MetadataBranchName)
	localDir := filepath.Join(cloneDir, "cd", "ef01234567")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "metadata.json"), []byte(`{"test":"local"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "local checkpoint")
	run("checkout", "main")

	// Re-open to see updated refs
	repo, err = git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to re-open repo: %v", err)
	}

	// Shared ancestry → no-op
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcileDisconnected_Disconnected(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local metadata branch (simulating the empty-orphan bug)
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")
	localDir := filepath.Join(cloneDir, "ab", "cdef012345")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "metadata.json"), []byte(`{"checkpoint_id":"abcdef012345"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: abcdef012345")
	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Verify they are disconnected before reconcile
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	localRef, err := repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("local ref not found: %v", err)
	}
	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		t.Fatalf("remote ref not found: %v", err)
	}
	if localRef.Hash() == remoteRef.Hash() {
		t.Fatal("expected different hashes before reconcile")
	}

	// Run reconciliation
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("ReconcileDisconnectedMetadataBranch() failed: %v", err)
	}

	// Verify result
	newRef, err := repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("local ref not found after reconcile: %v", err)
	}

	// Should have linear history: new tip -> remote tip -> remote root
	tipCommit, err := repo.CommitObject(newRef.Hash())
	if err != nil {
		t.Fatalf("failed to get tip commit: %v", err)
	}

	// Tip's parent should be the remote tip (linear chain, not merge)
	if len(tipCommit.ParentHashes) != 1 {
		t.Fatalf("expected 1 parent (linear), got %d", len(tipCommit.ParentHashes))
	}
	if tipCommit.ParentHashes[0] != remoteRef.Hash() {
		t.Errorf("tip parent = %s, want remote tip %s", tipCommit.ParentHashes[0], remoteRef.Hash())
	}

	// Verify merged tree contains both local and remote data
	tree, err := tipCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, tree, "", entries); err != nil {
		t.Fatalf("failed to flatten tree: %v", err)
	}

	// Remote data: metadata.json at root (from initBareWithMetadataBranch)
	if _, ok := entries["metadata.json"]; !ok {
		t.Error("merged tree missing remote data (metadata.json)")
	}
	// Local data: ab/cdef012345/metadata.json
	if _, ok := entries["ab/cdef012345/metadata.json"]; !ok {
		t.Error("merged tree missing local data (ab/cdef012345/metadata.json)")
	}

	// Original commit message should be preserved (git adds trailing newline)
	if tipCommit.Message != "Checkpoint: abcdef012345\n" {
		t.Errorf("commit message not preserved: got %q", tipCommit.Message)
	}
}

// Not parallel: uses t.Chdir().
func TestReconcileDisconnected_MirrorsV1CustomRefAfterRepair(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string, run func(args ...string))
	}{
		{
			name: "empty orphan reset",
			setup: func(t *testing.T, _ string, run func(args ...string)) {
				t.Helper()
				run("checkout", "--orphan", "temp-orphan")
				run("rm", "-rf", ".")
				run("commit", "--allow-empty", "-m", "empty orphan init")
				run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
				run("checkout", "main")
			},
		},
		{
			name: "local checkpoint replay",
			setup: func(t *testing.T, dir string, run func(args ...string)) {
				t.Helper()
				run("checkout", "--orphan", "temp-orphan")
				run("rm", "-rf", ".")
				localDir := filepath.Join(dir, "ab", "cdef012345")
				require.NoError(t, os.MkdirAll(localDir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(localDir, "metadata.json"),
					[]byte(`{"checkpoint_id":"abcdef012345"}`),
					0o644,
				))
				run("add", ".")
				run("commit", "-m", "Checkpoint: abcdef012345")
				run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
				run("checkout", "main")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bareDir := initBareWithMetadataBranch(t)
			cloneDir, run := cloneWithConfig(t, bareDir)
			tt.setup(t, cloneDir, run)

			enableV1CustomRefMirror(t, cloneDir)
			t.Chdir(cloneDir)

			repo, err := git.PlainOpen(cloneDir)
			require.NoError(t, err)
			_, ok := v1CustomRefHash(t, repo)
			require.False(t, ok, "custom ref should not exist before reconciliation")

			require.NoError(t, ReconcileDisconnectedMetadataBranch(t.Context(), repo, metadataOriginRemoteRef(), io.Discard))

			localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
			require.NoError(t, err)
			got, ok := v1CustomRefHash(t, repo)
			require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
			assert.Equal(t, localRef.Hash(), got)
		})
	}
}

func TestReconcileDisconnected_MultipleLocalCheckpoints(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local branch with 3 commits (empty root + 3 data commits)
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")

	// Empty root commit (the orphan bug commit)
	run("commit", "--allow-empty", "-m", "Initialize metadata branch")

	// Checkpoint 1
	dir1 := filepath.Join(cloneDir, "11", "1111111111")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "metadata.json"), []byte(`{"checkpoint_id":"111111111111"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: 111111111111")

	// Checkpoint 2
	dir2 := filepath.Join(cloneDir, "22", "2222222222")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "metadata.json"), []byte(`{"checkpoint_id":"222222222222"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: 222222222222")

	// Checkpoint 3
	dir3 := filepath.Join(cloneDir, "33", "3333333333")
	if err := os.MkdirAll(dir3, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir3, "metadata.json"), []byte(`{"checkpoint_id":"333333333333"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: 333333333333")

	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		t.Fatalf("remote ref not found: %v", err)
	}

	// Run reconciliation
	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("ReconcileDisconnectedMetadataBranch() failed: %v", err)
	}

	// Verify result
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	newRef, err := repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("local ref not found after reconcile: %v", err)
	}

	// Walk commits to verify linear chain
	var commitMessages []string
	current := newRef.Hash()
	for range 10 {
		c, cErr := repo.CommitObject(current)
		if cErr != nil {
			t.Fatalf("failed to get commit %s: %v", current, cErr)
		}
		commitMessages = append(commitMessages, c.Message)
		if len(c.ParentHashes) == 0 {
			break
		}
		if len(c.ParentHashes) != 1 {
			t.Fatalf("expected linear history, commit %s has %d parents", c.Hash, len(c.ParentHashes))
		}
		current = c.ParentHashes[0]
	}

	// Should have: 3 cherry-picked + remote commits (1 data + 1 root = at least 2)
	// The empty orphan commit is skipped, so we get exactly 3 cherry-picked commits
	if len(commitMessages) < 4 {
		t.Errorf("expected at least 4 commits in chain, got %d: %v", len(commitMessages), commitMessages)
	}

	// Verify all checkpoint data is in the final tree
	tipCommit, err := repo.CommitObject(newRef.Hash())
	if err != nil {
		t.Fatalf("failed to get tip: %v", err)
	}
	tree, err := tipCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, tree, "", entries); err != nil {
		t.Fatalf("failed to flatten tree: %v", err)
	}

	expectedPaths := []string{
		"metadata.json",               // Remote data
		"11/1111111111/metadata.json", // Checkpoint 1
		"22/2222222222/metadata.json", // Checkpoint 2
		"33/3333333333/metadata.json", // Checkpoint 3
	}
	for _, p := range expectedPaths {
		if _, ok := entries[p]; !ok {
			t.Errorf("merged tree missing expected path: %s", p)
		}
	}

	// First cherry-picked commit's parent should be the remote tip
	// Walk back from tip: tip (cp3) -> cp2 -> cp1 -> remote tip
	cp3, err := repo.CommitObject(newRef.Hash())
	if err != nil {
		t.Fatalf("failed to get cp3: %v", err)
	}
	cp2, err := repo.CommitObject(cp3.ParentHashes[0])
	if err != nil {
		t.Fatalf("failed to get cp2: %v", err)
	}
	cp1, err := repo.CommitObject(cp2.ParentHashes[0])
	if err != nil {
		t.Fatalf("failed to get cp1: %v", err)
	}
	if cp1.ParentHashes[0] != remoteRef.Hash() {
		t.Errorf("first cherry-picked commit parent = %s, want remote tip %s",
			cp1.ParentHashes[0], remoteRef.Hash())
	}
}

func TestIsMetadataDisconnected_NoRemote(t *testing.T) {
	t.Parallel()

	// Local-only repo with metadata branch, no remote tracking branch
	tmpDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test User")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")

	// Create orphan metadata branch
	run("checkout", "--orphan", paths.MetadataBranchName)
	run("rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(tmpDir, "metadata.json"), []byte(`{"test":true}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "checkpoint")
	run("checkout", "main")

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disconnected {
		t.Error("expected false (no remote), got true")
	}
}

func TestIsMetadataDisconnected_NoLocal(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, _ := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// No local branch → false
	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disconnected {
		t.Error("expected false (no local), got true")
	}
}

func TestIsMetadataDisconnected_SameHash(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, _ := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	if err := EnsureMetadataBranch(t.Context(), repo); err != nil {
		t.Fatalf("EnsureMetadataBranch failed: %v", err)
	}

	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disconnected {
		t.Error("expected false (same hash), got true")
	}
}

func TestIsMetadataDisconnected_SharedAncestry(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	if err := EnsureMetadataBranch(t.Context(), repo); err != nil {
		t.Fatalf("EnsureMetadataBranch failed: %v", err)
	}

	// Add a local commit on top (diverged, but shared ancestry)
	run("checkout", paths.MetadataBranchName)
	localDir := filepath.Join(cloneDir, "cd", "ef01234567")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "metadata.json"), []byte(`{"test":"local"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "local checkpoint")
	run("checkout", "main")

	repo, err = git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to re-open repo: %v", err)
	}

	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disconnected {
		t.Error("expected false (shared ancestry), got true")
	}
}

func TestIsMetadataDisconnected_Disconnected(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local metadata branch
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")
	localDir := filepath.Join(cloneDir, "ab", "cdef012345")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "metadata.json"), []byte(`{"checkpoint_id":"abcdef012345"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: abcdef012345")
	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !disconnected {
		t.Error("expected true (disconnected), got false")
	}
}

func TestReconcileDisconnected_ModifiedEntries(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local branch where commit 2 modifies a file from commit 1
	// (simulates multi-session condensation updating metadata.json)
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")

	// Commit 1: initial checkpoint
	dir1 := filepath.Join(cloneDir, "aa", "aaaaaaaaaa")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa","session_count":1}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: aaaaaaaaaaaa")

	// Commit 2: update same checkpoint (session_count 1→2) + add new file
	if err := os.WriteFile(filepath.Join(dir1, "metadata.json"),
		[]byte(`{"checkpoint_id":"aaaaaaaaaaaa","session_count":2}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir1, "1"), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "1", "metadata.json"),
		[]byte(`{"session_id":"second-session"}`), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "Checkpoint: aaaaaaaaaaaa (update)")

	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	if err := ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard); err != nil {
		t.Fatalf("ReconcileDisconnectedMetadataBranch() failed: %v", err)
	}

	// Verify the MODIFIED metadata.json has session_count:2, not the original 1
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	newRef, err := repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("local ref not found: %v", err)
	}
	tipCommit, err := repo.CommitObject(newRef.Hash())
	if err != nil {
		t.Fatalf("failed to get tip: %v", err)
	}
	tree, err := tipCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	metadataFile, err := tree.File("aa/aaaaaaaaaa/metadata.json")
	if err != nil {
		t.Fatalf("metadata.json not found in tree: %v", err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}
	if !strings.Contains(content, `"session_count":2`) {
		t.Errorf("metadata.json should have session_count:2 (modified value), got: %s", content)
	}
}

// TestCollectCommitChain_DepthLimit verifies that collectCommitChain returns an error
// when the commit chain exceeds MaxCommitTraversalDepth without reaching a root commit.
func TestCollectCommitChain_DepthLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	// Create an empty tree for all commits.
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, emptyTree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	// Build a linear chain of MaxCommitTraversalDepth+1 commits (all have parents,
	// so none is a root). collectCommitChain should bail out at the depth limit.
	var tip plumbing.Hash
	for i := range MaxCommitTraversalDepth + 1 {
		c := &object.Commit{
			TreeHash:  treeHash,
			Author:    object.Signature{Name: "test", Email: "test@test.com", When: time.Now().Add(time.Duration(i) * time.Second)},
			Committer: object.Signature{Name: "test", Email: "test@test.com", When: time.Now().Add(time.Duration(i) * time.Second)},
			Message:   "commit\n",
		}
		if tip != plumbing.ZeroHash {
			c.ParentHashes = []plumbing.Hash{tip}
		}
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, c.Encode(obj))
		h, sErr := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, sErr)
		tip = h
	}

	_, err = collectCommitChain(repo, tip, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
	assert.Contains(t, err.Error(), "without reaching root")
}

// TestCollectCommitChain_StopsAtShallowBoundary verifies that collectCommitChain
// treats commits listed in the shallow set as roots, stopping the walk at the
// boundary even when the boundary commit has a parent SHA recorded in the object
// store. Without this behaviour, a shallow checkpoint repo whose remote v1 was
// rebuilt elsewhere would produce a phantom chain of stale commits.
func TestCollectCommitChain_StopsAtShallowBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, emptyTree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	// Build a linear chain of 10 commits. Without shallow, the walk should
	// return all 10. With the 4th from the tip marked shallow, it should stop
	// at that commit, returning 4 entries (tip + 3 below it, including the
	// shallow boundary itself, treated as a root).
	var tip plumbing.Hash
	hashes := make([]plumbing.Hash, 0, 10)
	for i := range 10 {
		c := &object.Commit{
			TreeHash:  treeHash,
			Author:    object.Signature{Name: "test", Email: "test@test.com", When: time.Now().Add(time.Duration(i) * time.Second)},
			Committer: object.Signature{Name: "test", Email: "test@test.com", When: time.Now().Add(time.Duration(i) * time.Second)},
			Message:   "commit\n",
		}
		if tip != plumbing.ZeroHash {
			c.ParentHashes = []plumbing.Hash{tip}
		}
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, c.Encode(obj))
		h, sErr := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, sErr)
		hashes = append(hashes, h)
		tip = h
	}

	// Without shallow set: full chain of 10.
	chain, err := collectCommitChain(repo, tip, nil)
	require.NoError(t, err)
	assert.Len(t, chain, 10, "without shallow, expect full chain")

	// With the 4th-from-tip (index 6 in build order) marked shallow: walk
	// stops there. Result is oldest-first: shallow boundary, then up to tip
	// = 4 commits.
	shallow := map[plumbing.Hash]bool{hashes[6]: true}
	chain, err = collectCommitChain(repo, tip, shallow)
	require.NoError(t, err)
	require.Len(t, chain, 4, "expect tip + 3 commits down to the shallow boundary inclusive")
	assert.Equal(t, hashes[6], chain[0].Hash, "oldest entry should be the shallow boundary")
	assert.Equal(t, tip, chain[3].Hash, "newest entry should be the tip")
}

func TestLoadShallowHashes(t *testing.T) {
	t.Parallel()

	t.Run("non-shallow repo returns empty set", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := git.PlainInit(dir, false)
		require.NoError(t, err)

		set, err := loadShallowHashes(context.Background(), dir)
		require.NoError(t, err)
		assert.Empty(t, set)
	})

	t.Run("reads .git/shallow hash list", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := git.PlainInit(dir, false)
		require.NoError(t, err)

		shallowFile := filepath.Join(dir, ".git", "shallow")
		require.NoError(t, os.WriteFile(shallowFile,
			[]byte("be156aa7cc38c2c6117246cb8adad068a3886351\n82b2a8554cccd19f5aa60f520f645b32d8bc2400\n"),
			0o644))

		set, err := loadShallowHashes(context.Background(), dir)
		require.NoError(t, err)
		assert.Len(t, set, 2)
		assert.True(t, set[plumbing.NewHash("be156aa7cc38c2c6117246cb8adad068a3886351")])
		assert.True(t, set[plumbing.NewHash("82b2a8554cccd19f5aa60f520f645b32d8bc2400")])
	})
}

// TestReconcileDisconnected_AllEmptyOrphans verifies that when all local commits
// are empty-tree orphan commits (the exact bug artifact), reconciliation resets
// the local branch to the remote tip without cherry-picking.
func TestReconcileDisconnected_AllEmptyOrphans(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local branch with ONLY empty-tree commits
	// (simulating the empty-orphan bug in a repo that never had real checkpoints)
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")
	// Commit with empty tree (git allows this with --allow-empty)
	run("commit", "--allow-empty", "-m", "empty orphan init")
	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)

	// Get remote hash before reconciliation
	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	require.NoError(t, err)

	err = ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard)
	require.NoError(t, err)

	// Local branch should now point to the remote tip (reset, not cherry-picked)
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	localRef, err := repo.Reference(refName, true)
	require.NoError(t, err)

	assert.Equal(t, remoteRef.Hash(), localRef.Hash(),
		"local should be reset to remote tip when all local commits are empty orphans")
}

// TestReconcileDisconnected_CherryPickDeletion verifies that when a local commit
// deletes a file from its parent, the deletion is correctly propagated during
// cherry-pick reconciliation.
func TestReconcileDisconnected_CherryPickDeletion(t *testing.T) {
	t.Parallel()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	// Create a disconnected local branch with two commits:
	// 1. Adds two files
	// 2. Deletes one of them
	run("checkout", "--orphan", "temp-orphan")
	run("rm", "-rf", ".")

	// Commit 1: add two checkpoint files
	dir1 := filepath.Join(cloneDir, "ab", "cdef012345")
	require.NoError(t, os.MkdirAll(dir1, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "metadata.json"), []byte(`{"checkpoint_id":"abcdef012345"}`), 0o644))

	dir2 := filepath.Join(cloneDir, "cd", "ef01234567")
	require.NoError(t, os.MkdirAll(dir2, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "metadata.json"), []byte(`{"checkpoint_id":"cdef01234567"}`), 0o644))

	run("add", ".")
	run("commit", "-m", "Checkpoint: add two")

	// Commit 2: delete the second checkpoint
	require.NoError(t, os.RemoveAll(filepath.Join(cloneDir, "cd")))
	run("add", "-A")
	run("commit", "-m", "Checkpoint: remove second")

	run("branch", "-f", paths.MetadataBranchName, "temp-orphan")
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)

	err = ReconcileDisconnectedMetadataBranch(context.Background(), repo, metadataOriginRemoteRef(), io.Discard)
	require.NoError(t, err)

	// Verify merged tree: should have remote data + first local checkpoint,
	// but NOT the deleted second checkpoint
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	newRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	tipCommit, err := repo.CommitObject(newRef.Hash())
	require.NoError(t, err)
	tree, err := tipCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	require.NoError(t, checkpoint.FlattenTree(repo, tree, "", entries))

	// Remote data should be present
	assert.Contains(t, entries, "metadata.json", "remote data should be preserved")
	// First local checkpoint should be present
	assert.Contains(t, entries, "ab/cdef012345/metadata.json", "kept checkpoint should be present")
	// Second local checkpoint should be deleted
	assert.NotContains(t, entries, "cd/ef01234567/metadata.json", "deleted checkpoint should not be present")
}
