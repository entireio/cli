package checkpoint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// A subagent transcript and the image assets externalized out of it are one
// unit: the transcript holds only placeholders, so the bytes exist nowhere else
// in the checkpoint. These tests cover the two properties that follow — the
// pair is written together, and a rewrite *replaces* the asset set instead of
// piling a new one on top of the last.
//
// They drive the real store writers with externalization enabled, which is
// process-global state (env var + cwd), so none of them can run in parallel.

// taskImageTranscript writes a one-image Claude Code subagent transcript to a
// temp file and returns its path plus the raw image bytes. The PNG signature is
// required: only bytes that identify themselves as an image are externalized.
func taskImageTranscript(t *testing.T, dir, payload string) (path string, img []byte) {
	t.Helper()
	img = append([]byte("\x89PNG\r\n\x1a\n"), payload+"-padded-so-the-base64-clears-the-externalize-threshold"...)
	b64 := base64.StdEncoding.EncodeToString(img)
	line := `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n"
	path = filepath.Join(dir, "agent-"+payload+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))
	return path, img
}

// taskPlainTranscript writes a subagent transcript with no images at all — the
// shape a rewrite takes when the agent's later transcript carries no image, or
// when nothing in it is externalizable.
func taskPlainTranscript(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, "agent-"+name+".jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"no images here"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))
	return path
}

func manifestAssetNames(t *testing.T, manifestJSON string) []string {
	t.Helper()
	var doc struct {
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal([]byte(manifestJSON), &doc))
	names := make([]string, 0, len(doc.Assets))
	for _, a := range doc.Assets {
		names = append(names, a.Name)
	}
	return names
}

func taskPayloadFromTranscriptFile(ctx context.Context, t *testing.T, toolUseID, agentID, transcriptPath string) TaskPayload {
	t.Helper()
	content, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)
	prepared, assets, tooLarge := prepareSubagentTranscript(ctx, agent.AgentTypeClaudeCode, transcriptPath, content)
	require.False(t, tooLarge, "test fixture transcript must be under the size cap")
	redacted, jsonlErr := redact.JSONLBytes(prepared)
	if jsonlErr != nil {
		redacted = redact.AlreadyRedacted(redact.Bytes(prepared))
	}
	return TaskPayload{
		ToolUseID:   toolUseID,
		AgentID:     agentID,
		Transcript:  redacted,
		Assets:      assets,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
	}
}

func writeSessionWithTask(t *testing.T, store sessionMetadataStore, cpID id.CheckpointID, task TaskPayload) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "s-bundle",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"msg":"safe"}` + "\n")),
		AuthorName:   "T",
		AuthorEmail:  "t@t.com",
		Agent:        agent.AgentTypeClaudeCode,
		Tasks:        []TaskPayload{task},
	}))
}

// The transcript and its assets land together, under the task's own directory,
// with the image bytes stored byte-exact and gone from the transcript.
func TestTaskBundle_PersistentWritesTranscriptAndAssetsTogether(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("bbbbcccc0001")
	taskDir := cpID.Path() + "/tasks/toolu_bundle1/"

	transcriptPath, img := taskImageTranscript(t, t.TempDir(), "bundled")
	task := taskPayloadFromTranscriptFile(context.Background(), t, "toolu_bundle1", "agent1", transcriptPath)
	writeSessionWithTask(t, store, cpID, task)

	stored, ok := readBranchFile(t, store, taskDir+paths.AgentTranscriptFileName("agent1"))
	require.True(t, ok, "task transcript missing")
	require.Contains(t, stored, "entire-asset:assets/", "transcript should carry a placeholder")
	require.NotContains(t, stored, base64.StdEncoding.EncodeToString(img),
		"image bytes must not remain inline once externalized")

	manifest, ok := readBranchFile(t, store, taskDir+paths.AssetsManifestFile)
	require.True(t, ok, "assets manifest missing beside the transcript")
	names := manifestAssetNames(t, manifest)
	require.Len(t, names, 1)
	blob, ok := readBranchFile(t, store, taskDir+paths.AssetsDir+names[0])
	require.True(t, ok, "asset blob missing")
	require.Equal(t, string(img), blob, "stored asset bytes differ from the source image")
}

