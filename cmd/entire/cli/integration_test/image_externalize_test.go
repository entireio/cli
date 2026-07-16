//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestImageExternalization_FullHookFlow is the real end-to-end proof: it drives
// the actual entire hook binary (mid-turn commit -> condensation, then Stop ->
// finalize) on a Claude Code session whose transcript embeds an inline base64
// image, with externalization enabled via settings.local.json (also exercising
// the local-settings-merge fix). It then inspects the real entire/checkpoints/v1
// ref and confirms:
//   - full.jsonl carries the placeholder, not the raw base64 (survives finalize)
//   - the asset blob + manifest.json were written and decode to the exact image
//   - ReadSessionContent (the restore path) reinjects the image byte-exactly
func TestImageExternalization_FullHookFlow(t *testing.T) {
	// Uses settings/env that must be stable across the hook subprocesses; no t.Parallel.
	env := NewFeatureBranchEnv(t)

	// Enable externalization via the gitignored local settings file (the natural
	// rollout opt-in, and the path the merge fix restored).
	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	if err := os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	// A real, minimal PNG (valid magic bytes), padded so its base64 clears the
	// externalization length threshold.
	imgBytes := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("entire-real-e2e-image-payload-", 4))
	b64 := base64.StdEncoding.EncodeToString(imgBytes)

	session := env.NewSession()

	// Author a Claude Code transcript: prompt, a user turn with an inline image,
	// a file-writing tool use (so the commit has attributable content), result.
	transcript := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"add feature and look at this"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"u2","type":"user","message":{"role":"user","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Success"}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(session.TranscriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "add feature and look at this", session.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}

	// Mid-turn commit -> post-commit condensation externalizes.
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithShadowHooks("add feature", "feature.go")

	// Stop -> finalize rewrites each turn checkpoint with the full transcript. This
	// is where the (fixed) re-inlining bug lived: assert externalization survives it.
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 should exist after condensation")
	}
	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id found in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"

	// full.jsonl: placeholder present, raw base64 gone (externalized, and it stuck
	// through finalize).
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

	// manifest.json written; the asset blob decodes to the exact original image.
	manifest, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatal("assets/manifest.json missing")
	}
	if !strings.Contains(manifest, `"media_type": "image/png"`) {
		t.Errorf("manifest missing png entry: %s", manifest)
	}

	// Restore path: ReadSessionContent reinjects the image byte-exactly.
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
		t.Fatalf("parse checkpoint id %q: %v", cpID, err)
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

// TestImageExternalization_FinalizeWithFlagOffPreservesAssets guards the
// config-drift case: externalization is ON at condensation (placeholders +
// assets stored) but OFF at finalize (env override not inherited by the hook
// process, or settings toggled mid-session). Extraction then doesn't run at
// finalize and finalizeAssets is empty — that must mean "didn't run", not
// "no images": the previously-stored asset blobs must survive the rewrite
// (the re-inlined base64 in the finalized transcript is destroyed by
// redaction, so clearing the assets would lose the images permanently).
func TestImageExternalization_FinalizeWithFlagOffPreservesAssets(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	localSettings := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	if err := os.WriteFile(localSettings, []byte(`{"redaction":{"externalize_images":true}}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	imgBytes := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("entire-flag-drift-image-payload-", 4))
	b64 := base64.StdEncoding.EncodeToString(imgBytes)

	session := env.NewSession()
	transcript := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"add feature and look at this"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"u2","type":"user","message":{"role":"user","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Success"}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(session.TranscriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "add feature and look at this", session.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}

	// Mid-turn commit with the flag ON: condensation stores placeholder + asset.
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithShadowHooks("add feature", "feature.go")

	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id found in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"
	manifest, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatal("PRECONDITION: condensation should have stored assets/manifest.json")
	}

	// Toggle the flag OFF before the stop finalize.
	if err := os.Remove(localSettings); err != nil {
		t.Fatalf("remove settings.local.json: %v", err)
	}
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The stored assets must survive the finalize rewrite.
	manifestAfter, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatal("assets/manifest.json was cleared by a finalize that ran without externalization")
	}
	if manifestAfter != manifest {
		t.Errorf("manifest changed across a flag-off finalize:\nbefore: %s\nafter: %s", manifest, manifestAfter)
	}
}
