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

	// Root metadata.json still points at full.jsonl; the compact transcript is
	// written into the tree (and pushed) but not yet referenced by metadata.
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
}

func TestWriteCommitted_CompactTranscriptScopedToCheckpointStart(t *testing.T) {
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
	if strings.Contains(compactContent, "hello one") || strings.Contains(compactContent, "reply one") {
		t.Errorf("compact transcript contains content before checkpoint start:\n%s", compactContent)
	}
	if !strings.Contains(compactContent, "hello two") || !strings.Contains(compactContent, "reply two") {
		t.Errorf("compact transcript missing checkpoint-scoped content:\n%s", compactContent)
	}
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
	if strings.Contains(initialCompact, "beta") {
		t.Errorf("initial compact contains pre-start content:\n%s", initialCompact)
	}
	if !strings.Contains(initialCompact, "gamma") {
		t.Errorf("initial compact missing checkpoint-scoped content:\n%s", initialCompact)
	}

	// Finalize with the same raw transcript. replaceTranscript must sanitize
	// before compaction; otherwise the raw slice at line 2 would reintroduce
	// "beta".
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
	if strings.Contains(finalizeCompact, "beta") {
		t.Errorf("finalize compact contains pre-start content (raw bytes not sanitized):\n%s", finalizeCompact)
	}
	if finalizeCompact != initialCompact {
		t.Errorf("finalize compact diverges from initial write:\ninitial:  %s\nfinalize: %s", initialCompact, finalizeCompact)
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
