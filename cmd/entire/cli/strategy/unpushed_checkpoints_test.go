package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initCountTestRepo initializes an isolated repo with two commits and returns
// the repo dir plus the two commit hashes (first, second).
func initCountTestRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "one")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "first")
	first = testutil.GetHeadHash(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "two")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "second")
	second = testutil.GetHeadHash(t, dir)
	return dir, first, second
}

// writeGitRefsBackendSetting writes .entire/settings.json selecting the
// git-refs primary checkpoint backend.
func writeGitRefsBackendSetting(t *testing.T, dir string) {
	t.Helper()
	writeSettingsJSON(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}}}`)
}

const v1LocalRef = "refs/heads/" + paths.MetadataBranchName

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitBranch_NoV1Branch(t *testing.T) {
	dir, _, _ := initCountTestRepo(t)
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitBranch_TrackingRefEqual(t *testing.T) {
	dir, _, second := initCountTestRepo(t)
	testutil.GitUpdateRef(t, dir, v1LocalRef, second)
	testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, second)
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitBranch_LocalAhead(t *testing.T) {
	dir, first, second := initCountTestRepo(t)
	testutil.GitUpdateRef(t, dir, v1LocalRef, second)
	testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, first)
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 1, got, "one v1 commit ahead of the tracking ref")
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitBranch_TrackingRefAbsentCountsAll(t *testing.T) {
	dir, _, second := initCountTestRepo(t)
	testutil.GitUpdateRef(t, dir, v1LocalRef, second)
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 2, got, "absent tracking ref counts every v1 commit (deferred-publish reading)")
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitBranch_ExcludesMetadataRefInitCommit(t *testing.T) {
	dir, _, _ := initCountTestRepo(t)
	testutil.WriteFile(t, dir, "f.txt", "three")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, metadataRefInitSubject)
	testutil.GitUpdateRef(t, dir, v1LocalRef, testutil.GetHeadHash(t, dir))
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 2, got, "the orphan init commit seeds the branch but is not a checkpoint")
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitRefs_EmptyQueue(t *testing.T) {
	dir, _, _ := initCountTestRepo(t)
	writeGitRefsBackendSetting(t, dir)
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

// Not parallel: uses t.Chdir()
func TestCountUnpushedCheckpoints_GitRefs_QueueLength(t *testing.T) {
	dir, _, _ := initCountTestRepo(t)
	writeGitRefsBackendSetting(t, dir)

	// The v1 branch exists but must NOT be counted: the git-refs backend
	// counts the push queue, not v1 commits.
	testutil.GitUpdateRef(t, dir, v1LocalRef, testutil.GetHeadHash(t, dir))

	queue := checkpoint.NewPushQueue(filepath.Join(dir, ".git"))
	require.NoError(t, queue.Enqueue(plumbing.ReferenceName("refs/entire/checkpoints/aa/bb0000000001")))
	require.NoError(t, queue.Enqueue(plumbing.ReferenceName("refs/entire/checkpoints/aa/bb0000000002")))
	t.Chdir(dir)

	got, err := CountUnpushedCheckpoints(context.Background(), "origin")
	require.NoError(t, err)
	assert.Equal(t, 2, got, "git-refs backend counts queued refs")
}
