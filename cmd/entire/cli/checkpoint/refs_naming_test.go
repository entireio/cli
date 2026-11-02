package checkpoint

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// mustRefName is a test helper for the common case of a known-valid checkpoint ID.
func mustRefName(t *testing.T, cid id.CheckpointID) plumbing.ReferenceName {
	t.Helper()
	ref, err := RefName(cid)
	require.NoError(t, err)
	return ref
}

func TestRefName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cid  id.CheckpointID
		want plumbing.ReferenceName
	}{
		{
			name: "legacy hex shards on last two",
			cid:  "a1b2c3d4e5f6",
			want: "refs/entire/checkpoints/f6/a1b2c3d4e5f6",
		},
		{
			name: "ulid shards on last two",
			cid:  "01KVBJCWYA4YW6J5M9GP655HZN",
			want: "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RefName(tt.cid)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRefName_RejectsInvalidID(t *testing.T) {
	t.Parallel()
	for _, cid := range []id.CheckpointID{"", "not-an-id", "A1B2C3D4E5F6"} {
		_, err := RefName(cid)
		assert.Error(t, err, "RefName(%q) should error rather than build a malformed ref", cid)
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ref    plumbing.ReferenceName
		wantID id.CheckpointID
		wantOK bool
	}{
		{
			name:   "legacy round-trip",
			ref:    "refs/entire/checkpoints/f6/a1b2c3d4e5f6",
			wantID: "a1b2c3d4e5f6",
			wantOK: true,
		},
		{
			name:   "ulid round-trip",
			ref:    "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN",
			wantID: "01KVBJCWYA4YW6J5M9GP655HZN",
			wantOK: true,
		},
		{
			name:   "wrong prefix",
			ref:    "refs/heads/entire/checkpoints/v1",
			wantOK: false,
		},
		{
			name:   "shard does not match id (wrong bucket)",
			ref:    "refs/entire/checkpoints/a1/a1b2c3d4e5f6",
			wantOK: false,
		},
		{
			name:   "extra path segment",
			ref:    "refs/entire/checkpoints/f6/a1b2c3d4e5f6/0",
			wantOK: false,
		},
		{
			name:   "missing id",
			ref:    "refs/entire/checkpoints/a1/",
			wantOK: false,
		},
		{
			name:   "missing shard separator",
			ref:    "refs/entire/checkpoints/a1b2c3d4e5f6",
			wantOK: false,
		},
		{
			name:   "prefix only",
			ref:    "refs/entire/checkpoints/",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotOK := ParseRef(tt.ref)
			assert.Equal(t, tt.wantOK, gotOK)
			if tt.wantOK {
				assert.Equal(t, tt.wantID, gotID)
				// Round-trip: building the ref from the parsed ID reproduces it.
				assert.Equal(t, tt.ref, mustRefName(t, gotID))
			} else {
				assert.Equal(t, id.EmptyCheckpointID, gotID)
			}
		})
	}
}

// TestParseRef_ToleratesCaseFoldedShardDirectory reproduces the
// case-insensitive-filesystem shard collision (macOS APFS / Windows NTFS
// defaults): git resolves a new shard directory against existing ones
// case-insensitively, so a checkpoint ref can be written under a shard
// directory whose case differs from a fresh ShardFor() computation on that
// same ID — e.g. a ULID's canonical uppercase shard ("6B") folded into an
// already-present lowercase directory ("6b") left by an unrelated legacy hex
// checkpoint. ParseRef must still recognize such a ref: the underlying
// checkpoint object is intact, and rejecting it as malformed makes it
// permanently invisible to `entire checkpoint list`/`explain`.
//
// These cases are deliberately kept out of TestParseRef's table: unlike every
// other well-formed case there, RefName(gotID) recomputes the canonical
// (non-folded) shard and would not reproduce the folded ref under test, so the
// table's round-trip assertion does not apply here.
func TestParseRef_ToleratesCaseFoldedShardDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ref    plumbing.ReferenceName
		wantID id.CheckpointID
	}{
		{
			name:   "ulid ref folded into a case-colliding lowercase shard dir",
			ref:    "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B",
			wantID: "01M2DCHJCHTR9T9MZTSB7WV76B",
		},
		{
			// The mirror image: an uppercase shard directory holding an ID
			// whose freshly-computed shard is lowercase.
			name:   "legacy hex ref folded into a case-colliding uppercase shard dir",
			ref:    "refs/entire/checkpoints/6B/0da4f302686b",
			wantID: "0da4f302686b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotOK := ParseRef(tt.ref)
			require.True(t, gotOK, "ParseRef(%q) should accept a case-folded shard directory", tt.ref)
			assert.Equal(t, tt.wantID, gotID)
		})
	}
}
