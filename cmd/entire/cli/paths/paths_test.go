package paths

import (
	"entire.io/cli/cmd/entire/cli/sessionid"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestIsInfrastructurePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".entire/metadata/test", true},
		{".entire", true},
		{"src/main.go", false},
		{".entirefile", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsInfrastructurePath(tt.path)
			if got != tt.want {
				t.Errorf("IsInfrastructurePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSanitizePathForClaude(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/test/myrepo", "-Users-test-myrepo"},
		{"/home/user/project", "-home-user-project"},
		{"simple", "simple"},
		{"/path/with spaces/here", "-path-with-spaces-here"},
		{"/path.with.dots/file", "-path-with-dots-file"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizePathForClaude(tt.input)
			if got != tt.want {
				t.Errorf("SanitizePathForClaude(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetClaudeProjectDir_Override(t *testing.T) {
	// Set the override environment variable
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "/tmp/test-claude-project")

	result, err := GetClaudeProjectDir("/some/repo/path")
	if err != nil {
		t.Fatalf("GetClaudeProjectDir() error = %v", err)
	}

	if result != "/tmp/test-claude-project" {
		t.Errorf("GetClaudeProjectDir() = %q, want %q", result, "/tmp/test-claude-project")
	}
}

func TestGetClaudeProjectDir_Default(t *testing.T) {
	// Ensure env var is not set by setting it to empty string
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")

	result, err := GetClaudeProjectDir("/Users/test/myrepo")
	if err != nil {
		t.Fatalf("GetClaudeProjectDir() error = %v", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}
	expected := filepath.Join(homeDir, ".claude", "projects", "-Users-test-myrepo")

	if result != expected {
		t.Errorf("GetClaudeProjectDir() = %q, want %q", result, expected)
	}
}

func TestCurrentSessionFile(t *testing.T) {
	// Test that the constant is defined correctly
	if CurrentSessionFile != ".entire/current_session" {
		t.Errorf("CurrentSessionFile = %q, want %q", CurrentSessionFile, ".entire/current_session")
	}
}

func TestReadWriteCurrentSession(t *testing.T) {
	// Create temp directory to act as repo root
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Test reading non-existent file returns empty string (not error)
	sessionID, err := ReadCurrentSession()
	if err != nil {
		t.Errorf("ReadCurrentSession() on non-existent file error = %v, want nil", err)
	}
	if sessionID != "" {
		t.Errorf("ReadCurrentSession() on non-existent file = %q, want empty string", sessionID)
	}

	// Test writing creates directory and file
	testSessionID := "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e"
	err = WriteCurrentSession(testSessionID)
	if err != nil {
		t.Fatalf("WriteCurrentSession() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(CurrentSessionFile); os.IsNotExist(err) {
		t.Error("WriteCurrentSession() did not create file")
	}

	// Test reading back the session ID
	readSessionID, err := ReadCurrentSession()
	if err != nil {
		t.Errorf("ReadCurrentSession() error = %v, want nil", err)
	}
	if readSessionID != testSessionID {
		t.Errorf("ReadCurrentSession() = %q, want %q", readSessionID, testSessionID)
	}

	// Test overwriting existing session
	newSessionID := "2025-12-02-abcd1234"
	err = WriteCurrentSession(newSessionID)
	if err != nil {
		t.Errorf("WriteCurrentSession() overwrite error = %v", err)
	}

	readSessionID, err = ReadCurrentSession()
	if err != nil {
		t.Errorf("ReadCurrentSession() after overwrite error = %v", err)
	}
	if readSessionID != newSessionID {
		t.Errorf("ReadCurrentSession() after overwrite = %q, want %q", readSessionID, newSessionID)
	}
}

func TestReadCurrentSession_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create empty file
	if err := os.MkdirAll(EntireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}
	if err := os.WriteFile(CurrentSessionFile, []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create empty file: %v", err)
	}

	sessionID, err := ReadCurrentSession()
	if err != nil {
		t.Errorf("ReadCurrentSession() on empty file error = %v", err)
	}
	if sessionID != "" {
		t.Errorf("ReadCurrentSession() on empty file = %q, want empty string", sessionID)
	}
}

func TestEntireSessionID(t *testing.T) {
	claudeSessionID := "8f76b0e8-b8f1-4a87-9186-848bdd83d62e"

	result := sessionid.EntireSessionID(claudeSessionID)

	// Should match format: YYYY-MM-DD-<claude-session-id>
	pattern := `^\d{4}-\d{2}-\d{2}-` + regexp.QuoteMeta(claudeSessionID) + `$`
	matched, err := regexp.MatchString(pattern, result)
	if err != nil {
		t.Fatalf("regex error: %v", err)
	}
	if !matched {
		t.Errorf("sessionid.EntireSessionID() = %q, want format YYYY-MM-DD-%s", result, claudeSessionID)
	}
}

func TestEntireSessionID_PreservesInput(t *testing.T) {
	tests := []struct {
		name            string
		claudeSessionID string
	}{
		{"simple uuid", "abc123"},
		{"full uuid", "8f76b0e8-b8f1-4a87-9186-848bdd83d62e"},
		{"with special chars", "test-session_123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sessionid.EntireSessionID(tt.claudeSessionID)

			// Should end with the original Claude session ID
			suffix := "-" + tt.claudeSessionID
			if len(result) < len(suffix) || result[len(result)-len(suffix):] != suffix {
				t.Errorf("sessionid.EntireSessionID(%q) = %q, should end with %q", tt.claudeSessionID, result, suffix)
			}

			// Should start with date prefix (11 chars: YYYY-MM-DD-)
			if len(result) < 11 {
				t.Errorf("sessionid.EntireSessionID(%q) = %q, too short for date prefix", tt.claudeSessionID, result)
			}
		})
	}
}
