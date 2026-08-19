//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// codexCiphertext stands in for a Codex encrypted reasoning payload: long enough
// to be unmistakable in a stored blob, and shaped like the real base64.
var codexCiphertext = strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 40)

// codexRolloutWithEncryptedReasoning builds a Codex rollout whose reasoning and
// compaction items carry encrypted_content — the non-portable state Entire strips
// from its stored copy (see codex.SanitizePortableTranscript). Both real shapes use
// the `encrypted_content` key; that is the only key the sanitizer strips.
func codexRolloutWithEncryptedReasoning(sessionID, repoDir, ciphertext string) string {
	return strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + repoDir + `"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"add feature.txt"}]}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}}`,
		`{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"compaction","encrypted_content":"` + ciphertext + `"}}`,
		`{"timestamp":"2026-01-01T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added feature.txt"}]}}`,
	}, "\n") + "\n"
}

// countJSONLLines counts non-empty lines, matching how transcript offsets are
// counted (countTranscriptItems for JSONL agents).
func countJSONLLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// findShadowSessionTranscript locates the session transcript blob inside a shadow
// branch tree. It searches rather than reconstructing the path because the metadata
// directory is named after the date-prefixed Entire session ID, not the agent's
// raw session_id.
func findShadowSessionTranscript(t *testing.T, repoDir, branchName string) (string, bool) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return "", false
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}

	var content string
	var found bool
	err = tree.Files().ForEach(func(f *object.File) error {
		if found {
			return nil
		}
		if !strings.HasPrefix(f.Name, paths.EntireMetadataDir+"/") {
			return nil
		}
		if !strings.HasSuffix(f.Name, "/"+paths.TranscriptFileName) {
			return nil
		}
		c, cErr := f.Contents()
		if cErr != nil {
			return cErr
		}
		content = c
		found = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk shadow tree: %v", err)
	}
	return content, found
}

// codexHooker returns a helper that drives the real Codex hook binary.
func codexHooker(t *testing.T, repoDir, sessionID, transcriptPath string) func(string, map[string]any) {
	t.Helper()
	runner := NewCodexHookRunner(repoDir, t)
	return func(name string, extra map[string]any) {
		t.Helper()
		in := map[string]any{
			"session_id":      sessionID,
			"transcript_path": transcriptPath,
			"cwd":             repoDir,
			"model":           "gpt-5",
			"permission_mode": "default",
		}
		for k, v := range extra {
			in[k] = v
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal %s input: %v", name, err)
		}
		if err := runner.runCodexHook(name, b); err != nil {
			t.Fatalf("codex hook %s: %v", name, err)
		}
	}
}

func applyPatchHook(hook func(string, map[string]any), toolUseID, patch string) {
	hook("post-tool-use", map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"tool_use_id":   toolUseID,
		"tool_input":    map[string]string{"command": patch},
		"tool_response": "Success.",
	})
}

// TestCodexShadowBranch_SanitizesTranscript proves that the shadow-branch copy of a
// Codex rollout has the non-portable payloads stripped.
//
// Before the fix, lifecycle wrote the raw rollout to .entire/metadata/<session>/full.jsonl
// and the generic metadata-dir walker (addDirectoryToChanges -> createRedactedBlobFromFile)
// redacted every blob without ever sanitizing — so encrypted_content ciphertext landed in
// the shadow tree, and the 8 redaction layers had to scan all of it first (base64 is the
// pathological input for the entropy layer).
func TestCodexShadowBranch_SanitizesTranscript(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	// Long enough to be unmistakable in the blob, and shaped like the real thing.
	ciphertext := codexCiphertext

	sessionID := "codex-shadow-sanitize"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rollout := codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext)
	if err := os.WriteFile(transcriptPath, []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)

	// Turn start, then a file-mutating tool use so SaveStep writes a shadow checkpoint.
	// The file must exist on disk (uncommitted) when Stop fires, so the ephemeral
	// write has worktree changes to snapshot.
	hook("user-prompt-submit", map[string]any{
		"prompt": "add feature.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("feature.txt", "hi\n")
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after Codex stop", shadowBranch)
	}

	stored, ok := findShadowSessionTranscript(t, env.RepoDir, shadowBranch)
	if !ok {
		t.Fatalf("shadow branch %s has no session transcript", shadowBranch)
	}

	if strings.Contains(stored, ciphertext) {
		t.Error("shadow-branch transcript still contains encrypted_content ciphertext (not sanitized)")
	}
	if strings.Contains(stored, `"encrypted_content"`) {
		t.Error("shadow-branch transcript still has an encrypted_content key")
	}

	// The compaction item is stripped in place, not dropped: its line survives so
	// the stored transcript stays line-aligned with the agent's rollout. Offsets
	// like CheckpointTranscriptStart are counted on the rollout and applied here,
	// so a differing line count silently mis-scopes every later read.
	if !strings.Contains(stored, `"type":"compaction"`) {
		t.Error("compaction item was dropped; stored transcript is no longer line-aligned with the rollout")
	}
	if got, want := countJSONLLines(stored), countJSONLLines(rollout); got != want {
		t.Errorf("stored transcript has %d lines, rollout has %d — offsets into the stored copy will drift", got, want)
	}

	// Sanitization must not eat the actual conversation.
	if !strings.Contains(stored, "add feature.txt") {
		t.Error("shadow-branch transcript lost the user prompt")
	}
	if !strings.Contains(stored, "added feature.txt") {
		t.Error("shadow-branch transcript lost the assistant reply")
	}
}

