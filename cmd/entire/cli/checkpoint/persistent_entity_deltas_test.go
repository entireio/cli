package checkpoint

import (
	"context"
	"strconv"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entityDeltasDoc is a stand-in for the producer's document. The store treats
// it as opaque bytes, so its shape is irrelevant here beyond being identifiable.
const entityDeltasDoc = `{"schema_version":"1.0","entities":[]}` + "\n"

func writeEntityDeltasSession(t *testing.T, store *GitStore, cpID id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("line\n")),
		Prompts:      []string{"prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}

// The document must land in the directory of the session it describes, not in
// "the latest" one: a checkpoint holds several sessions and each has its own
// deltas.
func TestWriteSessionEntityDeltas_TargetsTheNamedSession(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	writeEntityDeltasSession(t, store, cpID, "session-000")
	writeEntityDeltasSession(t, store, cpID, "session-001")

	require.NoError(t, store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: cpID,
		SessionID:    "session-000",
		Document:     []byte(entityDeltasDoc),
	}))

	got, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.EntityDeltasFileName)
	require.True(t, ok, "deltas must land in session 0's directory")
	assert.JSONEq(t, entityDeltasDoc, got)

	_, ok = readBranchFile(t, store, cpID.Path()+"/1/"+paths.EntityDeltasFileName)
	assert.False(t, ok, "session 1 wrote no deltas and must not have acquired any")

	// Both sessions' own documents survive the backfill untouched.
	for i := range 2 {
		_, ok := readBranchFile(t, store, cpID.Path()+"/"+strconv.Itoa(i)+"/"+paths.MetadataFileName)
		require.True(t, ok)
	}

	// Each session's deltas arrive in their own backfill (one detached child
	// per session), so the second must not overwrite the first.
	second := `{"schema_version":"1.0","entities":[{"name":"F"}]}` + "\n"
	require.NoError(t, store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Document:     []byte(second),
	}))

	got, ok = readBranchFile(t, store, cpID.Path()+"/0/"+paths.EntityDeltasFileName)
	require.True(t, ok, "session 0's deltas must survive session 1's backfill")
	assert.JSONEq(t, entityDeltasDoc, got)
	got, ok = readBranchFile(t, store, cpID.Path()+"/1/"+paths.EntityDeltasFileName)
	require.True(t, ok)
	assert.Equal(t, second, got)
}

func TestWriteSessionEntityDeltas_UnknownCheckpointIsNotFound(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	require.NoError(t, store.ensureSessionsBranch(context.Background()))

	err := store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: id.MustCheckpointID("000000000000"),
		SessionID:    "session-000",
		Document:     []byte(entityDeltasDoc),
	})
	assert.ErrorIs(t, err, ErrCheckpointNotFound)
}

// A session miss is a hard error, NOT the not-found sentinel: falling through to
// another backend would be wrong, and there is no slot to append into.
func TestWriteSessionEntityDeltas_UnknownSessionErrorsWithoutMovingTheRef(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	writeEntityDeltasSession(t, store, cpID, "session-000")

	before, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	err = store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: cpID,
		SessionID:    "session-does-not-exist",
		Document:     []byte(entityDeltasDoc),
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCheckpointNotFound,
		"a session miss must not read as a missing checkpoint")

	after, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, before.Hash(), after.Hash(), "a failed backfill must not move the branch")
}

// The git-refs backend keeps the checkpoint at the root of its own ref, so the
// same applier has to work with an empty base path.
func TestRefsStore_WriteSessionEntityDeltas(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	cid := id.MustCheckpointID("b1b2c3d4e5f6")
	refsWrite(t, store, cid, "session-000", "line\n")

	require.NoError(t, store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: cid,
		SessionID:    "session-000",
		Document:     []byte(entityDeltasDoc),
	}))

	refName, err := RefName(cid)
	require.NoError(t, err)
	ref, err := store.repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	file, err := tree.File("0/" + paths.EntityDeltasFileName)
	require.NoError(t, err)
	contents, err := file.Contents()
	require.NoError(t, err)
	assert.JSONEq(t, entityDeltasDoc, contents)
}

