package fsstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// TestSeam_GitPrimaryWithFsMirror exercises the full pluggable seam: a git
// primary with the fsstore as a configured mirror, driven through
// checkpoint.Open. It writes all four WriteRequest variants and asserts each
// lands in BOTH backends, while reads resolve from the git primary.
//
// Not parallel: uses t.Chdir so settings + ref resolution target the test repo.
func TestSeam_GitPrimaryWithFsMirror(t *testing.T) {
	registerForTesting()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")

	mirrorDir := filepath.Join(t.TempDir(), "fs-mirror")
	writeMirrorSettings(t, dir, mirrorDir)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)

	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	const sessionID = "sess-1"

	// 1. Session: create the checkpoint.
	require.NoError(t, stores.Persistent.Write(ctx, cp.Session{
		CheckpointID: cid, SessionID: sessionID, Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted([]byte("initial transcript")),
		Prompts:    []string{"do the thing"}, FilesTouched: []string{"a.go"},
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	// 2. SessionTranscript: replace transcript at stop time.
	require.NoError(t, stores.Persistent.Write(ctx, cp.SessionTranscript{
		CheckpointID: cid, SessionID: sessionID,
		Transcript: redact.AlreadyRedacted([]byte("final transcript")),
		Prompts:    []string{"do the thing"},
	}))
	// 3. SessionSummary: set the latest session's summary.
	require.NoError(t, stores.Persistent.Write(ctx, cp.SessionSummary{
		CheckpointID: cid, Summary: &cp.Summary{Intent: "intent-x", Outcome: "outcome-y"},
	}))
	// 4. CheckpointAttribution: set combined attribution.
	require.NoError(t, stores.Persistent.Write(ctx, cp.CheckpointAttribution{
		CheckpointID: cid, Attribution: &cp.Attribution{AgentLines: 7, AgentPercentage: 70},
	}))

	// Reads resolve from the git primary.
	t.Run("git primary", func(t *testing.T) {
		assertAllVariants(t, stores.Persistent, cid)
	})

	// The fsstore mirror independently received every write.
	t.Run("fs mirror", func(t *testing.T) {
		mirror := New(mirrorDir)
		assertAllVariants(t, mirror, cid)
	})
}

// assertAllVariants verifies that all four writes are visible in a backend.
func assertAllVariants(t *testing.T, store cp.PersistentStore, cid id.CheckpointID) {
	t.Helper()
	ctx := context.Background()

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "checkpoint should exist")
	require.Len(t, summary.Sessions, 1)

	// SessionTranscript landed.
	content, err := store.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("final transcript"), content.Transcript)

	// SessionSummary landed.
	meta, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary)
	assert.Equal(t, "intent-x", meta.Summary.Intent)

	// CheckpointAttribution landed.
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 7, summary.CombinedAttribution.AgentLines)
}

func writeMirrorSettings(t *testing.T, repoDir, mirrorDir string) {
	t.Helper()
	// json-encode the path so separators / spaces are escaped correctly.
	encodedPath, err := json.Marshal(mirrorDir)
	require.NoError(t, err)
	body := `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}, "mirrors": [{"type": "fs", "config": {"path": ` +
		string(encodedPath) + `}}]}}`
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".entire", "settings.json"), []byte(body), 0o644))
}
