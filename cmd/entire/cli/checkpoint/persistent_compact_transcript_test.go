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

	err := store.Write(context.Background(), Session{
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

	// Root metadata.json: transcript points at full.jsonl, compact_transcript
	// at transcript.jsonl.
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	wantTranscript := "/" + sessionPath + paths.TranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
	wantHash := "/" + sessionPath + paths.ContentHashFileName
	if summary.Sessions[0].ContentHash != wantHash {
		t.Errorf("sessions[0].content_hash = %q, want %q", summary.Sessions[0].ContentHash, wantHash)
	}
	wantCompact := "/" + sessionPath + paths.CompactTranscriptFileName
	if summary.Sessions[0].CompactTranscript != wantCompact {
		t.Errorf("sessions[0].compact_transcript = %q, want %q", summary.Sessions[0].CompactTranscript, wantCompact)
	}
}

// TestWriteCommitted_CompactTranscriptFullWithMarker verifies the full-compact
// contract: transcript.jsonl stores the entire compacted session (so each
// checkpoint is self-contained), and the session metadata's
// compact_transcript_start marks where this checkpoint's slice begins. Readers
// recover this checkpoint's content as fullCompactLines[marker:].
func TestWriteCommitted_CompactTranscriptFullWithMarker(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("b2c3d4e5f6a1")

	err := store.Write(context.Background(), Session{
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

	compactContent, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	// The file now contains the WHOLE session, including pre-start content.
	for _, want := range []string{"hello one", "reply one", "hello two", "reply two"} {
		if !strings.Contains(compactContent, want) {
			t.Errorf("full compact transcript missing %q:\n%s", want, compactContent)
		}
	}

	// The marker scopes this checkpoint: raw line 2 (the second user turn) maps
	// to compact line 2, and fullCompactLines[2:] is exactly this checkpoint's slice.
	meta := readSessionMetadata(t, repo, cpID)
	marker, ok := meta.GetCompactTranscriptStart()
	if !ok {
		t.Fatal("compact_transcript_start not recorded in session metadata")
	}
	if marker != 2 {
		t.Fatalf("compact_transcript_start = %d, want 2", marker)
	}

	assertCompactSliceScoped(t, compactContent, marker,
		[]string{"hello one", "reply one"}, []string{"hello two", "reply two"})
}

func TestWriteCommitted_NonCompactableTranscriptPointsAtFull(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("c3d4e5f6a1b2")

	err := store.Write(context.Background(), Session{
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
	if summary.Sessions[0].CompactTranscript != "" {
		t.Errorf("sessions[0].compact_transcript = %q for non-compactable transcript, want empty", summary.Sessions[0].CompactTranscript)
	}
}

// TestUpdateCommitted_RefreshesCompactTranscriptPointer guards against the
// finalize path writing transcript.jsonl without updating the root
// metadata.json. When the initial write produced no compact transcript but a
// later backfill does, sessions[].compact_transcript must be refreshed to point
// at it rather than staying omitted.
func TestUpdateCommitted_RefreshesCompactTranscriptPointer(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("f6a1b2c3d4e5")

	// Initial write with a non-compactable transcript: full.jsonl is written but
	// no transcript.jsonl, so compact_transcript is omitted.
	err := store.Write(context.Background(), Session{
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
	summary := readSummaryFromBranch(t, repo, cpID)
	if summary.Sessions[0].CompactTranscript != "" {
		t.Fatalf("precondition: compact_transcript = %q, want empty", summary.Sessions[0].CompactTranscript)
	}

	// Finalize with a compactable transcript: transcript.jsonl is now written and
	// the root summary's compact_transcript must be refreshed to match.
	err = store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:        agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); !ok {
		t.Fatal("transcript.jsonl missing after finalize")
	}
	summary = readSummaryFromBranch(t, repo, cpID)
	wantCompact := "/" + sessionPath + paths.CompactTranscriptFileName
	if summary.Sessions[0].CompactTranscript != wantCompact {
		t.Errorf("sessions[0].compact_transcript = %q, want %q", summary.Sessions[0].CompactTranscript, wantCompact)
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

	// Initial write sanitizes before compaction. With start=2 the dropped
	// compaction line shifts the window so only "gamma" survives.
	err := store.Write(context.Background(), Session{
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
	// transcript.jsonl is now the full sanitized session: the compaction line is
	// dropped, so it holds [alpha, beta, gamma]. Scoping is via the marker.
	if !strings.Contains(initialCompact, "gamma") {
		t.Errorf("initial compact missing content:\n%s", initialCompact)
	}
	// The marker scopes out everything before the checkpoint start. Sanitize
	// runs before compaction, so the dropped compaction line shifts the window:
	// fullCompactLines[marker:] must be exactly [gamma], excluding "beta".
	initialMeta := readSessionMetadata(t, repo, cpID)
	initialMarker, ok := initialMeta.GetCompactTranscriptStart()
	if !ok {
		t.Fatal("compact_transcript_start not recorded after WriteCommitted")
	}
	assertCompactSliceScoped(t, initialCompact, initialMarker, []string{"beta"}, []string{"gamma"})

	// Finalize with the same raw transcript. replaceTranscript must sanitize
	// before compaction, exactly like the initial write — otherwise the full
	// content or the marker would diverge.
	err = store.Write(context.Background(), SessionTranscript{
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
	if finalizeCompact != initialCompact {
		t.Errorf("finalize compact diverges from initial write:\ninitial:  %s\nfinalize: %s", initialCompact, finalizeCompact)
	}
	finalizeMeta := readSessionMetadata(t, repo, cpID)
	finalizeMarker, ok := finalizeMeta.GetCompactTranscriptStart()
	if !ok {
		t.Fatal("compact_transcript_start not recorded after UpdateCommitted")
	}
	if finalizeMarker != initialMarker {
		t.Errorf("finalize marker %d diverges from initial marker %d", finalizeMarker, initialMarker)
	}
	assertCompactSliceScoped(t, finalizeCompact, finalizeMarker, []string{"beta"}, []string{"gamma"})
}

// assertCompactSliceScoped checks that slicing the full compact transcript at
// the marker yields exactly this checkpoint's content: every wantAbsent string
// (pre-start content) is gone and every wantPresent string is retained.
func assertCompactSliceScoped(t *testing.T, compactContent string, marker int, wantAbsent, wantPresent []string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(compactContent, "\n"), "\n")
	if marker > len(lines) {
		t.Fatalf("marker %d out of range for %d compact lines", marker, len(lines))
	}
	slice := strings.Join(lines[marker:], "\n")
	for _, s := range wantAbsent {
		if strings.Contains(slice, s) {
			t.Errorf("slice past marker contains pre-start content %q:\n%s", s, slice)
		}
	}
	for _, s := range wantPresent {
		if !strings.Contains(slice, s) {
			t.Errorf("slice past marker missing checkpoint content %q:\n%s", s, slice)
		}
	}
}

// TestUpdateCommitted_DropsStaleCompactWhenRegenerationProducesNone guards the
// OPF/finalize rewrite: if the re-redacted transcript no longer yields a compact
// transcript, the stale transcript.jsonl from the initial write must be removed
// (not shipped as a less-redacted artifact) and its marker cleared, rather than
// left pointing at content that no longer matches the re-redacted full.jsonl.
func TestUpdateCommitted_DropsStaleCompactWhenRegenerationProducesNone(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a7b8c9d0e1f2")

	// Initial write: compactable transcript → transcript.jsonl + marker present.
	if err := store.Write(context.Background(), Session{
		CheckpointID:              cpID,
		SessionID:                 "session-001",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:                     agent.AgentTypeClaudeCode,
		CheckpointTranscriptStart: 2,
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}
	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); !ok {
		t.Fatal("precondition: transcript.jsonl missing after initial write")
	}
	if _, ok := readSessionMetadata(t, repo, cpID).GetCompactTranscriptStart(); !ok {
		t.Fatal("precondition: compact_transcript_start not recorded after initial write")
	}

	// Finalize with a non-compactable transcript: regeneration yields nothing.
	if err := store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted([]byte("not json at all\nstill not json\n")),
		Agent:        agent.AgentTypeClaudeCode,
	}); err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Stale compact transcript dropped from the tree.
	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); ok {
		t.Error("stale transcript.jsonl left in tree after regeneration produced none")
	}
	// Root summary pointer cleared.
	if got := readSummaryFromBranch(t, repo, cpID).Sessions[0].CompactTranscript; got != "" {
		t.Errorf("sessions[0].compact_transcript = %q, want empty", got)
	}
	// Session metadata marker cleared.
	if offset, ok := readSessionMetadata(t, repo, cpID).GetCompactTranscriptStart(); ok {
		t.Errorf("compact_transcript_start still set (%d) after stale compact dropped", offset)
	}
}

func TestUpdateCommitted_RegeneratesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	initial := claudeStyleTranscript()
	err := store.Write(context.Background(), Session{
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
	err = store.Write(context.Background(), SessionTranscript{
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
