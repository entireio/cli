//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// claudeMemoryNote stands in for a developer's private memory note: text that
// lives outside the repository and that no redaction rule can recognise.
const claudeMemoryNote = "Client Acme Bank rollout is tracked in the Globex ledger"

// claudeInjectedRecords are the records Claude Code writes for the text it
// attaches to a model request, each carrying claudeMemoryNote: the instruction
// files it loaded, its system prompt snapshot, and its request log.
func claudeInjectedRecords(blobHash string) string {
	return strings.Join([]string{
		`{"parentUuid":"u1","attachment":{"type":"instructions","files":[{"path":"/home/dev/.claude/memory/clients.md","type":"User","content":"` + claudeMemoryNote + `"}]},"type":"attachment","uuid":"i-` + blobHash + `","rendered":[{"content":"<system-reminder>` + claudeMemoryNote + `</system-reminder>"}]}`,
		`{"parentUuid":"i-` + blobHash + `","attachment":{"type":"prompt_snapshot","systemPrompt":["# Memory ` + claudeMemoryNote + `"]},"type":"attachment","uuid":"p-` + blobHash + `"}`,
		`{"type":"api-request-shape","shapeHash":"shape-` + blobHash + `","shape":{"system":[{"type":"text","text":"` + claudeMemoryNote + `"}],"tools":[]}}`,
		`{"type":"api-request-blob","hash":"` + blobHash + `","message":{"role":"user","content":[{"type":"text","text":"` + claudeMemoryNote + `"}]}}`,
		`{"type":"api-request","id":"r-` + blobHash + `","shapeHash":"shape-` + blobHash + `","tail":["` + blobHash + `"]}`,
	}, "\n") + "\n"
}

// appendToTranscript appends raw JSONL to a session's transcript, as the agent
// does between hooks.
func appendToTranscript(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append to transcript: %v", err)
	}
}

func readTranscriptFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return string(content)
}

// TestClaudeCheckpoint_DoesNotStoreInjectedInstructionText proves that the text
// Claude Code attaches to its model requests reaches neither the Stop hook's
// working copy nor the committed checkpoint, while the conversation and the
// line numbering both survive.
func TestClaudeCheckpoint_DoesNotStoreInjectedInstructionText(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("feature.txt", "hi\n")
	session.CreateTranscript("add feature.txt", []FileChange{{Path: "feature.txt", Content: "hi\n"}})
	appendToTranscript(t, session.TranscriptPath, claudeInjectedRecords("blob-1"))
	agentTranscript := readTranscriptFile(t, session.TranscriptPath)

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}

	working, ok := findStoredSessionTranscript(t, env.RepoDir)
	if !ok {
		t.Fatal("stop stored no session transcript")
	}
	if strings.Contains(working, claudeMemoryNote) {
		t.Error("the Stop hook's working copy still holds the memory note")
	}

	env.GitCommitWithHooks("add feature.txt", "feature.txt")
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	if checkpointID == "" {
		t.Fatal("commit produced no checkpoint")
	}
	stored, found := checkpointBlob(env, checkpointID, "0/"+paths.TranscriptFileName)
	if !found {
		t.Fatal("checkpoint has no stored transcript")
	}

	if strings.Contains(stored, claudeMemoryNote) {
		t.Error("the committed checkpoint still holds the memory note")
	}
	for _, kept := range []string{
		"add feature.txt", "Done!",
		`"type":"instructions"`, `"path":"/home/dev/.claude/memory/clients.md"`,
		`"type":"entire_blanked_prompt_snapshot"`, `"hash":"blob-1"`, `"shapeHash":"shape-blob-1"`,
	} {
		if !strings.Contains(stored, kept) {
			t.Errorf("stored transcript lost %q", kept)
		}
	}
	// Claude Code resumes a recorded prompt_snapshot verbatim, so an emptied one
	// left under that kind would resume the session with no system prompt.
	if strings.Contains(stored, `"type":"prompt_snapshot"`) {
		t.Error("the stored transcript keeps an emptied prompt_snapshot that Claude Code would resume with")
	}
	if got, want := countJSONLLines(stored), countJSONLLines(agentTranscript); got != want {
		t.Errorf("stored transcript has %d lines, the agent's has %d: offsets into the stored copy will drift", got, want)
	}

	// The agent's own file is never rewritten.
	if got := readTranscriptFile(t, session.TranscriptPath); got != agentTranscript {
		t.Error("the agent's transcript was modified")
	}
}

// TestClaudeCheckpoint_GrowthStillDetectedAfterCommit guards the growth baseline:
// the stored copy is much smaller than the agent's transcript once its request
// log is blanked, and the next commit must still see new content and condense.
func TestClaudeCheckpoint_GrowthStillDetectedAfterCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("feature.txt", "hi\n")
	session.CreateTranscript("add feature.txt", []FileChange{{Path: "feature.txt", Content: "hi\n"}})
	appendToTranscript(t, session.TranscriptPath, strings.Repeat(claudeInjectedRecords("blob-1"), 20))
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}
	env.GitCommitWithHooks("add feature.txt", "feature.txt")
	firstCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if firstCheckpoint == "" {
		t.Fatal("first commit produced no checkpoint")
	}

	// Turn 2 adds one short exchange: far less than the blanked request log took out.
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("second user-prompt-submit: %v", err)
	}
	env.WriteFile("second.txt", "yo\n")
	session.CreateTranscript("now add second.txt", []FileChange{{Path: "second.txt", Content: "yo\n"}})
	appendToTranscript(t, session.TranscriptPath, strings.Repeat(claudeInjectedRecords("blob-1"), 20)+claudeInjectedRecords("blob-2"))
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	env.GitCommitWithHooks("add second.txt", "second.txt")

	secondCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if secondCheckpoint == "" {
		t.Fatal("second commit produced no checkpoint")
	}
	if secondCheckpoint == firstCheckpoint {
		t.Fatalf("second commit did not condense a new checkpoint; both report %s", firstCheckpoint)
	}
	stored, found := checkpointBlob(env, secondCheckpoint, "0/"+paths.TranscriptFileName)
	if !found {
		t.Fatal("second checkpoint has no stored transcript")
	}
	if strings.Contains(stored, claudeMemoryNote) {
		t.Error("the second checkpoint still holds the memory note")
	}
	if !strings.Contains(stored, "now add second.txt") {
		t.Error("the second checkpoint lost the new prompt")
	}
}
