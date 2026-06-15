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
		Entries:       []SessionEntry{},
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

func TestSessionEntryStructure(t *testing.T) {
	t.Parallel()

	entry := SessionEntry{
		UUID:          "entry-uuid-123",
		Type:          EntryTool,
		Timestamp:     time.Now(),
		Content:       "Tool output",
		ToolName:      "Write",
		ToolInput:     map[string]interface{}{"file_path": "test.go"},
		ToolOutput:    "file written",
		FilesAffected: []string{"test.go"},
	}

	if entry.UUID != "entry-uuid-123" {
		t.Errorf("expected UUID %q, got %q", "entry-uuid-123", entry.UUID)
	}
	if entry.Type != EntryTool {
		t.Errorf("expected Type %q, got %q", EntryTool, entry.Type)
	}
}

func TestGetLastUserPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entries  []SessionEntry
		expected string
	}{
		{
			name:     "empty session",
			entries:  []SessionEntry{},
			expected: "",
		},
		{
			name: "single user entry",
			entries: []SessionEntry{
				{Type: EntryUser, Content: "hello"},
			},
			expected: "hello",
		},
		{
			name: "multiple entries, user last",
			entries: []SessionEntry{
				{Type: EntryUser, Content: "first"},
				{Type: EntryAssistant, Content: "response"},
				{Type: EntryUser, Content: "second"},
			},
			expected: "second",
		},
		{
			name: "multiple entries, assistant last",
			entries: []SessionEntry{
				{Type: EntryUser, Content: "question"},
				{Type: EntryAssistant, Content: "answer"},
			},
			expected: "question",
		},
		{
			name: "no user entries",
			entries: []SessionEntry{
				{Type: EntrySystem, Content: "system message"},
				{Type: EntryAssistant, Content: "greeting"},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := &AgentSession{Entries: tt.entries}
			result := session.GetLastUserPrompt()
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestGetLastAssistantResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entries  []SessionEntry
		expected string
	}{
		{
			name:     "empty session",
			entries:  []SessionEntry{},
			expected: "",
		},
		{
			name: "single assistant entry",
			entries: []SessionEntry{
				{Type: EntryAssistant, Content: "hello"},
			},
			expected: "hello",
		},
		{
			name: "multiple entries, assistant last",
			entries: []SessionEntry{
				{Type: EntryUser, Content: "question"},
				{Type: EntryAssistant, Content: "answer"},
			},
			expected: "answer",
		},
		{
			name: "multiple assistant entries",
			entries: []SessionEntry{
				{Type: EntryAssistant, Content: "first response"},
				{Type: EntryUser, Content: "follow up"},
				{Type: EntryAssistant, Content: "second response"},
			},
			expected: "second response",
		},
		{
			name: "no assistant entries",
			entries: []SessionEntry{
				{Type: EntryUser, Content: "question"},
				{Type: EntryTool, Content: "tool output"},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := &AgentSession{Entries: tt.entries}
			result := session.GetLastAssistantResponse()
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestTruncateAtUUID(t *testing.T) {
	t.Parallel()

	t.Run("empty uuid returns original", func(t *testing.T) {
		t.Parallel()

		session := &AgentSession{
			SessionID: "test",
			Entries: []SessionEntry{
				{UUID: "1", Content: "first"},
				{UUID: "2", Content: "second"},
			},
		}
		result := session.TruncateAtUUID("")
		if result != session {
			t.Error("expected same session for empty UUID")
		}
	})

	t.Run("truncates at uuid", func(t *testing.T) {
		t.Parallel()

		session := &AgentSession{
			SessionID: "test",
			AgentName: "claude-code",
			RepoPath:  "/repo",
			Entries: []SessionEntry{
				{UUID: "1", Content: "first", FilesAffected: []string{"a.go"}},
				{UUID: "2", Content: "second", FilesAffected: []string{"b.go"}},
				{UUID: "3", Content: "third", FilesAffected: []string{"c.go"}},
			},
		}
		result := session.TruncateAtUUID("2")

		if len(result.Entries) != 2 {
			t.Errorf("expected 2 entries, got %d", len(result.Entries))
		}
		if result.SessionID != "test" {
			t.Error("session metadata should be preserved")
		}
	})

	t.Run("uuid not found includes all entries", func(t *testing.T) {
		t.Parallel()

		session := &AgentSession{
			SessionID: "test",
			Entries: []SessionEntry{
				{UUID: "1", Content: "first"},
				{UUID: "2", Content: "second"},
			},
		}
		result := session.TruncateAtUUID("nonexistent")

		if len(result.Entries) != 2 {
			t.Errorf("expected 2 entries, got %d", len(result.Entries))
		}
	})
}

func TestFindToolResultUUID(t *testing.T) {
	t.Parallel()

	session := &AgentSession{
		Entries: []SessionEntry{
			{UUID: "user-1", Type: EntryUser},
			{UUID: "tool-1", Type: EntryTool},
			{UUID: "assistant-1", Type: EntryAssistant},
			{UUID: "tool-2", Type: EntryTool},
		},
	}

	t.Run("finds existing tool uuid", func(t *testing.T) {
		t.Parallel()

		uuid, found := session.FindToolResultUUID("tool-1")
		if !found {
			t.Error("expected to find tool-1")
		}
		if uuid != "tool-1" {
			t.Errorf("expected uuid %q, got %q", "tool-1", uuid)
		}
	})

	t.Run("returns empty for non-tool entry", func(t *testing.T) {
		t.Parallel()

		_, found := session.FindToolResultUUID("user-1")
		if found {
			t.Error("expected not to find user-1 as tool")
		}
	})

	t.Run("returns empty for nonexistent uuid", func(t *testing.T) {
		t.Parallel()

		_, found := session.FindToolResultUUID("nonexistent")
		if found {
			t.Error("expected not to find nonexistent uuid")
		}
	})
}
