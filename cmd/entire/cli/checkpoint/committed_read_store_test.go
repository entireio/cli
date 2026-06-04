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
func customRef() plumbing.ReferenceName { return plumbing.ReferenceName(paths.MetadataRefName) }
func originV1Ref() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
}

func customRefHash(t *testing.T, repo *git.Repository) (plumbing.Hash, bool) {
	t.Helper()
	ref, err := repo.Reference(customRef(), true)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return ref.Hash(), true
}

// writeV1Checkpoint writes a committed checkpoint to the v1 branch.
func writeV1Checkpoint(t *testing.T, repo *git.Repository, cpID id.CheckpointID) {
	t.Helper()
	require.NoError(t, NewGitStore(repo).WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		Prompts:      []string{"prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}

// enableV11 chdirs into dir and opts into checkpoints v1.1.
func enableV11(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	writeSettings(t, dir, `"1.1"`)
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
	assert.Equal(t, v1BranchRef(), NewGitStore(nil).CommittedReadRef())
	assert.Equal(t, customRef(), NewGitStoreWithRef(nil, customRef()).CommittedReadRef())
}

// Not parallel: uses t.Chdir() so settings.Load resolves the test repo.
func TestNewCommittedReadStore_SelectsRefByVersion(t *testing.T) {
	dir, repo, h := newTestRepo(t)
	setRef(t, repo, v1BranchRef(), h)
	t.Chdir(dir)

	writeSettings(t, dir, "") // v1 only
	assert.Equal(t, v1BranchRef(), NewCommittedReadStore(context.Background(), repo).CommittedReadRef())

	writeSettings(t, dir, `"1.1"`)
	assert.Equal(t, customRef(), NewCommittedReadStore(context.Background(), repo).CommittedReadRef())
}

// v1.1 reads always go through the custom ref as-is (no v1 fallback, and no
// read-time seeding from v1).
// Not parallel: subtests use t.Chdir().
func TestNewCommittedReadStore_V11Reads(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, dir string, repo *git.Repository) (wantCustomHash plumbing.Hash, wantCustomExists bool)
		wantFound bool
	}{
		{"reads metadata when custom ref points at v1", func(t *testing.T, _ string, repo *git.Repository) (plumbing.Hash, bool) {
			ref, err := repo.Reference(v1BranchRef(), true)
			require.NoError(t, err)
			setRef(t, repo, customRef(), ref.Hash())
			return ref.Hash(), true
		}, true},
		{"does not seed missing custom ref from local v1", func(_ *testing.T, _ string, _ *git.Repository) (plumbing.Hash, bool) {
			return plumbing.ZeroHash, false
		}, false},
		{"does not seed missing custom ref from origin v1", func(t *testing.T, _ string, repo *git.Repository) (plumbing.Hash, bool) {
			ref, err := repo.Reference(v1BranchRef(), true)
			require.NoError(t, err)
			setRef(t, repo, originV1Ref(), ref.Hash())
			require.NoError(t, repo.Storer.RemoveReference(v1BranchRef()))
			return plumbing.ZeroHash, false
		}, false},
		{"reads custom ref as-is when it differs from v1", func(t *testing.T, dir string, repo *git.Repository) (plumbing.Hash, bool) {
			hash := commitFile(t, repo, dir, "other.txt", "diverged", "diverged")
			setRef(t, repo, customRef(), hash)
			return hash, true
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, repo, _ := newTestRepo(t)
			enableV11(t, dir)
			cpID := id.MustCheckpointID("a1b2c3d4e5f6")
			writeV1Checkpoint(t, repo, cpID)
			wantCustomHash, wantCustomExists := tt.mutate(t, dir, repo)

			store := NewCommittedReadStore(context.Background(), repo)
			require.Equal(t, customRef(), store.CommittedReadRef(), "must read the custom ref, not fall back to v1")

			summary, err := store.ReadCommitted(context.Background(), cpID)
			require.NoError(t, err)
			if tt.wantFound {
				require.NotNil(t, summary)
				assert.Equal(t, cpID, summary.CheckpointID)
			} else {
				assert.Nil(t, summary, "must not fall back to v1")
			}

			gotCustomHash, gotCustomExists := customRefHash(t, repo)
			require.Equal(t, wantCustomExists, gotCustomExists)
			if wantCustomExists {
				assert.Equal(t, wantCustomHash, gotCustomHash)
			}
		})
	}
}
