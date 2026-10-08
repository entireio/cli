package fsstore

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// The filesystem store keeps the line attribution an older CLI wrote through
// every rewrite, like the git stores: a summary write, a transcript backfill,
// attaching another session, and re-writing the same session.
func TestStore_RewritesKeepLegacyAttribution(t *testing.T) {
	t.Parallel()
	const (
		initial  = `{"agent_lines":12,"metric_version":2}`
		prompts  = `[{"checkpoint_number":1}]`
		combined = `{"agent_lines":20}`
	)
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	write := func(sessionID string) {
		t.Helper()
		require.NoError(t, store.Write(ctx, cp.Session{
			CheckpointID: cid, SessionID: sessionID, Strategy: "manual-commit",
			Transcript: redact.AlreadyRedacted([]byte("t")), FilesTouched: []string{"a.go"},
		}))
	}
	write("legacy-session")

	// Seed the stored document as an older CLI would have left it.
	path := store.path(cid)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var sc storedCheckpoint
	require.NoError(t, json.Unmarshal(data, &sc))
	sc.Summary.LegacyCombinedAttribution = json.RawMessage(combined)
	sc.Sessions[0].Metadata.LegacyInitialAttribution = json.RawMessage(initial)
	sc.Sessions[0].Metadata.LegacyPromptAttributions = json.RawMessage(prompts)
	require.NoError(t, store.save(&sc))

	require.NoError(t, store.Write(ctx, cp.SessionSummary{CheckpointID: cid, Summary: &cp.Summary{Intent: "i"}}))
	require.NoError(t, store.Write(ctx, cp.SessionTranscript{CheckpointID: cid, SessionID: "legacy-session", Transcript: redact.AlreadyRedacted([]byte("t2"))}))
	write("attached-session")
	write("legacy-session")

	got, err := store.load(cid)
	require.NoError(t, err)
	assert.JSONEq(t, combined, string(got.Summary.LegacyCombinedAttribution))
	idx := sessionIndexByID(got.Sessions, "legacy-session")
	require.GreaterOrEqual(t, idx, 0)
	assert.JSONEq(t, initial, string(got.Sessions[idx].Metadata.LegacyInitialAttribution))
	assert.JSONEq(t, prompts, string(got.Sessions[idx].Metadata.LegacyPromptAttributions))
	assert.Empty(t, got.Sessions[sessionIndexByID(got.Sessions, "attached-session")].Metadata.LegacyInitialAttribution,
		"a session this version writes carries none")
}