// TestCodexShadowBranch_GrowthStillDetectedAfterCommit is the regression guard for the
// coordinate coupling that sanitization introduces.
//
// sessionHasNewContent compares the shadow transcript blob's size against
// state.CheckpointTranscriptSize, the baseline recorded at the previous condensation.
// Sanitizing the shadow blob shrinks it by ~99% for Codex, so if the baseline keeps
// being measured on the raw transcript, `transcriptBlobSize > CheckpointTranscriptSize`
// is false forever and the session never condenses again after its first commit.
func TestCodexShadowBranch_GrowthStillDetectedAfterCommit(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	ciphertext := codexCiphertext
	sessionID := "codex-shadow-growth"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeRollout := func(content string) {
		t.Helper()
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
	}

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)

	// Turn 1: work, then commit — this condenses and records the growth baseline.
	writeRollout(codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext))
	hook("user-prompt-submit", map[string]any{
		"prompt": "add feature.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("feature.txt", "hi\n")
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	env.GitCommitWithShadowHooks("add feature.txt", "feature.txt")

	firstCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if firstCheckpoint == "" {
		t.Fatal("first commit produced no checkpoint")
	}

	// Turn 2: the rollout grows with a genuinely new exchange, then commit again.
	grown := codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext) +
		`{"timestamp":"2026-01-01T00:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"now add second.txt"}]}}` + "\n" +
		`{"timestamp":"2026-01-01T00:01:01Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}}` + "\n" +
		`{"timestamp":"2026-01-01T00:01:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added second.txt"}]}}` + "\n"
	writeRollout(grown)

	hook("user-prompt-submit", map[string]any{
		"prompt": "now add second.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("second.txt", "yo\n")
	applyPatchHook(hook, "call_2", "*** Begin Patch\n*** Add File: second.txt\n+yo\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	env.GitCommitWithShadowHooks("add second.txt", "second.txt")

	secondCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if secondCheckpoint == "" {
		t.Fatal("second commit produced no checkpoint")
	}
	if secondCheckpoint == firstCheckpoint {
		t.Fatalf("second commit did not condense a new checkpoint (growth went undetected); "+
			"both commits report checkpoint %s", firstCheckpoint)
	}
}

// TestCodexCondense_NoAssetsFromSanitizedAwayContent proves that image
// externalization runs on the sanitized transcript, not the raw one.
//
// Condensation's pipeline order is sanitize -> externalize -> redact. If
// externalization runs before sanitization, images embedded in items that
// sanitization discards (Codex compaction payloads) get extracted into asset blobs
// and a manifest entry, while the transcript line that referenced them is dropped
// moments later — leaving an orphaned asset stored and pushed forever.
func TestCodexCondense_NoAssetsFromSanitizedAwayContent(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	if err := os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	// Two distinct images, both padded past the externalization length threshold.
	// keptImg lives in a normal user message. droppedImg lives in a compaction item
	// nested inside a "compacted" line's replacement_history, which
	// sanitizeHistoryItems removes outright — the one place sanitization still
	// discards content.
	//
	// The shape is synthetic: real Codex compaction items carry only
	// encrypted_content, no readable content. It is here to pin the ordering
	// invariant (never externalize out of content we are about to discard), not to
	// reproduce an observed Codex rollout.
	keptImg := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("kept-image-payload-", 8))
	droppedImg := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("dropped-image-payload-", 8))
	keptB64 := base64.StdEncoding.EncodeToString(keptImg)
	droppedB64 := base64.StdEncoding.EncodeToString(droppedImg)

	keptSum := sha256.Sum256(keptImg)
	keptHex := hex.EncodeToString(keptSum[:])
	droppedSum := sha256.Sum256(droppedImg)
	droppedHex := hex.EncodeToString(droppedSum[:])

	sessionID := "codex-sanitize-before-extract"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")
	rollout := strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + env.RepoDir + `"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[` +
			`{"type":"input_text","text":"add feature.txt and look at this screenshot"},` +
			`{"type":"input_image","image_url":"data:image/png;base64,` + keptB64 + `"}` +
			`]}}`,
		// The nested compaction item is discarded by sanitization, so nothing in it
		// should ever be extracted into an asset.
		`{"timestamp":"2026-01-01T00:00:02Z","type":"compacted","payload":{"message":"","replacement_history":[` +
			`{"type":"compaction","content":[{"type":"input_image","image_url":"data:image/png;base64,` + droppedB64 + `"}]}` +
			`]}}`,
		`{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added feature.txt"}]}}`,
	}, "\n") + "\n"

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)
	hook("user-prompt-submit", map[string]any{
		"prompt": "add feature.txt and look at this screenshot", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("feature.txt", "hi\n")
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	env.GitCommitWithShadowHooks("add feature.txt", "feature.txt")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"

	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatalf("assets/manifest.json missing at %s", sessionPath)
	}

	var manifest struct {
		Assets []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, raw)
	}

	var sums []string
	for _, a := range manifest.Assets {
		sums = append(sums, a.SHA256)
	}

	if !slices.Contains(sums, keptHex) {
		t.Errorf("the kept image was not externalized; manifest sha256s = %v", sums)
	}
	if slices.Contains(sums, droppedHex) {
		t.Error("an image inside a sanitized-away compaction item was externalized into an orphaned asset " +
			"(externalization ran before sanitization)")
	}
	if len(manifest.Assets) != 1 {
		t.Errorf("expected exactly 1 externalized asset, got %d: %s", len(manifest.Assets), raw)
	}
}
