package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestWriteCommitted_DoesNotEmitOPFAppliedTrailer is the regression guard
// for the architectural promise: standard post-commit condensation writes
// 7-layer-only blobs and MUST NOT mark them with the Entire-OPF-Applied
// trailer. The trailer is emitted exclusively by the pre-push rewrite
// path; if a future change accidentally added it to the standard writer,
// the pre-push rewrite would skip those commits (HasOPFApplied true →
// reparent-only, no actual OPF run) and ship 7-layer content as if it
// were 8-layer. This test pins down that contract.
func TestWriteCommitted_DoesNotEmitOPFAppliedTrailer(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	readmeFile := filepath.Join(tempDir, "README.md")
	require.NoError(t, os.WriteFile(readmeFile, []byte("# Test"), 0o644))
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	err = store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "regression-no-opf-trailer",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"role":"user","content":"hello"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Read the latest commit message on entire/checkpoints/v1 and assert
	// HasOPFApplied is false. We resolve via the ref then walk back the
	// single commit the writer just produced.
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("entire/checkpoints/v1"), true)
	require.NoError(t, err, "writer should have created entire/checkpoints/v1")
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	if trailers.HasOPFApplied(commit.Message) {
		t.Errorf("standard WriteCommitted emitted Entire-OPF-Applied trailer; commit message:\n%s", commit.Message)
	}
}
