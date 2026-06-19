package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// newTestRepo creates an isolated repo with a single "init" commit and returns
// its directory, an open handle, and the commit hash.
func newTestRepo(t *testing.T) (string, *git.Repository, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return dir, repo, commitFile(t, repo, dir, "f.txt", "init", "init")
}

// commitFile commits content to path; successive calls build a linear chain.
func commitFile(t *testing.T, repo *git.Repository, dir, path, content, msg string) plumbing.Hash {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(path)
	require.NoError(t, err)
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@test.com"}})
	require.NoError(t, err)
	return h
}

func setRef(t *testing.T, repo *git.Repository, name plumbing.ReferenceName, hash plumbing.Hash) {
	t.Helper()
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(name, hash)))
}

func v1BranchRef() plumbing.ReferenceName {
	return plumbing.NewBranchReferenceName(paths.MetadataBranchName)
}

// writeSettings writes .entire/settings.json (empty version omits the option).
func writeSettings(t *testing.T, dir, version string) {
	t.Helper()
	body := `{"enabled": true}`
	if version != "" {
		body = `{"enabled": true, "strategy_options": {"checkpoints_version": ` + version + `}}`
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", paths.SettingsFileName), []byte(body), 0o644))
}

func TestGitStore_CommittedReadRef(t *testing.T) {
	t.Parallel()
	assert.Equal(t, v1BranchRef(), NewGitStore(nil, DefaultV1Refs()).CommittedReadRef())

	syntheticRead := plumbing.ReferenceName("refs/entire/checkpoints/synthetic-read")
	refs := CommittedRefs{
		Primary: v1BranchRef(),
		Read:    syntheticRead,
		Push:    []plumbing.ReferenceName{v1BranchRef()},
	}
	assert.Equal(t, syntheticRead, NewGitStore(nil, refs).CommittedReadRef())
}

// Not parallel: WriteCommitted touches repo refs.
func TestGitStore_WriteCommittedTargetsPrimary(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)

	synthetic := plumbing.ReferenceName("refs/entire/checkpoints/synthetic-primary")
	refs := CommittedRefs{Primary: synthetic, Read: synthetic, Push: []plumbing.ReferenceName{synthetic}}
	store := NewGitStore(repo, refs)

	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	require.NoError(t, store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		Prompts:      []string{"prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))

	ref, err := repo.Reference(synthetic, true)
	require.NoError(t, err, "synthetic primary ref must exist after write")
	assert.NotEqual(t, plumbing.ZeroHash, ref.Hash())

	_, err = repo.Reference(v1BranchRef(), true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "v1 branch must not be touched when Primary is synthetic")
}

func TestNewGitStore_UsesRefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	synthetic := plumbing.ReferenceName("refs/entire/checkpoints/synthetic")
	refs := CommittedRefs{Primary: synthetic, Read: synthetic, Push: []plumbing.ReferenceName{synthetic}}
	store := NewGitStore(repo, refs)
	assert.Equal(t, synthetic, store.CommittedReadRef())
	assert.Equal(t, refs, store.Refs())
}

// Not parallel: uses t.Chdir() to exercise on-disk settings being ignored.
func TestNewGitStore_IgnoresCheckpointsVersion(t *testing.T) {
	dir, repo, h := newTestRepo(t)
	setRef(t, repo, v1BranchRef(), h)
	t.Chdir(dir)

	writeSettings(t, dir, "") // v1 only
	assert.Equal(t, v1BranchRef(), NewGitStore(repo, ResolveCommittedRefs(context.Background())).CommittedReadRef())

	writeSettings(t, dir, `"1.1"`)
	assert.Equal(t, v1BranchRef(), NewGitStore(repo, ResolveCommittedRefs(context.Background())).CommittedReadRef())
}
