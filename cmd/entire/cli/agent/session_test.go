//nolint:govet // Test file with struct field assignments for completeness
package agent

import (
	"testing"
	"time"
)

func TestAgentSessionStructure(t *testing.T) {
	t.Parallel()

	session := AgentSession{
		SessionID:     "test-session-123",
		AgentName:     "claude-code",
		RepoPath:      "/path/to/repo",
		SessionRef:    "/path/to/session/file",
		StartTime:     time.Now(),
		ModifiedFiles: []string{"file1.go"},
		NewFiles:      []string{"file2.go"},
		DeletedFiles:  []string{"file3.go"},
	}

	if session.SessionID != "test-session-123" {
		t.Errorf("expected SessionID %q, got %q", "test-session-123", session.SessionID)
	}
	if session.AgentName != "claude-code" {
		t.Errorf("expected AgentName %q, got %q", "claude-code", session.AgentName)
	}
}
