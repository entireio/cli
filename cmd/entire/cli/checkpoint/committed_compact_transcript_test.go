package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// claudeStyleTranscript returns a Claude Code-format JSONL transcript with two
// user/assistant exchanges (4 lines total).
func claudeStyleTranscript() []byte {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello one"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"reply one"}],"usage":{"input_tokens":5,"output_tokens":7}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"hello two"}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-01-01T00:00:03Z","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"reply two"}],"usage":{"input_tokens":6,"output_tokens":8}}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// readBranchFile reads a file from the committed checkpoints branch tree.
// Returns ("", false) when the file does not exist.
func readBranchFile(t *testing.T, store *GitStore, path string) (string, bool) {
	t.Helper()
	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}
	file, err := tree.File(path)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	if err != nil {
		t.Fatalf("Contents(%s) error = %v", path, err)
	}
	return content, true
}

func TestWriteCommitted_WritesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Prompts:      []string{"hello one"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"

	// full.jsonl is still written for CLI read paths.
	if _, ok := readBranchFile(t, store, sessionPath+paths.TranscriptFileName); !ok {
		t.Error("full.jsonl missing from checkpoint tree")
	}

	// transcript.jsonl is written with compact content derived from the
	// transcript. The compact format itself is covered by transcript/compact;
	// here we only assert the store persisted non-empty derived content.
	compactContent, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	if !strings.Contains(compactContent, "reply two") {
		t.Error("compact transcript missing assistant content")
	}

	// Root metadata.json points at the compact transcript.jsonl when one was
	// written; full.jsonl remains in the tree for CLI read paths.
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	wantTranscript := "/" + sessionPath + paths.CompactTranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
	wantHash := "/" + sessionPath + paths.ContentHashFileName
	if summary.Sessions[0].ContentHash != wantHash {
		t.Errorf("sessions[0].content_hash = %q, want %q", summary.Sessions[0].ContentHash, wantHash)
	}
}

// compactLinesFrom returns the compact transcript content from line `start`
// (0-indexed) onward — the numeric slice a consumer applies using the
// compact_transcript_start offset recorded in metadata.
func compactLinesFrom(content string, start int) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if start >= len(lines) {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

func TestWriteCommitted_CompactTranscriptStoredFullWithStartOffset(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("b2c3d4e5f6a1")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-001",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:                     agent.AgentTypeClaudeCode,
		CheckpointTranscriptStart: 2,
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// The compact transcript is stored in FULL (never trimmed) — it contains the
	// pre-start content as well as this checkpoint's own content, mirroring
	// full.jsonl.
	compactContent, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	for _, want := range []string{"hello one", "reply one", "hello two", "reply two"} {
		if !strings.Contains(compactContent, want) {
			t.Errorf("full compact transcript missing %q:\n%s", want, compactContent)
		}
	}

	// The checkpoint's start is recorded as a compact-line offset in metadata,
	// not by trimming the file. claudeStyleTranscript compacts to 4 lines (one
	// per message), so raw start line 2 maps to compact line 2.
	meta := readSessionMetadataAtIndex(t, repo, cpID, 0)
	if meta.CompactTranscriptStart != 2 {
		t.Errorf("compact_transcript_start = %d, want 2", meta.CompactTranscriptStart)
	}

	// Slicing the stored compact at the recorded offset yields exactly the
	// checkpoint's portion.
	scoped := compactLinesFrom(compactContent, meta.CompactTranscriptStart)
	if strings.Contains(scoped, "hello one") || strings.Contains(scoped, "reply one") {
		t.Errorf("compact slice at offset contains pre-start content:\n%s", scoped)
	}
	if !strings.Contains(scoped, "hello two") || !strings.Contains(scoped, "reply two") {
		t.Errorf("compact slice at offset missing checkpoint content:\n%s", scoped)
	}
}

func TestWriteCommitted_NonCompactableTranscriptPointsAtFull(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("c3d4e5f6a1b2")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("not json at all\nstill not json\n")),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); ok {
		t.Error("transcript.jsonl written for non-compactable transcript")
	}

	summary := readSummaryFromBranch(t, repo, cpID)
	wantTranscript := "/" + sessionPath + paths.TranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
}

// codexTranscriptWithCompactionBeforeStart returns a Codex-format JSONL
// transcript whose line 1 is a `compaction` entry that
// codex.SanitizePortableTranscript drops. With a checkpoint start of line 2,
// slicing the raw (unsanitized) transcript yields [beta, gamma] while slicing
// the sanitized transcript (compaction removed) yields only [gamma] — so the
// compact transcript diverges unless the finalize path sanitizes like the
// initial-write path does.
func codexTranscriptWithCompactionBeforeStart() []byte {
	lines := []string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"alpha"}]}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"compaction","encrypted_content":"REDACTED"}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"beta"}]}}`,
		`{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"gamma"}]}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestUpdateCommitted_CodexCompactSanitizedLikeInitialWrite guards against the
