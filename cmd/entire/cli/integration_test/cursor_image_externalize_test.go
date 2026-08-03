//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/require"
)

// TestCursorImageExternalization_SidecarCapture is the Cursor end-to-end proof.
// Cursor keeps pasted images in a per-session SQLite blob store (store.db), NOT
// the JSONL transcript Entire condenses, so the transcript codec used for Claude
// and Codex cannot reach them. This drives the real Cursor hook flow (session
// start -> before-submit-prompt -> mid-turn commit condensation -> stop finalize)
// with a store.db that holds an image, externalization enabled, and asserts the
// checkpoint captures the image as an asset (blob + manifest) even though the
// transcript never contained it and carries no placeholder.
func TestCursorImageExternalization_SidecarCapture(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed; skipping cursor store.db capture test")
	}

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	require.NoError(t, os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644))

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}
	chatsDir := t.TempDir()

	// Propagate the cursor project + chats dirs to BOTH the stop-hook subprocess
	// (via cliEnv) and the git-hook condensation subprocess (via gitHookEnv).
	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir,
		"ENTIRE_TEST_CURSOR_CHATS_DIR="+chatsDir,
	)

	const conversationID = "cursor-image-e2e"

	// Transcript is text-only — Cursor never inlines the image here.
	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"+
			`{"type":"assistant","text":"done"}`+"\n"), 0o600))

	// The image lives only in Cursor's SQLite store, keyed by conversation id at
	// <chats>/<workspace-hash>/<conversationID>/store.db.
	img := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("cursor-real-sidecar-image-payload-", 8))...)
	storeDBPath := filepath.Join(chatsDir, "workspace-hash", conversationID, "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeDBPath), 0o755))
	buildCursorStoreDB(t, storeDBPath, map[string][]byte{
		"img-blob":  img,
		"text-blob": []byte("this is a message body, not an image, and should be ignored"),
	})

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "look at this screenshot and add a feature",
	})

	env.WriteFile("feature.go", "package main\n// new feature\n")

	// Stop ends the turn; the commit's condensation then creates the checkpoint
	// and captures the sidecar image (Cursor has no mid-turn tool hooks, so the
	// checkpoint is born at commit time, not updated by a later finalize).
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
		"loop_count":      1,
	})
	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	cpID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cpID, "expected a condensed checkpoint after commit")
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"

	// The transcript is untouched: no placeholder, no image bytes (there were none).
	full, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.TranscriptFileName)
	require.True(t, ok, "full.jsonl missing at %s", sessionPath)
	require.NotContains(t, full, "entire-asset:", "cursor transcript must not carry a placeholder")

	// The manifest indexes the captured image.
	manifest, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	require.True(t, ok, "assets/manifest.json missing — sidecar image was not captured")
	var manifestDoc struct {
		Version int `json:"version"`
		Assets  []struct {
			Name      string `json:"name"`
			MediaType string `json:"media_type"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal([]byte(manifest), &manifestDoc))
	require.Len(t, manifestDoc.Assets, 1, "expected exactly one captured image in the manifest")
	entry := manifestDoc.Assets[0]
	require.Equal(t, "image/png", entry.MediaType)
	require.True(t, strings.HasPrefix(entry.Name, "img-") && strings.HasSuffix(entry.Name, ".png"),
		"asset name %q is not img-<hash>.png", entry.Name)

	// The asset blob is stored byte-exact.
	blob, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsDir+entry.Name)
	require.True(t, ok, "asset blob %s missing", entry.Name)
	require.Equal(t, string(img), blob, "stored asset bytes differ from the store.db image")
}

// TestCursorImageExternalization_SurvivesFinalizeRewrite guards against the
// finalize-wipe regression: when a mid-turn commit's condensation captures a
// Cursor sidecar image and a later stop finalizes the checkpoint with a grown
// (rewritten) transcript, writeAssets clears the whole assets/ folder before
// re-writing. If finalize omitted the sidecar images from its asset set, the
// captured image would be permanently dropped. This drives that exact sequence
// and asserts the image survives finalize.
func TestCursorImageExternalization_SurvivesFinalizeRewrite(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed; skipping cursor store.db capture test")
	}

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	require.NoError(t, os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644))

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}
	chatsDir := t.TempDir()
	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir,
		"ENTIRE_TEST_CURSOR_CHATS_DIR="+chatsDir,
	)

	const conversationID = "cursor-finalize-wipe"
	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	// v1: what condensation stores at the mid-turn commit.
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"), 0o600))

	img := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("cursor-finalize-image-payload-", 8))...)
	storeDBPath := filepath.Join(chatsDir, "workspace-hash", conversationID, "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeDBPath), 0o755))
	buildCursorStoreDB(t, storeDBPath, map[string][]byte{"img-blob": img})

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath, "model": "cursor-default",
	})
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath,
		"prompt": "look at this screenshot and add a feature",
	})

	// Mid-turn commit while the session is ACTIVE: condensation creates the
	// checkpoint + captures the sidecar image, and PostCommit records it in
	// TurnCheckpointIDs so the later stop finalize runs over it. AsAgent takes the
	// no-TTY active-session fast path (a human mid-turn commit path differs).
	env.WriteFile("feature.go", "package main\n// new feature\n")
	env.GitCommitWithShadowHooksAsAgent("Add feature", "feature.go")

	cpID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cpID, "expected a condensed checkpoint after the mid-turn commit")
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"
	_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	require.True(t, ok, "PRECONDITION: condensation should have captured the sidecar image")

	// Grow the transcript so the finalized full transcript differs from what
	// condensation stored -> replaceTranscript reports rewrote==true, the exact
	// condition under which finalize rewrites (and previously wiped) the assets.
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"+
			`{"type":"assistant","text":"added the feature"}`+"\n"), 0o600))

	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath,
		"model": "cursor-default", "loop_count": 1,
	})

	// Regression assertion: the image asset must STILL be present after finalize.
	manifest, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	require.True(t, ok, "assets/manifest.json missing after finalize — sidecar image was wiped")
	var manifestDoc struct {
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal([]byte(manifest), &manifestDoc))
	require.Len(t, manifestDoc.Assets, 1, "expected the captured image to survive finalize")
	blob, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsDir+manifestDoc.Assets[0].Name)
	require.True(t, ok, "asset blob missing after finalize")
	require.Equal(t, string(img), blob, "asset bytes changed after finalize")
}

// TestCursorImageExternalization_PreservesImagesOnFinalizeCaptureMiss guards the
// best-effort edge: condensation captures a Cursor image, but the sidecar
// re-capture at finalize yields nothing (e.g. sqlite3 locked/timed out, or — as
// simulated here — the store.db is momentarily gone). A rewriting finalize must
// then PRESERVE the images condensation stored rather than clearing the assets/
// folder for the now-empty asset set.
func TestCursorImageExternalization_PreservesImagesOnFinalizeCaptureMiss(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed; skipping cursor store.db capture test")
	}

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	require.NoError(t, os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644))

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}
	chatsDir := t.TempDir()
	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir,
		"ENTIRE_TEST_CURSOR_CHATS_DIR="+chatsDir,
	)

	const conversationID = "cursor-finalize-miss"
	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"), 0o600))

	img := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("cursor-preserve-image-payload-", 8))...)
	storeDBPath := filepath.Join(chatsDir, "workspace-hash", conversationID, "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeDBPath), 0o755))
	buildCursorStoreDB(t, storeDBPath, map[string][]byte{"img-blob": img})

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath, "model": "cursor-default",
	})
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath,
		"prompt": "look at this screenshot and add a feature",
	})

	// Mid-turn commit: condensation captures the image into the checkpoint.
	env.WriteFile("feature.go", "package main\n// new feature\n")
	env.GitCommitWithShadowHooksAsAgent("Add feature", "feature.go")

	cpID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cpID, "expected a condensed checkpoint after the mid-turn commit")
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"
	_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	require.True(t, ok, "PRECONDITION: condensation should have captured the sidecar image")

	// Grow the transcript so finalize rewrites (rewrote=true), AND remove the
	// store.db so the finalize re-capture yields nothing — the transient-miss case.
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"+
			`{"type":"assistant","text":"added the feature"}`+"\n"), 0o600))
	require.NoError(t, os.Remove(storeDBPath))

	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id": conversationID, "transcript_path": transcriptPath,
		"model": "cursor-default", "loop_count": 1,
	})

	// The image captured at condensation must survive the finalize rewrite even
	// though the re-capture found nothing.
	manifest, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	require.True(t, ok, "assets/manifest.json missing after finalize — sidecar image was wiped on a capture miss")
	var manifestDoc struct {
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal([]byte(manifest), &manifestDoc))
	require.Len(t, manifestDoc.Assets, 1, "expected the captured image to survive a finalize capture miss")
	blob, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsDir+manifestDoc.Assets[0].Name)
	require.True(t, ok, "asset blob missing after finalize")
	require.Equal(t, string(img), blob, "asset bytes changed after finalize")
}

// buildCursorStoreDB writes a Cursor-style store.db with a blobs(id, data) table
// populated from the given blobs, by shelling out to sqlite3.
func buildCursorStoreDB(t *testing.T, path string, blobs map[string][]byte) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("CREATE TABLE blobs(id TEXT PRIMARY KEY, data BLOB);\n")
	for id, data := range blobs {
		sb.WriteString("INSERT INTO blobs(id,data) VALUES('" + id + "', x'" + hex.EncodeToString(data) + "');\n")
	}
	cmd := exec.CommandContext(context.Background(), "sqlite3", path, sb.String())
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build store.db: %s", out)
}
