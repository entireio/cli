//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// onePixelPNGPrefix is the start of a base64 1x1 PNG: the signature plus an
// IHDR chunk declaring width 1 and height 1.
const onePixelPNGPrefix = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"

const (
	dropImagesSettings              = `{"redaction":{"drop_images":true}}`
	externalizeImagesSettings       = `{"redaction":{"externalize_images":true}}`
	dropAndExternalizeImageSettings = `{"redaction":{"drop_images":true,"externalize_images":true}}`
)

func writeLocalSettings(t *testing.T, env *TestEnv, content string) {
	t.Helper()
	path := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// testImage returns the base64 of a PNG-signed payload unique to tag. The
// payload is random-looking, as compressed image data is, so redaction treats
// it the way it treats a real image that was not dropped.
func testImage(tag string) string {
	raw := []byte("\x89PNG\r\n\x1a\n")
	block := sha256.Sum256([]byte(tag))
	for range 8 {
		raw = append(raw, block[:]...)
		block = sha256.Sum256(block[:])
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// imageFragmentLen is the longest run of an image's base64 a stored file may
// share with it. Redaction can cut an image it does not recognize into
// pieces, so the whole value being absent does not show the image is gone.
const imageFragmentLen = 24

func claudeTranscriptWithImage(b64 string) string {
	return strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"add feature and look at this"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"u2","type":"user","message":{"role":"user","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Success"}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
	}, "\n") + "\n"
}

// requireNoImageStored reads every file at the tip of the checkpoint branch
// and fails if any holds the image, or a piece of it, or sits in an assets
// folder.
func requireNoImageStored(t *testing.T, env *TestEnv, b64 string) {
	t.Helper()
	require.True(t, env.BranchExists(paths.MetadataBranchName), "no checkpoint branch: nothing was stored")
	files := strings.Fields(gitOutput(t, env.RepoDir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName))
	require.NotEmpty(t, files, "the checkpoint branch is empty")
	for _, file := range files {
		require.NotContains(t, file, "/"+paths.AssetsDir, "an asset was stored while drop_images is on")
		content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, file)
		require.True(t, ok, "cannot read %s", file)
		for i := 0; i+imageFragmentLen <= len(b64); i += imageFragmentLen {
			require.NotContains(t, content, b64[i:i+imageFragmentLen], "%s holds part of the image", file)
		}
		require.NotContains(t, content, "entire-asset:", "%s holds an asset placeholder", file)
	}
}

func requireStoredTranscriptHasOnePixelImage(t *testing.T, env *TestEnv, path string) {
	t.Helper()
	content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, path)
	require.True(t, ok, "%s was not stored", path)
	require.Contains(t, content, onePixelPNGPrefix, "%s should carry the 1x1 placeholder image", path)
}

// startClaudeSessionWithImage begins a Claude Code turn whose transcript holds
// an inline image, and makes a mid-turn commit so condensation stores it.
func startClaudeSessionWithImage(t *testing.T, env *TestEnv, b64 string) *Session {
	t.Helper()
	session := env.NewSession()
	require.NoError(t, os.WriteFile(session.TranscriptPath, []byte(claudeTranscriptWithImage(b64)), 0o644))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "add feature and look at this", session.TranscriptPath))
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithHooks("add feature", "feature.go")
	return session
}

// Commit condensation, then the turn-end rewrite of the same checkpoint.
func TestDropImages_ClaudeCommitAndTurnEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("claude-drop")

	session := startClaudeSessionWithImage(t, env, b64)

	cpID := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpID)
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(cpID, paths.TranscriptFileName))

	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(cpID, paths.TranscriptFileName))

	// What a resume would restore holds the placeholder, not the image.
	repo, err := gitrepo.OpenPath(env.RepoDir)
	require.NoError(t, err)
	defer repo.Close()
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	checkpointID, err := id.NewCheckpointID(cpID)
	require.NoError(t, err)
	content, err := stores.Persistent.ReadSessionContent(context.Background(), checkpointID, 0)
	require.NoError(t, err)
	require.NotContains(t, string(content.Transcript), b64)
	require.Contains(t, string(content.Transcript), onePixelPNGPrefix)

	// The agent's own transcript is never modified.
	live, err := os.ReadFile(session.TranscriptPath)
	require.NoError(t, err)
	require.Contains(t, string(live), b64, "the agent's transcript must keep its image")
}