// A rewrite of the same task whose transcript no longer externalizes anything
// must clear the previous asset set. Asset names are random, so a new write can
// never overwrite an old one: without an explicit clear the old blobs and
// manifest survive, referenced by nothing.
func TestTaskBundle_PersistentRewriteWithoutImagesClearsAssets(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("bbbbcccc0002")
	taskDir := cpID.Path() + "/tasks/toolu_bundle2/"
	tmp := t.TempDir()

	withImage, _ := taskImageTranscript(t, tmp, "stale")
	write := func(transcriptPath string) {
		task := taskPayloadFromTranscriptFile(context.Background(), t, "toolu_bundle2", "agent1", transcriptPath)
		writeSessionWithTask(t, store, cpID, task)
	}

	write(withImage)
	manifest, ok := readBranchFile(t, store, taskDir+paths.AssetsManifestFile)
	require.True(t, ok, "first write should store a manifest")
	staleName := manifestAssetNames(t, manifest)[0]

	write(taskPlainTranscript(t, tmp, "rewrite"))

	if _, present := readBranchFile(t, store, taskDir+paths.AssetsDir+staleName); present {
		t.Error("stale asset blob survived a rewrite that externalized nothing")
	}
	if _, present := readBranchFile(t, store, taskDir+paths.AssetsManifestFile); present {
		t.Error("stale assets manifest survived a rewrite that externalized nothing")
	}
	stored, ok := readBranchFile(t, store, taskDir+paths.AgentTranscriptFileName("agent1"))
	require.True(t, ok, "rewritten transcript missing")
	require.NotContains(t, stored, "entire-asset:assets/",
		"the rewritten transcript should reference no assets")
}

// The shadow-branch writer must replace the asset subtree too. It only ever
// appended before, so a second image-bearing write to the same task accumulated
// both sets — the first one unreferenced by the surviving transcript.
func TestTaskBundle_EphemeralRewriteReplacesAssets(t *testing.T) {
	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "README.md"), []byte("# Test"), 0o600))
	_, err = worktree.Add("README.md")
	require.NoError(t, err)
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	t.Chdir(tempDir)
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	store := newEphemeralStore(repo, DefaultV1Refs())
	baseCommit := initialCommit.String()
	taskDir := paths.EntireMetadataDir + "/test-session/tasks/toolu_eph1/"

	write := func(transcriptPath string) {
		_, writeErr := store.Write(context.Background(), TaskStep{
			SessionID: "test-session", BaseCommit: baseCommit,
			ToolUseID: "toolu_eph1", AgentID: "agent1",
			Agent:                  agent.AgentTypeClaudeCode,
			SubagentTranscriptPath: transcriptPath,
			CheckpointUUID:         "uuid", CommitMessage: "Task checkpoint",
			AuthorName: "Test", AuthorEmail: "test@test.com",
		})
		require.NoError(t, writeErr)
	}
	shadowTree := func() *object.Tree {
		ref, refErr := repo.Reference(plumbing.NewBranchReferenceName(ShadowBranchNameForCommit(baseCommit, "")), true)
		require.NoError(t, refErr)
		commit, commitErr := repo.CommitObject(ref.Hash())
		require.NoError(t, commitErr)
		tree, treeErr := commit.Tree()
		require.NoError(t, treeErr)
		return tree
	}
	readShadowFile := func(path string) (string, bool) {
		file, fileErr := shadowTree().File(path)
		if fileErr != nil {
			return "", false
		}
		content, contentErr := file.Contents()
		require.NoError(t, contentErr)
		return content, true
	}

	first, _ := taskImageTranscript(t, tempDir, "eph-first")
	write(first)
	manifest, ok := readShadowFile(taskDir + paths.AssetsManifestFile)
	require.True(t, ok, "first write should store a manifest")
	staleName := manifestAssetNames(t, manifest)[0]

	second, secondImg := taskImageTranscript(t, tempDir, "eph-second")
	write(second)

	manifest, ok = readShadowFile(taskDir + paths.AssetsManifestFile)
	require.True(t, ok, "rewrite should store a manifest")
	names := manifestAssetNames(t, manifest)
	require.Len(t, names, 1, "the manifest must describe only the current asset set")
	require.NotEqual(t, staleName, names[0], "fixture bug: the two writes produced the same asset")

	if _, present := readShadowFile(taskDir + paths.AssetsDir + staleName); present {
		t.Error("the previous write's asset blob accumulated instead of being replaced")
	}
	blob, ok := readShadowFile(taskDir + paths.AssetsDir + names[0])
	require.True(t, ok, "current asset blob missing")
	require.Equal(t, string(secondImg), blob)

	// Dropping the subtree must not take the transcript with it.
	stored, ok := readShadowFile(taskDir + paths.AgentTranscriptFileName("agent1"))
	require.True(t, ok, "task transcript missing after rewrite")
	require.Contains(t, stored, "entire-asset:assets/"+names[0])
}

