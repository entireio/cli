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

// TestParseRef_CaseInsensitiveShard covers #2401: on a case-insensitive
// filesystem (macOS APFS, Windows NTFS) a ULID checkpoint whose canonical shard
// is "6B" is written into a pre-existing legacy-hex "6b" directory, and the ref
// must still parse back to its ID instead of becoming permanently invisible.
func TestParseRef_CaseInsensitiveShard(t *testing.T) {
	t.Parallel()

	// The exact case from the issue report: ULID sharding on "6B", stored under
	// a legacy "6b" directory.
	const reported id.CheckpointID = "01M2DCHJCHTR9T9MZTSB7WV76B"
	require.Equal(t, "6B", reported.ShardFor(), "precondition: canonical shard is uppercase")

	gotID, ok := ParseRef("refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B")
	assert.True(t, ok, "a ref whose shard differs only in case must still parse")
	assert.Equal(t, reported, gotID)

	// The mirror case: a lowercase legacy ID stored under an uppercase directory.
	gotID, ok = ParseRef("refs/entire/checkpoints/F6/a1b2c3d4e5f6")
	assert.True(t, ok, "a lowercase ID under an uppercase shard directory must still parse")
	assert.Equal(t, id.CheckpointID("a1b2c3d4e5f6"), gotID)

	// Folding the case must not weaken the guard: a genuinely different bucket
	// is still rejected.
	_, ok = ParseRef("refs/entire/checkpoints/a1/01M2DCHJCHTR9T9MZTSB7WV76B")
	assert.False(t, ok, "a ref in a genuinely different bucket must stay rejected")
}
