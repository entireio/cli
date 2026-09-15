package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestCapPromptAttributions_KeepsFittingPayload(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[{"checkpoint_number":1,"user_lines_added":3}]`)
	assert.Equal(t, raw, CapPromptAttributions(context.Background(), raw, "s1"))
	assert.Nil(t, CapPromptAttributions(context.Background(), nil, "s1"))
}

func TestCapPromptAttributions_DropsOversizedPayload(t *testing.T) {
	t.Parallel()
	// One byte over the cap: the field is dropped rather than truncated, since a
	// clipped JSON array is worse than an absent diagnostic.
	raw := json.RawMessage(bytes.Repeat([]byte("x"), MaxPromptAttributionsBytes+1))
	assert.Nil(t, CapPromptAttributions(context.Background(), raw, "s1"))

	exact := json.RawMessage(bytes.Repeat([]byte("x"), MaxPromptAttributionsBytes))
	assert.Equal(t, exact, CapPromptAttributions(context.Background(), exact, "s1"), "the cap is inclusive")
}

// oversizedPromptAttributions is a syntactically valid prompt_attributions
// payload just over the cap, the shape a pre-v0.10.1 nested-checkout walk left
// behind.
func oversizedPromptAttributions(t *testing.T) json.RawMessage {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`[{"checkpoint_number":1,"user_added_per_file":{`)
	for i := 0; b.Len() < MaxPromptAttributionsBytes+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `".claude/worktrees/agent/pkg/file%d.go":3`, i)
	}
	b.WriteString(`}}]`)
	raw := json.RawMessage(b.Bytes())
	require.True(t, json.Valid(raw))
	return raw
}

// sessionMetadataAt reads <checkpoint>/<session index>/metadata.json from the
// branch tip as a generic document, so a test can assert on key presence.
func sessionMetadataAt(t *testing.T, store *GitStore, cpID id.CheckpointID, sessionIndex int) map[string]json.RawMessage {
	t.Helper()
	ref, err := store.repo.Reference(store.refs.Primary, true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	f, err := commit.File(fmt.Sprintf("%s/%d/%s", cpID.Path(), sessionIndex, paths.MetadataFileName))
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(content), &doc))
	return doc
}

// The cap is applied at the writer boundary, not only in the helper: a
// session written with an oversized diagnostic lands without the field, and a
// session with a small one keeps it.
func TestGitStoreWrite_CapsPromptAttributionsInStoredMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()

	write := func(cpID string, attrs json.RawMessage) id.CheckpointID {
		checkpointID := id.MustCheckpointID(cpID)
		require.NoError(t, store.Write(ctx, Session{
			CheckpointID:           checkpointID,
			SessionID:              "session-" + cpID,
			Strategy:               "manual-commit",
			Transcript:             redact.AlreadyRedacted([]byte(`{"role":"user","content":"hi"}` + "\n")),
			Prompts:                []string{"hi"},
			AuthorName:             "Test",
			AuthorEmail:            "test@test.com",
			PromptAttributionsJSON: attrs,
		}))
		return checkpointID
	}

	big := write("aaaaaaaaaaaa", oversizedPromptAttributions(t))
	doc := sessionMetadataAt(t, store, big, 0)
	assert.NotContains(t, doc, "prompt_attributions", "an oversized diagnostic is dropped at the write boundary")
	assert.Contains(t, doc, "session_id", "the rest of the metadata is intact")

	small := write("bbbbbbbbbbbb", json.RawMessage(`[{"checkpoint_number":1,"user_lines_added":2}]`))
	doc = sessionMetadataAt(t, store, small, 0)
	assert.JSONEq(t, `[{"checkpoint_number":1,"user_lines_added":2}]`, string(doc["prompt_attributions"]), "a fitting diagnostic is kept verbatim")
}

// The finalize/backfill path re-marshals metadata an older CLI wrote. Without
// the cap there, a pre-v0.10.1 oversized field would be copied into every later
// rewrite of that checkpoint.
func TestUpdateSessionMetadata_CapsPromptAttributionsFromOlderWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	w := &treeWriter{repo: repo}

	existing, err := json.Marshal(map[string]any{
		"session_id":          "legacy",
		"prompt_attributions": oversizedPromptAttributions(t),
	})
	require.NoError(t, err)
	blob, err := CreateBlobFromContent(repo, existing)
	require.NoError(t, err)
	const sessionDir = "ab/cdef000001/0"
	metadataPath := checkpointSubtreePath(sessionDir, paths.MetadataFileName)
	entries := map[string]object.TreeEntry{
		metadataPath: {Name: metadataPath, Mode: filemode.Regular, Hash: blob},
	}

	require.NoError(t, w.updateSessionMetadata(sessionDir, entries, func(m *Metadata) {
		m.CompactTranscriptStart = new(int)
	}))

	assert.NotEqual(t, blob, entries[metadataPath].Hash, "the blob was rewritten")
	updated, err := readJSONFromBlob[map[string]json.RawMessage](repo, entries[metadataPath].Hash)
	require.NoError(t, err)
	assert.NotContains(t, *updated, "prompt_attributions")
	assert.Contains(t, *updated, "session_id")
	assert.Contains(t, *updated, "compact_transcript_start", "the caller's mutation is applied")
}
