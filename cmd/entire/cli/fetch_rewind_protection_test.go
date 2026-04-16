package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetchMetadataBranch_DoesNotRewindLocalAhead is a regression test for the
// data-loss bug where the fetch unconditionally SetReferences the local
// metadata branch to origin's tip, orphaning any locally-ahead commits. This
// happens in the "post-condense, pre-push" window — or whenever pre-push
// fails. The buggy behavior leaves the local branch at origin's hash A while
// the user's most recent checkpoint B (and everything reachable only from B)
// becomes unreachable and subject to GC.
//
// Uses t.Chdir — not parallel.
func TestFetchMetadataBranch_DoesNotRewindLocalAhead(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "bare.git")
	localDir := filepath.Join(tmpDir, "local")

	runGit(t, tmpDir, "init", "--bare", bareDir)

	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "hello")
	testutil.GitAdd(t, localDir, "README.md")
	testutil.GitCommit(t, localDir, "init")
	runGit(t, localDir, "remote", "add", "origin", bareDir)

	// Push the working branch + metadata branch (starts at same commit as HEAD).
	runGit(t, localDir, "branch", paths.MetadataBranchName)
	runGit(t, localDir, "push", "origin", "HEAD:refs/heads/main", paths.MetadataBranchName)
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	aHash := revParse(t, localDir, paths.MetadataBranchName)

	// Advance local metadata branch by one commit (B), without pushing. This
	// simulates the window where condensation has produced a local checkpoint
	// commit but pre-push has not yet run (or failed).
	runGit(t, localDir, "checkout", paths.MetadataBranchName)
	testutil.WriteFile(t, localDir, "metadata-b.txt", "checkpoint B")
	testutil.GitAdd(t, localDir, "metadata-b.txt")
	testutil.GitCommit(t, localDir, "checkpoint B")
	bHash := revParse(t, localDir, paths.MetadataBranchName)
	require.NotEqual(t, aHash, bHash, "test setup: local metadata branch should have advanced beyond remote")
	// Go back to main so CWD isn't sitting on the orphan-ish metadata branch.
	runGit(t, localDir, "checkout", "--quiet", "main")

	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`),
		0o644,
	))

	t.Chdir(localDir)

	require.NoError(t, FetchMetadataBranch(ctx))

	afterFetch := revParse(t, localDir, paths.MetadataBranchName)
	assert.Equal(t, bHash, afterFetch,
		"FetchMetadataBranch must not rewind a locally-ahead metadata branch; expected %s (B), got %s (likely rewound to A=%s)",
		bHash, afterFetch, aHash)
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s failed: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}