// refRaceStorer interposes another writer's ref advance into the exact window
// the backfill loses: between reading the tip and writing its own commit.
//
// It fires on the FIRST ref set of either kind, so the same test proves the
// compare-and-swap and detects its removal: with the CAS, the delegated call
// conflicts and the backfill rebuilds; with a plain SetReference, the delegated
// call force-lands on the stale parent and the interposed write is orphaned.
type refRaceStorer struct {
	storage.Storer

	advance func()
	fired   bool
}

func (s *refRaceStorer) interpose() {
	if s.fired || s.advance == nil {
		return
	}
	s.fired = true
	s.advance()
}

func (s *refRaceStorer) SetReference(ref *plumbing.Reference) error {
	s.interpose()

	return s.Storer.SetReference(ref)
}

func (s *refRaceStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	s.interpose()

	return s.Storer.CheckAndSetReference(newRef, old)
}

// The proven orphaning scenario, deterministically: the detached backfill child
// reads the v1 tip, condensation lands another session's checkpoint on it, and
// the child then writes. Both checkpoints must survive and the deltas must sit
// on the new tip — a force-set would replace the winner's commit with one built
// on the stale parent and silently drop it.
func TestBackfillEntityDeltas_TipMovedUnderTheWrite_KeepsBothCheckpoints(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	mine := id.MustCheckpointID("a1b2c3d4e5f6")
	theirs := id.MustCheckpointID("b2c3d4e5f6a1")

	writeEntityDeltasSession(t, store, mine, "session-000")
	before, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	repo.Storer = &refRaceStorer{Storer: repo.Storer, advance: func() {
		writeEntityDeltasSession(t, store, theirs, "session-001")
	}}

	require.NoError(t, store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: mine,
		SessionID:    "session-000",
		Document:     []byte(entityDeltasDoc),
	}))

	got, ok := readBranchFile(t, store, mine.Path()+"/0/"+paths.EntityDeltasFileName)
	require.True(t, ok, "the deltas must land even though the tip moved")
	assert.JSONEq(t, entityDeltasDoc, got)

	_, ok = readBranchFile(t, store, theirs.Path()+"/0/"+paths.MetadataFileName)
	assert.True(t, ok, "the checkpoint that won the race must survive the backfill")
	_, ok = readBranchFile(t, store, mine.Path()+"/0/"+paths.MetadataFileName)
	assert.True(t, ok, "the backfill's own checkpoint must still be there")

	after, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash(), after.Hash(), "the backfill must have advanced the branch")
}

// The git-refs backend races on the checkpoint's own ref instead of the branch:
// a second session of the same checkpoint condensing mid-backfill moves exactly
// the ref the backfill is writing.
func TestRefsStore_BackfillEntityDeltas_TipMovedUnderTheWrite_KeepsBothSessions(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	cid := id.MustCheckpointID("b1b2c3d4e5f6")
	refsWrite(t, store, cid, "session-000", "line\n")

	store.repo.Storer = &refRaceStorer{Storer: store.repo.Storer, advance: func() {
		refsWrite(t, store, cid, "session-001", "other\n")
	}}

	require.NoError(t, store.Write(context.Background(), SessionEntityDeltas{
		CheckpointID: cid,
		SessionID:    "session-000",
		Document:     []byte(entityDeltasDoc),
	}))

	refName, err := RefName(cid)
	require.NoError(t, err)
	ref, err := store.repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	file, err := tree.File("0/" + paths.EntityDeltasFileName)
	require.NoError(t, err, "the deltas must land even though the ref moved")
	contents, err := file.Contents()
	require.NoError(t, err)
	assert.JSONEq(t, entityDeltasDoc, contents)

	_, err = tree.File("1/" + paths.MetadataFileName)
	require.NoError(t, err, "the session that won the race must survive the backfill")
}