func TestDropImages_CodexCommitAndTurnEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("codex-drop")

	sessionID := "codex-drop-images"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")
	rollout := strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","thread_source":"user","cwd":"` + env.RepoDir + `"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[` +
			`{"type":"input_text","text":"add feature.txt and look at this screenshot"},` +
			`{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"}` +
			`]}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c0","output":"saved data:image/png;base64,` + b64 + ` to disk"}}`,
		`{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(rollout), 0o644))

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)
	hook("user-prompt-submit", map[string]any{"prompt": "add feature.txt and look at this screenshot", "hook_event_name": "UserPromptSubmit"})
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	env.WriteFile("feature.txt", "hi\n")
	env.GitCommitWithHooks("add feature.txt", "feature.txt")

	cpID := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpID)
	requireNoImageStored(t, env, b64)

	hook("stop", map[string]any{"hook_event_name": "Stop"})

	requireNoImageStored(t, env, b64)
	full, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(cpID, paths.TranscriptFileName))
	require.True(t, ok)
	require.Contains(t, full, "data:image/png;base64,"+onePixelPNGPrefix, "the data URI should hold the 1x1 placeholder")
}

// OpenCode stores one JSON document, not JSONL: pretty-printed by OpenCode 1,
// where an attached image is a data URI, and converted from OpenCode 2, where
// it is bare base64 in a file part.
func TestDropImages_OpenCodeCommitAndTurnEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)
	writeLocalSettings(t, env, dropImagesSettings)
	dataURIImage := testImage("opencode-1-drop")
	filePartImage := testImage("opencode-2-drop")

	session := env.NewOpenCodeSession()
	require.NoError(t, env.SimulateOpenCodeSessionStart(session.ID, session.TranscriptPath))
	require.NoError(t, env.SimulateOpenCodeTurnStart(session.ID, session.TranscriptPath, "add a script and look at this"))
	env.WriteFile("script.sh", "#!/bin/bash\necho hello")
	session.CreateOpenCodeTranscript("add a script and look at this", []FileChange{{Path: "script.sh", Content: "#!/bin/bash\necho hello"}})

	raw, err := os.ReadFile(session.TranscriptPath)
	require.NoError(t, err)
	var export struct {
		Info     map[string]any   `json:"info"`
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &export))
	parts, ok := export.Messages[0]["parts"].([]any)
	require.True(t, ok, "the first message has no parts")
	export.Messages[0]["parts"] = append(parts,
		map[string]any{"type": "file", "mime": "image/png", "filename": "one.png", "url": "data:image/png;base64," + dataURIImage},
		map[string]any{"type": "file", "mime": "image/png", "name": "two.png", "data": filePartImage},
	)
	pretty, err := json.MarshalIndent(export, "", "  ")
	require.NoError(t, err)
	require.Greater(t, strings.Count(string(pretty), "\n"), 10, "the export must span many lines")
	require.NoError(t, os.WriteFile(session.TranscriptPath, pretty, 0o644))
	env.CopyTranscriptToEntireTmp(session.ID, session.TranscriptPath)

	env.GitCommitWithHooksAsAgent("add script", "script.sh")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID)
	requireOpenCodeImagesDropped := func() {
		t.Helper()
		requireNoImageStored(t, env, dataURIImage)
		requireNoImageStored(t, env, filePartImage)
		full, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(cpID, paths.TranscriptFileName))
		require.True(t, ok)
		require.Equal(t, 2, strings.Count(full, onePixelPNGPrefix), "both images should be 1x1 placeholders")
		require.Contains(t, full, "data:image/png;base64,"+onePixelPNGPrefix)
	}
	requireOpenCodeImagesDropped()

	require.NoError(t, env.SimulateOpenCodeTurnEnd(session.ID, session.TranscriptPath))

	requireOpenCodeImagesDropped()
}

func TestDropImages_WinsOverExternalize(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropAndExternalizeImageSettings)
	b64 := testImage("both-on")

	session := startClaudeSessionWithImage(t, env, b64)
	requireNoImageStored(t, env, b64)

	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(env.GetLatestCheckpointIDFromHistory(), paths.TranscriptFileName))
}

// Assets a commit stored before the setting was turned on are cleared when
// the turn-end rewrite runs with it on.
func TestDropImages_TurnedOnMidTurnClearsStoredAssets(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, externalizeImagesSettings)
	b64 := testImage("mid-turn")

	session := startClaudeSessionWithImage(t, env, b64)
	cpID := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpID)
	_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(cpID, paths.AssetsManifestFile))
	require.True(t, ok, "precondition: the commit should have stored the image as an asset")

	writeLocalSettings(t, env, dropImagesSettings)
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(cpID, paths.TranscriptFileName))
}

// Cursor keeps images in its own store, never in the transcript. With drop on
// they must not be captured, even when externalization is also on.
func TestDropImages_CursorSidecarNotCaptured(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed; skipping cursor store.db test")
	}

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)
	writeLocalSettings(t, env, dropAndExternalizeImageSettings)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}
	chatsDir := t.TempDir()
	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir,
		"ENTIRE_TEST_CURSOR_CHATS_DIR="+chatsDir,
	)

	const conversationID = "cursor-drop-images"
	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"look at this screenshot and add a feature"}`+"\n"+
			`{"type":"assistant","text":"done"}`+"\n"), 0o600))

	img := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("cursor-drop-sidecar-image-payload-", 8))...)
	storeDBPath := filepath.Join(chatsDir, "workspace-hash", conversationID, "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeDBPath), 0o755))
	buildCursorStoreDB(t, storeDBPath, map[string][]byte{"img-blob": img})

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
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
		"loop_count":      1,
	})
	env.GitCommitWithHooks("Add feature", "feature.go")

	require.NotEmpty(t, env.TryGetLatestCheckpointID(), "expected a condensed checkpoint after commit")
	requireNoImageStored(t, env, string(img))
}

