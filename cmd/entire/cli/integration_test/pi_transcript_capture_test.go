//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

func writePiJSONL(t *testing.T, transcriptPath string, entries []map[string]interface{}) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}

	var out bytes.Buffer
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("failed to marshal transcript entry: %v", err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}

	if err := os.WriteFile(transcriptPath, out.Bytes(), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
}

func TestPiNewSession_CheckpointStoresFullTranscript(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	piSession := env.NewPiSession()

	if err := env.SimulatePiSessionStart(piSession.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("new-session.txt", "new session content\n")

	entries := []map[string]interface{}{
		{
			"type": "message",
			"id":   "1",
			"message": map[string]interface{}{
				"role":    "user",
				"content": "new session prompt",
			},
		},
		{
			"type": "message",
			"id":   "2",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "working on it"},
					{
						"type": "toolCall",
						"id":   "tool-1",
						"name": "write",
						"arguments": map[string]interface{}{
							"path": "new-session.txt",
						},
					},
				},
			},
		},
		{
			"type": "message",
			"id":   "3",
			"message": map[string]interface{}{
				"role":       "toolResult",
				"toolName":   "write",
				"toolCallId": "tool-1",
				"details": map[string]interface{}{
					"path": "new-session.txt",
				},
			},
		},
	}
	writePiJSONL(t, piSession.TranscriptPath, entries)

	if err := env.SimulatePiStop(piSession.ID, piSession.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Commit new-session transcript turn", "new-session.txt")

	checkpointID := env.TryGetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("expected a checkpoint after stop hook")
	}

	storedTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpointID, paths.TranscriptFileName))
	if !found {
		t.Fatalf("checkpoint transcript not found for checkpoint %s", checkpointID)
	}

	liveTranscriptBytes, err := os.ReadFile(piSession.TranscriptPath)
	if err != nil {
		t.Fatalf("failed to read live transcript: %v", err)
	}
	if storedTranscript != string(liveTranscriptBytes) {
		t.Fatalf("checkpoint transcript does not match live transcript")
	}
}

func TestPiResumedSession_CheckpointIncludesHistoricalTranscript(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	piSession := env.NewPiSession()

	if err := env.SimulatePiSessionStart(piSession.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit turn1 failed: %v", err)
	}

	env.WriteFile("resume-history-1.txt", "turn1\n")
	turn1 := []map[string]interface{}{
		{
			"type": "message",
			"id":   "1",
			"message": map[string]interface{}{
				"role":    "user",
				"content": "turn one prompt",
			},
		},
		{
			"type": "message",
			"id":   "2",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "turn one response"},
					{
						"type": "toolCall",
						"id":   "tool-1",
						"name": "write",
						"arguments": map[string]interface{}{
							"path": "resume-history-1.txt",
						},
					},
				},
			},
		},
		{
			"type": "message",
			"id":   "3",
			"message": map[string]interface{}{
				"role":       "toolResult",
				"toolName":   "write",
				"toolCallId": "tool-1",
				"details": map[string]interface{}{
					"path": "resume-history-1.txt",
				},
			},
		},
	}
	writePiJSONL(t, piSession.TranscriptPath, turn1)

	if err := env.SimulatePiStop(piSession.ID, piSession.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop turn1 failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Commit resume-history turn1", "resume-history-1.txt")

	firstCheckpointID := env.TryGetLatestCheckpointID()
	if firstCheckpointID == "" {
		t.Fatal("expected first checkpoint after turn1 stop")
	}

	if err := env.SimulatePiSessionEnd(piSession.ID, piSession.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiSessionEnd failed: %v", err)
	}
	if err := env.SimulatePiSessionStart(piSession.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart (resume) failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit turn2 failed: %v", err)
	}

	env.WriteFile("resume-history-2.txt", "turn2\n")
	turn2WithHistory := append([]map[string]interface{}{}, turn1...)
	turn2WithHistory = append(turn2WithHistory,
		map[string]interface{}{
			"type": "message",
			"id":   "4",
			"message": map[string]interface{}{
				"role":    "user",
				"content": "turn two prompt",
			},
		},
		map[string]interface{}{
			"type": "message",
			"id":   "5",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "turn two response"},
					{
						"type": "toolCall",
						"id":   "tool-2",
						"name": "write",
						"arguments": map[string]interface{}{
							"path": "resume-history-2.txt",
						},
					},
				},
			},
		},
		map[string]interface{}{
			"type": "message",
			"id":   "6",
			"message": map[string]interface{}{
				"role":       "toolResult",
				"toolName":   "write",
				"toolCallId": "tool-2",
				"details": map[string]interface{}{
					"path": "resume-history-2.txt",
				},
			},
		},
	)
	writePiJSONL(t, piSession.TranscriptPath, turn2WithHistory)

	if err := env.SimulatePiStop(piSession.ID, piSession.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop turn2 failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Commit resume-history turn2", "resume-history-2.txt")

	secondCheckpointID := env.TryGetLatestCheckpointID()
	if secondCheckpointID == "" {
		t.Fatal("expected second checkpoint after resumed turn")
	}
	if secondCheckpointID == firstCheckpointID {
		t.Fatalf("expected different checkpoint IDs, got both %q", secondCheckpointID)
	}

	storedTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(secondCheckpointID, paths.TranscriptFileName))
	if !found {
		t.Fatalf("checkpoint transcript not found for checkpoint %s", secondCheckpointID)
	}

	if !strings.Contains(storedTranscript, "turn one prompt") {
		t.Fatalf("resumed checkpoint transcript missing historical prompt from turn one")
	}
	if !strings.Contains(storedTranscript, "turn two prompt") {
		t.Fatalf("resumed checkpoint transcript missing prompt from resumed turn")
	}
}

func TestPiStop_MissingTranscript_GracefulCheckpointing(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	piSession := env.NewPiSession()

	if err := env.SimulatePiSessionStart(piSession.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("missing-transcript.txt", "content despite missing transcript\n")

	// Canonical transcript source may be missing during teardown windows.
	missingTranscriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "missing-session.jsonl")
	if err := env.SimulatePiStop(piSession.ID, missingTranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop should be graceful with missing transcript, got error: %v", err)
	}

	state := loadSessionStateFromRepo(t, env.RepoDir, piSession.ID)
	if state == nil {
		t.Fatal("expected session state after graceful missing-transcript handling")
	}
	if state.Phase != session.PhaseIdle {
		t.Fatalf("session phase = %q, want %q", state.Phase, session.PhaseIdle)
	}
}
