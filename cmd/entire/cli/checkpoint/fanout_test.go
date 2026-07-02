package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// fakePrimary is a minimal PersistentStore that records writes and reports a
// fixed read result, so tests can assert read delegation and write fan-out.
type fakePrimary struct {
	writes   []WriteRequest
	writeErr error
	listErr  error
	listCall int
}

func (f *fakePrimary) Read(context.Context, id.CheckpointID) (*CheckpointSummary, error) {
	return &CheckpointSummary{}, nil
}

func (f *fakePrimary) List(context.Context) ([]CheckpointInfo, error) {
	f.listCall++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []CheckpointInfo{{}}, nil
}

func (f *fakePrimary) ReadSessionContent(context.Context, id.CheckpointID, int) (*SessionContent, error) {
	return &SessionContent{}, nil
}
func (f *fakePrimary) ReadSessionMetadata(context.Context, id.CheckpointID, int) (*Metadata, error) {
	return &Metadata{}, nil
}
func (f *fakePrimary) ReadSessionPrompts(context.Context, id.CheckpointID, int) (string, error) {
	return "", nil
}
func (f *fakePrimary) ReadSessionMetadataAndPrompts(context.Context, id.CheckpointID, int) (*Metadata, string, error) {
	return &Metadata{}, "", nil
}

func (f *fakePrimary) Write(_ context.Context, req WriteRequest) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, req)
	return nil
}

// fakePrimaryWithAuthor adds the optional AuthorReader capability.
type fakePrimaryWithAuthor struct {
	*fakePrimary

	author    Author
	authorErr error
}

func (f *fakePrimaryWithAuthor) GetCheckpointAuthor(context.Context, id.CheckpointID) (Author, error) {
	return f.author, f.authorErr
}

// fakeMirror records the writes it receives and can be made to fail.
type fakeMirror struct {
	writes   []WriteRequest
	writeErr error
}

func (m *fakeMirror) Write(_ context.Context, req WriteRequest) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.writes = append(m.writes, req)
	return nil
}

func TestFanout_NoMirrorsReturnsPrimaryUnwrapped(t *testing.T) {
	t.Parallel()

	primary := &fakePrimaryWithAuthor{fakePrimary: &fakePrimary{}, author: Author{Name: "A"}}
	store := newFanoutStore(primary, nil)

	// With no mirrors the primary is returned as-is — same value, no wrapper.
	assert.Same(t, any(primary), any(store))
}

func TestFanout_WriteFansOutToAllMirrors(t *testing.T) {
	t.Parallel()

	primary := &fakePrimary{}
	m1, m2 := &fakeMirror{}, &fakeMirror{}
	store := newFanoutStore(primary, []Writer{m1, m2})

	req := SessionSummary{CheckpointID: id.CheckpointID("abc123def456")}
	require.NoError(t, store.Write(context.Background(), req))

	assert.Len(t, primary.writes, 1)
	assert.Len(t, m1.writes, 1)
	assert.Len(t, m2.writes, 1)
}

func TestFanout_MirrorFailureDoesNotFailWrite(t *testing.T) {
	t.Parallel()

	primary := &fakePrimary{}
	failing := &fakeMirror{writeErr: errors.New("mirror down")}
	ok := &fakeMirror{}
	store := newFanoutStore(primary, []Writer{failing, ok})

	// Primary succeeded, so the operation succeeds even though a mirror failed,
	// and later mirrors still receive the write.
	require.NoError(t, store.Write(context.Background(), SessionSummary{}))
	assert.Len(t, primary.writes, 1)
	assert.Len(t, ok.writes, 1)
}

func TestFanout_PrimaryFailureSkipsMirrors(t *testing.T) {
	t.Parallel()

	primary := &fakePrimary{writeErr: errors.New("primary down")}
	mirror := &fakeMirror{}
	store := newFanoutStore(primary, []Writer{mirror})

	err := store.Write(context.Background(), SessionSummary{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary down")
	// The mirror must not be written when the primary write failed.
	assert.Empty(t, mirror.writes)
}

func TestFanout_ReadsDelegateToPrimary(t *testing.T) {
	t.Parallel()

	primary := &fakePrimary{}
	store := newFanoutStore(primary, []Writer{&fakeMirror{}})

	_, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, primary.listCall)
}

func TestFanout_PreservesAuthorReaderWhenPrimaryHasIt(t *testing.T) {
	t.Parallel()

	primary := &fakePrimaryWithAuthor{fakePrimary: &fakePrimary{}, author: Author{Name: "Ada", Email: "ada@example.com"}}
	store := newFanoutStore(primary, []Writer{&fakeMirror{}})

	author, ok := store.(AuthorReader)
	require.True(t, ok, "fan-out wrapper should expose AuthorReader when primary does")
	got, err := author.GetCheckpointAuthor(context.Background(), id.CheckpointID("abc123def456"))
	require.NoError(t, err)
	assert.Equal(t, "Ada", got.Name)
}

func TestFanout_OmitsAuthorReaderWhenPrimaryLacksIt(t *testing.T) {
	t.Parallel()

	primary := &fakePrimary{} // no GetCheckpointAuthor
	store := newFanoutStore(primary, []Writer{&fakeMirror{}})

	_, ok := store.(AuthorReader)
	assert.False(t, ok, "fan-out wrapper must not advertise AuthorReader when primary lacks it")
}
