package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"

	// Register agents so GetByAgentType works in tests.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
)

func TestResolveTranscriptPath_FileExists(t *testing.T) {
	t.Parallel()

	// When the transcript file exists at the stored path, resolveTranscriptPath
	// should return that path unchanged.
	tmpDir := t.TempDir()
	transcriptFile := filepath.Join(tmpDir, "session-123.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"test":true}`), 0o600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	state := &session.State{
		TranscriptPath: transcriptFile,
		AgentType:      agent.AgentTypeCursor,
	}

	resolved, err := resolveTranscriptPath(t.Context(), state)
	if err != nil {
		t.Fatalf("resolveTranscriptPath() error = %v", err)
	}
	if resolved != transcriptFile {
		t.Errorf("resolveTranscriptPath() = %q, want %q", resolved, transcriptFile)
	}
	if state.TranscriptPath != transcriptFile {
		t.Errorf("state.TranscriptPath changed to %q, should remain %q", state.TranscriptPath, transcriptFile)
	}
}

func TestResolveTranscriptPath_ReResolvesToNestedLayout(t *testing.T) {
	t.Parallel()

	// Simulates Cursor CLI relocating a transcript mid-session:
	// Stored path (flat):  <dir>/<uuid>.jsonl  (does not exist)
	// Actual path (nested): <dir>/<uuid>/<uuid>.jsonl  (exists)
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "agent-transcripts")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	agentSessionID := "87874108-eff2-47a0-b260-183961dd6cb0"
	flatPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	// Create the file at the nested path only
	nestedDir := filepath.Join(sessionDir, agentSessionID)
	if err := os.MkdirAll(nestedDir, 0o750); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}
	nestedPath := filepath.Join(nestedDir, agentSessionID+".jsonl")
	if err := os.WriteFile(nestedPath, []byte(`{"role":"user"}`), 0o600); err != nil {
		t.Fatalf("failed to write nested transcript: %v", err)
	}

	state := &session.State{
		TranscriptPath: flatPath,
		AgentType:      agent.AgentTypeCursor,
	}

	resolved, err := resolveTranscriptPath(t.Context(), state)
	if err != nil {
		t.Fatalf("resolveTranscriptPath() error = %v", err)
	}
	if resolved != nestedPath {
		t.Errorf("resolveTranscriptPath() = %q, want %q", resolved, nestedPath)
	}
	// State should be updated to the resolved path
	if state.TranscriptPath != nestedPath {
		t.Errorf("state.TranscriptPath = %q, want %q", state.TranscriptPath, nestedPath)
	}
}

func TestResolveTranscriptPath_FileNotFoundAndCannotResolve(t *testing.T) {
	t.Parallel()

	// When the file doesn't exist and re-resolution also fails, the original
	// not-found error should be returned.
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "agent-transcripts")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	agentSessionID := "nonexistent-uuid"
	flatPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	state := &session.State{
		TranscriptPath: flatPath,
		AgentType:      agent.AgentTypeCursor,
	}

	_, err := resolveTranscriptPath(t.Context(), state)
	if err == nil {
		t.Fatal("resolveTranscriptPath() expected error, got nil")
	}
	// State should not change
	if state.TranscriptPath != flatPath {
		t.Errorf("state.TranscriptPath changed to %q, should remain %q", state.TranscriptPath, flatPath)
	}
}

func TestResolveTranscriptPath_UnknownAgentFallsThrough(t *testing.T) {
	t.Parallel()

	// When the agent type is unknown, re-resolution should not be attempted,
	// and the original not-found error should be returned.
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "nonexistent.jsonl")

	state := &session.State{
		TranscriptPath: missingPath,
		AgentType:      "Unknown Agent",
	}

	_, err := resolveTranscriptPath(t.Context(), state)
	if err == nil {
		t.Fatal("resolveTranscriptPath() expected error, got nil")
	}
}

func TestResolveTranscriptPath_EmptyPath(t *testing.T) {
	t.Parallel()

	state := &session.State{
		TranscriptPath: "",
		AgentType:      agent.AgentTypeCursor,
	}

	_, err := resolveTranscriptPath(t.Context(), state)
	if err == nil {
		t.Fatal("resolveTranscriptPath() expected error for empty path, got nil")
	}
}

func TestResolveTranscriptPath_DirectoryPathReturnsAsIs(t *testing.T) {
	t.Parallel()

	// When the path is a directory (not a file), os.Stat succeeds so
	// resolveTranscriptPath returns the path as-is. The caller will get
	// an error when it tries os.ReadFile.
	tmpDir := t.TempDir()
	dirPath := filepath.Join(tmpDir, "adir")
	if err := os.MkdirAll(dirPath, 0o750); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	state := &session.State{
		TranscriptPath: dirPath,
		AgentType:      agent.AgentTypeCursor,
	}

	resolved, err := resolveTranscriptPath(t.Context(), state)
	if err != nil {
		t.Fatalf("resolveTranscriptPath() error = %v", err)
	}
	if resolved != dirPath {
		t.Errorf("resolveTranscriptPath() = %q, want %q", resolved, dirPath)
	}
}

func TestResolveTranscriptPath_ClaudeCodeNoReResolution(t *testing.T) {
	t.Parallel()

	// Claude Code transcripts don't relocate mid-session. resolveTranscriptPath
	// should still attempt re-resolution (the agent's ResolveSessionFile handles it).
	// This test verifies the function works generically for any agent.
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "projects", "sessions")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	missingPath := filepath.Join(sessionDir, "session-abc.jsonl")

	state := &session.State{
		TranscriptPath: missingPath,
		AgentType:      agent.AgentTypeClaudeCode,
	}

	_, err := resolveTranscriptPath(t.Context(), state)
	if err == nil {
		t.Fatal("resolveTranscriptPath() expected error, got nil")
	}
	// Path should remain unchanged since re-resolution also fails
	if state.TranscriptPath != missingPath {
		t.Errorf("state.TranscriptPath = %q, want %q", state.TranscriptPath, missingPath)
	}
}
