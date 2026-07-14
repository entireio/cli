package cli

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// seedTreelessFetchRepo builds a bare origin with a `main` commit and a 2-commit
// orphan entire/checkpoints/v1 branch, then makes a single-branch file:// clone
// of main (a real fetch-pack, not the local hardlink optimization, so the
// metadata branch and its history are absent until fetched). Returns the clone
// dir and the origin metadata tip. The caller is responsible for t.Chdir.
func seedTreelessFetchRepo(t *testing.T) (clonedDir, originTip string) {
	t.Helper()
	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "bare.git")
	localDir := filepath.Join(tmpDir, "local")

	runGit(t, tmpDir, "init", "--bare", bareDir)

	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "hello")
	testutil.GitAdd(t, localDir, "README.md")
	testutil.GitCommit(t, localDir, "init")
	runGit(t, localDir, "branch", "-M", "main")
	runGit(t, localDir, "remote", "add", "origin", bareDir)
	runGit(t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runGit(t, localDir, "rm", "-rf", ".")
	testutil.WriteFile(t, localDir, "a/metadata.json", `{"checkpoint_id":"deadbeef0001"}`)
	testutil.GitAdd(t, localDir, "a/metadata.json")
	testutil.GitCommit(t, localDir, "Checkpoint: deadbeef0001")
	testutil.WriteFile(t, localDir, "b/metadata.json", `{"checkpoint_id":"deadbeef0002"}`)
	testutil.GitAdd(t, localDir, "b/metadata.json")
	testutil.GitCommit(t, localDir, "Checkpoint: deadbeef0002")
	runGit(t, localDir, "checkout", "main")
	runGit(t, localDir, "push", "origin", "HEAD:refs/heads/main", paths.MetadataBranchName)
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	originTip = gitOutput(t, bareDir, "rev-parse", "refs/heads/"+paths.MetadataBranchName)

	clonedDir = filepath.Join(tmpDir, "cloned")
	runGit(t, tmpDir, "clone", "--single-branch", "--branch", "main", "file://"+bareDir, clonedDir)
	runGit(t, clonedDir, "config", "user.email", "test@example.com")
	runGit(t, clonedDir, "config", "user.name", "Test")
	return clonedDir, originTip
}

func repoIsShallow(t *testing.T, dir string) bool {
	t.Helper()
	return gitOutput(t, dir, "rev-parse", "--is-shallow-repository") == "true"
}

// TestFetchMetadataTreeOnly_DoesNotShallowRepo is a regression test for the
// shallow-metadata false-disconnect.
//
// FetchMetadataTreeOnly resolves the latest checkpoint on resume/explain/attach.
// It used to fetch with --depth=1, which adds the fetched tip to .git/shallow.
// Once the metadata tip is a shallow boundary, a later `git merge-base` against
// refs/remotes/origin/entire/checkpoints/v1 can't reach the real common
// ancestor (it's below the boundary) and the disconnection check falsely
// reports "no common ancestor" — aborting push and looping doctor.
//
// The fix drops --depth=1 and relies on blob filtering for cheapness, so the
// fetch never creates a shallow boundary.
func TestFetchMetadataTreeOnly_DoesNotShallowRepo(t *testing.T) {
	// Uses t.Chdir() — cannot run in parallel.
	clonedDir, originTip := seedTreelessFetchRepo(t)
	t.Chdir(clonedDir)

	if err := FetchMetadataTreeOnly(t.Context()); err != nil {
		t.Fatalf("FetchMetadataTreeOnly: %v", err)
	}

	// The fix: the tip-read must not leave the repo shallow. Under the old
	// --depth=1 behavior this would be shallow.
	if repoIsShallow(t, clonedDir) {
		t.Errorf("repo is shallow after tree-only fetch; the tip-read must not create a shallow boundary")
	}

	// The full metadata history is present (two commits), not truncated to one.
	originRef := "refs/remotes/origin/" + paths.MetadataBranchName
	if n := gitOutput(t, clonedDir, "rev-list", "--count", originRef); n != "2" {
		t.Errorf("origin metadata history has %s commit(s), want 2 (full depth)", n)
	}

	// The local primary ref is advanced to the tip so reads work.
	localRef := "refs/heads/" + paths.MetadataBranchName
	if got := gitOutput(t, clonedDir, "rev-parse", localRef); got != originTip {
		t.Errorf("local primary ref %s = %q, want origin tip %q", localRef, got, originTip)
	}
}

// TestFetchMetadataTreeOnly_HealsPriorShallow verifies that a repo already
// shallowed by an older CLI (a lingering --depth=1 boundary on the metadata
// branch) is unshallowed by the tip-read, so the poison doesn't persist
// indefinitely for users who ran the buggy version.
func TestFetchMetadataTreeOnly_HealsPriorShallow(t *testing.T) {
	// Uses t.Chdir() — cannot run in parallel.
	clonedDir, _ := seedTreelessFetchRepo(t)

	// Reproduce the old behavior: a --depth=1 fetch grafts the metadata tip into
	// .git/shallow, marking the repo shallow.
	runGit(t, clonedDir, "fetch", "--depth=1", "origin",
		"+refs/heads/"+paths.MetadataBranchName+":refs/remotes/origin/"+paths.MetadataBranchName)
	if !repoIsShallow(t, clonedDir) {
		t.Fatal("precondition: expected a shallow repo after --depth=1 fetch")
	}

	t.Chdir(clonedDir)
	if err := FetchMetadataTreeOnly(t.Context()); err != nil {
		t.Fatalf("FetchMetadataTreeOnly: %v", err)
	}

	if repoIsShallow(t, clonedDir) {
		t.Errorf("repo still shallow after tree-only fetch; a prior --depth=1 boundary must be healed")
	}
	originRef := "refs/remotes/origin/" + paths.MetadataBranchName
	if n := gitOutput(t, clonedDir, "rev-list", "--count", originRef); n != "2" {
		t.Errorf("metadata history = %s commit(s) after heal, want 2 (full depth)", n)
	}
}