// An unreadable transcript leaves the previously stored pair untouched. Half of
// a consistent pair is worse than a stale one: clearing the assets would orphan
// the placeholders in the transcript that is still there.
func TestTaskBundle_PersistentFailedWriteLeavesStoredPairIntact(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("bbbbcccc0003")
	taskDir := cpID.Path() + "/tasks/toolu_bundle3/"

	transcriptPath, img := taskImageTranscript(t, t.TempDir(), "intact")
	write := func(path string) {
		if path == transcriptPath {
			task := taskPayloadFromTranscriptFile(context.Background(), t, "toolu_bundle3", "agent1", path)
			writeSessionWithTask(t, store, cpID, task)
			return
		}
		// Unreadable path: store task metadata only (no new transcript/assets).
		require.NoError(t, store.Write(context.Background(), Session{
			CheckpointID: cpID, SessionID: "s-bundle", Strategy: "manual-commit",
			Transcript: redact.AlreadyRedacted([]byte(`{"msg":"safe"}` + "\n")),
			AuthorName: "T", AuthorEmail: "t@t.com",
			Agent: agent.AgentTypeClaudeCode,
			Tasks: []TaskPayload{{
				ToolUseID:                   "toolu_bundle3",
				AgentID:                     "agent1",
				TranscriptUnavailableReason: "transcript unreadable",
				StartedAt:                   time.Now(),
				CompletedAt:                 time.Now(),
			}},
		}))
	}

	write(transcriptPath)
	manifest, ok := readBranchFile(t, store, taskDir+paths.AssetsManifestFile)
	require.True(t, ok)
	name := manifestAssetNames(t, manifest)[0]

	// Rewrite the same task with a transcript that cannot be read.
	write(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))

	blob, ok := readBranchFile(t, store, taskDir+paths.AssetsDir+name)
	require.True(t, ok, "assets must survive a write that stored no new transcript")
	require.Equal(t, string(img), blob)
	stored, ok := readBranchFile(t, store, taskDir+paths.AgentTranscriptFileName("agent1"))
	require.True(t, ok, "the previously stored transcript must survive")
	require.Contains(t, stored, "entire-asset:assets/"+name,
		"transcript and assets must still agree with each other")
}

// The stored bundle is self-consistent: rooted at the task directory, the
// placeholder-bearing transcript reinjects to the original bytes. This pins the
// contract for whoever reads task transcripts back — note that no CLI reader
// roots at the task directory today (ReadSessionContent reinjects for numbered
// session dirs only), so a task transcript read through the CLI is currently
// placeholder-bearing by design, not by accident.
func TestTaskBundle_StoredBundleReinjectsByteExact(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("bbbbcccc0004")
	taskDir := cpID.Path() + "/tasks/toolu_bundle4/"

	transcriptPath, img := taskImageTranscript(t, t.TempDir(), "roundtrip")
	original, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)

	task := taskPayloadFromTranscriptFile(context.Background(), t, "toolu_bundle4", "agent1", transcriptPath)
	writeSessionWithTask(t, store, cpID, task)

	stored, ok := readBranchFile(t, store, taskDir+paths.AgentTranscriptFileName("agent1"))
	require.True(t, ok)
	manifest, ok := readBranchFile(t, store, taskDir+paths.AssetsManifestFile)
	require.True(t, ok)
	name := manifestAssetNames(t, manifest)[0]
	blob, ok := readBranchFile(t, store, taskDir+paths.AssetsDir+name)
	require.True(t, ok)

	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	restored, err := codec.ReinjectImages([]byte(stored), func(lookup string) (imageextract.Asset, bool) {
		if lookup != name {
			return imageextract.Asset{}, false
		}
		return imageextract.Asset{Name: name, Data: []byte(blob)}, true
	})
	require.NoError(t, err)
	require.Equal(t, string(img), blob)
	require.Equal(t, strings.TrimSpace(string(original)), strings.TrimSpace(string(restored)),
		"the stored transcript plus its assets must reproduce the source transcript")
}
