package checkpoint

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/redact"
)

// claudeTranscriptWithImage returns a Claude Code JSONL transcript whose first
// line embeds an inline base64 image, followed by an ordinary assistant reply.
// It returns the raw (image-inline) bytes plus the base64 string so tests can
// assert on both the extracted and reinjected forms.
func claudeTranscriptWithImage(t *testing.T) (raw []byte, b64 string) {
	t.Helper()
	b64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nround-trip-fixture-bytes-long-enough-to-be-externalized\x00\x01\x02\x03"))
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[` +
			`{"type":"text","text":"look at this"},` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
			`]}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"nice screenshot"}],"usage":{"input_tokens":5,"output_tokens":7}}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n"), b64
}

// claudeImagePayload builds a one-image Claude Code transcript from a distinct
// payload, returning the raw inline bytes and the base64 string.
func claudeImagePayload(t *testing.T, payload string) (raw []byte, b64 string) {
	t.Helper()
	b64 = base64.StdEncoding.EncodeToString([]byte(payload + "-padded-so-the-base64-clears-the-externalize-threshold"))
	line := `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}`
	return []byte(line + "\n"), b64
}

// externalize runs the codec the way the condensation/finalize paths do.
func externalize(t *testing.T, raw []byte) (rewritten []byte, assets []TranscriptAsset) {
	t.Helper()
	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	rw, ex, err := codec.ExtractImages(raw)
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	out := make([]TranscriptAsset, len(ex))
	for i, a := range ex {
		out[i] = TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}
	return rw, out
}

// TestAssets_BackfillReExternalizesAndReplacesAssets is the S1 regression: the
// stop-hook finalize path (backfillTranscript / SessionTranscript) must persist a
// newly-externalized transcript and its assets, replacing the condense-time
// assets rather than orphaning them or re-inlining the images.
func TestAssets_BackfillReExternalizesAndReplacesAssets(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000010")
	sessionPath := cpID.Path() + "/0/"

	// Condense: first (mid-turn) externalized write.
	rawA, _ := claudeImagePayload(t, "condense-image")
	rewrittenA, assetsA := externalize(t, rawA)
	if len(assetsA) != 1 {
		t.Fatalf("want 1 asset from condense, got %d", len(assetsA))
	}
	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID, SessionID: "s-backfill", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted(rewrittenA), Assets: assetsA,
		Agent: agent.AgentTypeClaudeCode, AuthorName: "T", AuthorEmail: "t@t.com",
	}); err != nil {
		t.Fatalf("condense Write: %v", err)
	}

	// Finalize: backfill with a different, longer externalized transcript.
	rawB, b64B := claudeImagePayload(t, "finalize-different-image-with-more-bytes")
	rewrittenB, assetsB := externalize(t, rawB)
	if err := store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID, SessionID: "s-backfill",
		Transcript: redact.AlreadyRedacted(rewrittenB), Assets: assetsB,
		Agent: agent.AgentTypeClaudeCode,
	}); err != nil {
		t.Fatalf("backfill Write: %v", err)
	}

	// Stored full.jsonl carries B's placeholder, not raw base64; the old asset
	// blob is gone and B's is present.
	stored, ok := readBranchFile(t, store, sessionPath+paths.TranscriptFileName)
	if !ok {
		t.Fatal("full.jsonl missing")
	}
	if strings.Contains(stored, b64B) {
		t.Error("stored transcript still contains raw base64 after backfill")
	}
	if !strings.Contains(stored, "entire-asset:assets/"+assetsB[0].Name) {
		t.Error("stored transcript missing backfilled placeholder")
	}
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assetsA[0].Name); ok {
		t.Error("stale condense-time asset blob was not cleared on backfill")
	}
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assetsB[0].Name); !ok {
		t.Error("backfilled asset blob missing")
	}

	// Manifest pointer updated; restore round-trips to B byte-exact.
	summary := readSummaryFromBranch(t, repo, cpID)
	if summary.Sessions[0].AssetsManifest != "/"+sessionPath+paths.AssetsManifestFile {
		t.Errorf("assets_manifest pointer = %q, want set", summary.Sessions[0].AssetsManifest)
	}
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent: %v", err)
	}
	if string(content.Transcript) != string(rawB) {
		t.Fatalf("backfill round-trip not byte-exact:\n got: %s\nwant: %s", content.Transcript, rawB)
	}
}

// TestAssets_BackfillIdenticalTranscriptKeepsAssets is the short-circuit
// regression: a backfill whose transcript is byte-identical to what is stored
// (so replaceTranscript short-circuits) must NOT clear the assets, even if it is
// called with empty Assets — the still-present placeholder must keep round-tripping.
func TestAssets_BackfillIdenticalTranscriptKeepsAssets(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000012")
	sessionPath := cpID.Path() + "/0/"

	rawA, _ := claudeImagePayload(t, "shortcircuit-image")
	rewrittenA, assetsA := externalize(t, rawA)
	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID, SessionID: "s1", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted(rewrittenA), Assets: assetsA,
		Agent: agent.AgentTypeClaudeCode, AuthorName: "T", AuthorEmail: "t@t.com",
	}); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// Backfill with the identical transcript (short-circuit) and NO assets.
	if err := store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID, SessionID: "s1",
		Transcript: redact.AlreadyRedacted(rewrittenA),
		Agent:      agent.AgentTypeClaudeCode,
	}); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	// Assets survive; the placeholder still round-trips to the original image.
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assetsA[0].Name); !ok {
		t.Error("asset blob was cleared by an identical-transcript backfill")
	}
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent: %v", err)
	}
	if strings.Contains(string(content.Transcript), "entire-asset:assets/") {
		t.Errorf("dangling placeholder after identical-transcript backfill: %s", content.Transcript)
	}
	if string(content.Transcript) != string(rawA) {
		t.Errorf("restore did not round-trip after identical-transcript backfill")
	}
}

