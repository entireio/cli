package fsstore

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

const (
	fsLinkSHAFallback = "b01b59663fd4860fd15a9939499be44a14dbf168"
	fsLinkSHARecorded = "5f2e1d0c9b8a7766554433221100ffeeddccbbaa"
)

// writeImported seeds a one-session imported checkpoint carrying a link.
func writeImported(t *testing.T, store *Store, cid id.CheckpointID, commitSHA, method string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), cp.Session{
		CheckpointID:    cid,
		SessionID:       "sess-1",
		Strategy:        "import",
		Kind:            importedSessionKind,
		Transcript:      redact.AlreadyRedacted([]byte("transcript")),
		CommitSHA:       commitSHA,
		CommitSHAMethod: method,
	}))
}

func assertLink(t *testing.T, store *Store, cid id.CheckpointID, wantSHA, wantMethod string) {
	t.Helper()
	ctx := context.Background()
	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, wantSHA, summary.CommitSHA, "root summary commit_sha")
	assert.Equal(t, wantMethod, summary.CommitSHAMethod, "root summary commit_sha_method")

	md, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, wantSHA, md.CommitSHA, "session metadata commit_sha")
	assert.Equal(t, wantMethod, md.CommitSHAMethod, "session metadata commit_sha_method")
}

func TestStore_CommitSHABackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	t.Run("write records the link, backfill upgrades it", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		writeImported(t, store, cid, fsLinkSHAFallback, cp.CommitSHAMethodFallback)
		assertLink(t, store, cid, fsLinkSHAFallback, cp.CommitSHAMethodFallback)

		require.NoError(t, store.Write(ctx, cp.CheckpointCommitSHA{
			CheckpointID: cid, SessionID: "sess-1",
			CommitSHA: fsLinkSHARecorded, Method: cp.CommitSHAMethodRecorded,
		}))
		assertLink(t, store, cid, fsLinkSHARecorded, cp.CommitSHAMethodRecorded)
	})

	t.Run("an empty session ID targets every imported session", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		writeImported(t, store, cid, "", "")

		require.NoError(t, store.Write(ctx, cp.CheckpointCommitSHA{
			CheckpointID: cid,
			CommitSHA:    fsLinkSHARecorded, Method: cp.CommitSHAMethodRecorded,
		}))
		assertLink(t, store, cid, fsLinkSHARecorded, cp.CommitSHAMethodRecorded)
	})

	t.Run("a refused downgrade rewrites nothing on disk", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		writeImported(t, store, cid, fsLinkSHARecorded, cp.CommitSHAMethodRecorded)
		before, err := os.ReadFile(store.path(cid))
		require.NoError(t, err)

		require.NoError(t, store.Write(ctx, cp.CheckpointCommitSHA{
			CheckpointID: cid, SessionID: "sess-1",
			CommitSHA: fsLinkSHAFallback, Method: cp.CommitSHAMethodHeuristic,
		}), "a refused downgrade is a no-op, not an error")
		assertLink(t, store, cid, fsLinkSHARecorded, cp.CommitSHAMethodRecorded)

		after, err := os.ReadFile(store.path(cid))
		require.NoError(t, err)
		assert.Equal(t, before, after, "a no-op backfill must leave the stored document byte-identical")
	})

	t.Run("reports not-found for an unknown checkpoint", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		err := store.Write(ctx, cp.CheckpointCommitSHA{
			CheckpointID: cid, CommitSHA: fsLinkSHARecorded, Method: cp.CommitSHAMethodRecorded,
		})
		require.ErrorIs(t, err, cp.ErrCheckpointNotFound)
	})
}