// finalize path compacting raw Codex bytes while the initial-write path
// compacts sanitized bytes. Both must produce the same checkpoint-scoped
// compact transcript.
func TestUpdateCommitted_CodexCompactSanitizedLikeInitialWrite(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("e5f6a1b2c3d4")

	raw := codexTranscriptWithCompactionBeforeStart()
	compactPath := cpID.Path() + "/0/" + paths.CompactTranscriptFileName

	// Initial write sanitizes before compaction (drops the `compaction` line).
	// The compact transcript is stored in full; the checkpoint's start is
	// recorded as an offset.
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-001",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(raw),
		Agent:                     agent.AgentTypeCodex,
		CheckpointTranscriptStart: 2,
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}
	initialCompact, ok := readBranchFile(t, store, compactPath)
	if !ok {
		t.Fatal("transcript.jsonl missing after WriteCommitted")
	}
	initialMeta := readSessionMetadataAtIndex(t, repo, cpID, 0)

	// Finalize with the same raw transcript. replaceTranscript must sanitize
	// before compaction; otherwise the stored full compact and the recorded
	// offset would diverge from the initial write.
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(raw),
		Agent:        agent.AgentTypeCodex,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}
	finalizeCompact, ok := readBranchFile(t, store, compactPath)
	if !ok {
		t.Fatal("transcript.jsonl missing after UpdateCommitted")
	}
	finalizeMeta := readSessionMetadataAtIndex(t, repo, cpID, 0)

	// The stored full compact transcript and the recorded start offset are
	// identical across both paths (sanitization parity).
	if finalizeCompact != initialCompact {
		t.Errorf("finalize compact diverges from initial write:\ninitial:  %s\nfinalize: %s", initialCompact, finalizeCompact)
	}
	if finalizeMeta.CompactTranscriptStart != initialMeta.CompactTranscriptStart {
		t.Errorf("finalize compact_transcript_start = %d, want %d (initial)",
			finalizeMeta.CompactTranscriptStart, initialMeta.CompactTranscriptStart)
	}

	// Slicing the stored compact at the recorded offset excludes the pre-start
	// "beta" and includes "gamma". Had the finalize path skipped Codex
	// sanitization, the offset would shift and "beta" would leak into scope.
	scoped := compactLinesFrom(finalizeCompact, finalizeMeta.CompactTranscriptStart)
	if strings.Contains(scoped, "beta") {
		t.Errorf("compact slice at offset contains pre-start content:\n%s", scoped)
	}
	if !strings.Contains(scoped, "gamma") {
		t.Errorf("compact slice at offset missing checkpoint content:\n%s", scoped)
	}
}

func TestUpdateCommitted_RegeneratesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	initial := claudeStyleTranscript()
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(initial),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	extended := append([]byte{}, initial...)
	extended = append(extended,
		[]byte(`{"type":"user","uuid":"u3","timestamp":"2026-01-01T00:00:04Z","message":{"role":"user","content":"hello three"}}`+"\n")...)
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(extended),
		Agent:        agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	compactContent, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing after UpdateCommitted")
	}
	if !strings.Contains(compactContent, "hello three") {
		t.Errorf("compact transcript not regenerated with new content:\n%s", compactContent)
	}
}

// TestUpdateCommitted_PointsAtCompactAfterDeferredFinalize covers the deferred
// finalization flow: the checkpoint is created without a transcript (so the
// pointer is empty), then UpdateCommitted backfills the full transcript and
// must move the root metadata.json pointer onto the compact transcript.jsonl.
func TestUpdateCommitted_PointsAtCompactAfterDeferredFinalize(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("f6a1b2c3d4e5")

	// Initial write with no transcript (deferred): files-only checkpoint.
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		FilesTouched: []string{"main.go"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	summary := readSummaryFromBranch(t, repo, cpID)
	if summary.Sessions[0].Transcript != "" {
		t.Errorf("pre-finalize sessions[0].transcript = %q, want empty", summary.Sessions[0].Transcript)
	}

	// Finalize with the full transcript.
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:        agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); !ok {
		t.Fatal("transcript.jsonl missing after deferred finalize")
	}
	summary = readSummaryFromBranch(t, repo, cpID)
	wantTranscript := "/" + sessionPath + paths.CompactTranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("post-finalize sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
}