// TestAssets_BackfillInlineClearsStaleAssets covers the flag-off-at-finalize case:
// a backfill with an inline transcript and no assets must clear the assets stored
// at condense time (no orphans) and clear the manifest pointer.
func TestAssets_BackfillInlineClearsStaleAssets(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000011")
	sessionPath := cpID.Path() + "/0/"

	rawA, _ := claudeImagePayload(t, "condense-image")
	rewrittenA, assetsA := externalize(t, rawA)
	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID, SessionID: "s-inline", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted(rewrittenA), Assets: assetsA,
		Agent: agent.AgentTypeClaudeCode, AuthorName: "T", AuthorEmail: "t@t.com",
	}); err != nil {
		t.Fatalf("condense Write: %v", err)
	}

	// Backfill inline (as if externalization were off at finalize): no Assets.
	rawB, b64B := claudeImagePayload(t, "condense-image") // same content, inline
	if err := store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID, SessionID: "s-inline",
		Transcript: redact.AlreadyRedacted(rawB),
		Agent:      agent.AgentTypeClaudeCode,
	}); err != nil {
		t.Fatalf("backfill Write: %v", err)
	}

	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assetsA[0].Name); ok {
		t.Error("stale asset blob not cleared when backfill went inline")
	}
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsManifestFile); ok {
		t.Error("manifest not cleared when backfill went inline")
	}
	summary := readSummaryFromBranch(t, repo, cpID)
	if summary.Sessions[0].AssetsManifest != "" {
		t.Errorf("assets_manifest pointer = %q, want empty", summary.Sessions[0].AssetsManifest)
	}
	stored, _ := readBranchFile(t, store, sessionPath+paths.TranscriptFileName)
	if !strings.Contains(stored, b64B) {
		t.Error("inline backfill should store raw base64")
	}
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent: %v", err)
	}
	if string(content.Transcript) != string(rawB) {
		t.Errorf("inline backfill restore mismatch")
	}
}

// TestAssets_StoreRestoreRoundTrip is the end-to-end contract for image
// externalization at the persistent-store layer: a Claude Code transcript with an
// inline base64 image is externalized before the write, stored as a placeholder
// plus an assets/ blob and manifest, and reinjected byte-exactly on read.
func TestAssets_StoreRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000001")

	raw, b64 := claudeTranscriptWithImage(t)

	// Externalize exactly as the condensation path does, then store the
	// placeholder-bearing transcript with its assets.
	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	if codec == nil {
		t.Fatal("expected a Claude Code image codec")
	}
	rewritten, assets, err := codec.ExtractImages(raw)
	if err != nil {
		t.Fatalf("ExtractImages() error = %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 externalized asset, got %d", len(assets))
	}
	writeAssets := make([]TranscriptAsset, len(assets))
	for i, a := range assets {
		writeAssets[i] = TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}

	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-assets-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rewritten),
		Assets:       writeAssets,
		Prompts:      []string{"look at this"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"

	// Stored full.jsonl carries the placeholder, not the raw base64.
	stored, ok := readBranchFile(t, store, sessionPath+paths.TranscriptFileName)
	if !ok {
		t.Fatal("full.jsonl missing from checkpoint tree")
	}
	if strings.Contains(stored, b64) {
		t.Error("stored full.jsonl still contains raw base64 image data")
	}
	if !strings.Contains(stored, "entire-asset:assets/"+assets[0].Name) {
		t.Errorf("stored full.jsonl missing placeholder for %s", assets[0].Name)
	}

	// The asset blob and manifest are written under assets/.
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assets[0].Name); !ok {
		t.Errorf("asset blob %s missing from checkpoint tree", assets[0].Name)
	}
	manifest, ok := readBranchFile(t, store, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatal("assets/manifest.json missing from checkpoint tree")
	}
	if !strings.Contains(manifest, assets[0].Name) || !strings.Contains(manifest, `"media_type": "image/png"`) {
		t.Errorf("manifest missing expected asset entry: %s", manifest)
	}

	// Session metadata points at the manifest.
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	wantManifest := "/" + sessionPath + paths.AssetsManifestFile
	if summary.Sessions[0].AssetsManifest != wantManifest {
		t.Errorf("sessions[0].assets_manifest = %q, want %q", summary.Sessions[0].AssetsManifest, wantManifest)
	}

	// Read back: the image is reinjected byte-exactly, reproducing the original.
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if strings.Contains(string(content.Transcript), "entire-asset:assets/") {
		t.Error("restored transcript still contains a placeholder")
	}
	if !strings.Contains(string(content.Transcript), b64) {
		t.Error("restored transcript missing reinjected base64 image")
	}
	if string(content.Transcript) != string(raw) {
		t.Fatalf("round-trip not byte-exact:\n got: %s\nwant: %s", content.Transcript, raw)
	}
}

// TestAssets_NoExternalizationWritesNoManifest confirms the default (no assets)
// path is unchanged: no assets/ folder and an empty AssetsManifest pointer.
func TestAssets_NoExternalizationWritesNoManifest(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000002")

	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-assets-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Prompts:      []string{"hello one"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsManifestFile); ok {
		t.Error("assets/manifest.json should not be written when there are no assets")
	}
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	if summary.Sessions[0].AssetsManifest != "" {
		t.Errorf("sessions[0].assets_manifest = %q, want empty", summary.Sessions[0].AssetsManifest)
	}
}
