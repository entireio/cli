//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestCodexImageExternalization_FullHookFlow is the Codex end-to-end proof: it
// drives the real Codex hook binary (user-prompt-submit -> apply_patch
// post-tool-use -> mid-turn commit condensation -> stop finalize) on a Codex
// rollout transcript that embeds an image as a data-URI, with externalization
// enabled via settings.local.json. It then asserts the actual
// entire/checkpoints/v1 ref stores a placeholder (not the raw base64) that
// survives Codex's SanitizePortableTranscript, writes the asset blob + manifest,
// and that ReadSessionContent reinjects the image byte-exactly.
func TestCodexImageExternalization_FullHookFlow(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	if err := os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	// A real, minimal PNG padded past the externalization length threshold.
	imgBytes := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("codex-real-e2e-image-payload-", 4))
	b64 := base64.StdEncoding.EncodeToString(imgBytes)

	sessionID := "codex-image-e2e"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")

	// A Codex rollout: session meta, then a user message with an inline image
	// data-URI (the confirmed real format), then an assistant reply.
	rollout := strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + env.RepoDir + `"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[` +
			`{"type":"input_text","text":"add feature.txt and look at this screenshot"},` +
			`{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"}` +
			`]}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	}, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	runner := NewCodexHookRunner(env.RepoDir, t)
	hook := func(name string, extra map[string]any) {
		t.Helper()
		in := map[string]any{
			"session_id":      sessionID,
			"transcript_path": transcriptPath,
			"cwd":             env.RepoDir,
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

	// Turn start (creates the Codex session), then a file-mutating tool use so the
	// commit has attributable content.
	hook("user-prompt-submit", map[string]any{"prompt": "add feature.txt and look at this screenshot", "hook_event_name": "UserPromptSubmit"})
	patch := "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n"
	hook("post-tool-use", map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"tool_use_id": "call_1", "tool_input": map[string]string{"command": patch}, "tool_response": "Success.",
	})

	// Mid-turn commit -> post-commit condensation externalizes; stop -> finalize.
	env.WriteFile("feature.txt", "hi\n")
	env.GitCommitWithShadowHooks("add feature.txt", "feature.txt")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 should exist after Codex condensation")
	}
	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"

	full, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.TranscriptFileName)
	if !ok {
		t.Fatalf("full.jsonl missing at %s", sessionPath)
	}
	if strings.Contains(full, b64) {
		t.Error("stored full.jsonl still contains the raw base64 image (externalization did not persist)")
	}
	if !strings.Contains(full, "entire-asset:assets/") {
		t.Error("stored full.jsonl has no image placeholder")
	}
	if !strings.Contains(full, "data:image/png;base64,entire-asset:assets/") {
		t.Error("expected the placeholder inside the data-URI (prefix preserved)")
	}
	if _, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile); !ok {
		t.Error("assets/manifest.json missing")
	}

	// Restore reinjects the image byte-exactly.
	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatalf("open stores: %v", err)
	}
	checkpointID, err := id.NewCheckpointID(cpID)
	if err != nil {
		t.Fatalf("parse checkpoint id: %v", err)
	}
	content, err := stores.Persistent.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent: %v", err)
	}
	if strings.Contains(string(content.Transcript), "entire-asset:assets/") {
		t.Error("restored transcript still has a placeholder (reinjection failed)")
	}
	if !strings.Contains(string(content.Transcript), b64) {
		t.Error("restored transcript is missing the reinjected base64 image")
	}
}