func appendImageToTranscript(t *testing.T, path, b64 string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(`{"type":"user","uuid":"img-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_shot","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}}` + "\n")
	require.NoError(t, err)
}

// A subagent's transcript stored with the commit that carries its work.
func TestDropImages_SubagentTranscriptAtCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("subagent-commit")

	const (
		taskToolUseID = "toolu_01DropAtCommit"
		subagentID    = "a00aa11bb22cc33dd"
		editedFile    = "docs/red.md"
	)
	session := env.NewSession()
	session.CreateTranscript("delegate "+editedFile+" to a subagent", nil)
	subagentTranscript := session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "content"}})
	appendImageToTranscript(t, subagentTranscript, b64)

	require.NoError(t, env.SimulateUserPromptSubmit(session.ID))
	require.NoError(t, env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID))
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	require.NoError(t, env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}))
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	env.GitCommitWithHooks("Add red.md", editedFile)

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID, "the commit should carry an Entire-Checkpoint trailer")
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, CheckpointTaskFilePath(cpID, taskToolUseID, "agent-"+subagentID+".jsonl"))
}

// A subagent that commits its own work mid-turn has its transcript added to
// that commit's checkpoint afterwards, on a separate write path.
func TestDropImages_SubagentTranscriptAttachedAfterItsOwnCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("subagent-attach")

	const (
		taskToolUseID = "toolu_01DropOnAttach"
		subagentID    = "a99ff88ee77dd66cc"
		editedFile    = "docs/red.md"
	)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/red.md and commit it", nil)
	subagentTranscript := session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Red is warm.\n"}})
	appendImageToTranscript(t, subagentTranscript, b64)

	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	require.NoError(t, env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID))
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	env.GitCommitWithHooksAsAgent("Add red.md", editedFile)
	require.NoError(t, env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}))

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID, "the subagent's commit should carry an Entire-Checkpoint trailer")
	taskTranscript := CheckpointTaskFilePath(cpID, taskToolUseID, "agent-"+subagentID+".jsonl")
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, taskTranscript)

	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, taskTranscript)
}

func TestDropImages_SessionAttach(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("attach")

	sessionID := "attach-drop-images-001"
	transcriptPath := filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(claudeTranscriptWithImage(b64)), 0o644))

	output := env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f")
	require.Contains(t, output, "Created checkpoint")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID)
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(cpID, paths.TranscriptFileName))
}

func TestDropImages_Import(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	writeLocalSettings(t, env, dropImagesSettings)
	b64 := testImage("import")

	sessionID := "import-drop-images"
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","cwd":` + claudeFixtureCwdJSON(t, env.RepoDir) + `,"timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl"), []byte(content), 0o644))

	out := env.RunCLI("import", agentClaudeCode)
	require.Contains(t, out, "Imported 1", "got: %s", out)

	importedID := agentimport.DeriveCheckpointID(sessionID, "u1").String()
	requireNoImageStored(t, env, b64)
	requireStoredTranscriptHasOnePixelImage(t, env, SessionFilePath(importedID, paths.TranscriptFileName))
}
