package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestFormatMetadataTrailer(t *testing.T) {
	message := "Update authentication logic"
	metadataDir := ".entire/metadata/2025-01-28-abc123"

	expected := "Update authentication logic\n\nEntire-Metadata: .entire/metadata/2025-01-28-abc123\n"
	got := FormatMetadataTrailer(message, metadataDir)

	if got != expected {
		t.Errorf("FormatMetadataTrailer() = %q, want %q", got, expected)
	}
}

func TestParseMetadataTrailer(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantDir   string
		wantFound bool
	}{
		{
			name:      "standard commit message",
			message:   "Update logic\n\nEntire-Metadata: .entire/metadata/2025-01-28-abc123\n",
			wantDir:   ".entire/metadata/2025-01-28-abc123",
			wantFound: true,
		},
		{
			name:      "no trailer",
			message:   "Simple commit message",
			wantDir:   "",
			wantFound: false,
		},
		{
			name:      "trailer with extra spaces",
			message:   "Message\n\nEntire-Metadata:   .entire/metadata/xyz   \n",
			wantDir:   ".entire/metadata/xyz",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, gotFound := ParseMetadataTrailer(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseMetadataTrailer() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotDir != tt.wantDir {
				t.Errorf("ParseMetadataTrailer() dir = %v, want %v", gotDir, tt.wantDir)
			}
		})
	}
}

func TestFormatTaskMetadataTrailer(t *testing.T) {
	message := "Task: Implement feature X"
	taskMetadataDir := ".entire/metadata/2025-01-28-abc123/tasks/toolu_xyz"

	expected := "Task: Implement feature X\n\nEntire-Metadata-Task: .entire/metadata/2025-01-28-abc123/tasks/toolu_xyz\n"
	got := FormatTaskMetadataTrailer(message, taskMetadataDir)

	if got != expected {
		t.Errorf("FormatTaskMetadataTrailer() = %q, want %q", got, expected)
	}
}

func TestParseTaskMetadataTrailer(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantDir   string
		wantFound bool
	}{
		{
			name:      "task commit message",
			message:   "Task: Feature\n\nEntire-Metadata-Task: .entire/metadata/2025-01-28-abc/tasks/toolu_123\n",
			wantDir:   ".entire/metadata/2025-01-28-abc/tasks/toolu_123",
			wantFound: true,
		},
		{
			name:      "no task trailer",
			message:   "Simple commit message",
			wantDir:   "",
			wantFound: false,
		},
		{
			name:      "regular metadata trailer not matched",
			message:   "Message\n\nEntire-Metadata: .entire/metadata/xyz\n",
			wantDir:   "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, gotFound := ParseTaskMetadataTrailer(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseTaskMetadataTrailer() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotDir != tt.wantDir {
				t.Errorf("ParseTaskMetadataTrailer() dir = %v, want %v", gotDir, tt.wantDir)
			}
		})
	}
}

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

func TestWriteCurrentSession_CreatesReadme(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Write a session - this should also create README
	if err := WriteCurrentSession("test-session-123"); err != nil {
		t.Fatalf("WriteCurrentSession() error = %v", err)
	}

	// Verify README was created
	readmePath := filepath.Join(EntireDir, ReadmeFileName)
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("README should exist at %s: %v", readmePath, err)
	}

	// Verify content matches expected
	if string(content) != EntireDirReadme {
		t.Errorf("README content mismatch\ngot:\n%s\nwant:\n%s", string(content), EntireDirReadme)
	}
}

func TestWriteCurrentSession_DoesNotOverwriteExistingReadme(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create .entire directory with custom README
	if err := os.MkdirAll(EntireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}
	customContent := "# Custom README\n\nUser-modified content\n"
	readmePath := filepath.Join(EntireDir, ReadmeFileName)
	if err := os.WriteFile(readmePath, []byte(customContent), 0o644); err != nil {
		t.Fatalf("failed to write custom README: %v", err)
	}

	// Write a session - should NOT overwrite existing README
	if err := WriteCurrentSession("test-session-456"); err != nil {
		t.Fatalf("WriteCurrentSession() error = %v", err)
	}

	// Verify README still has custom content
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read README: %v", err)
	}

	if string(content) != customContent {
		t.Errorf("README was overwritten\ngot:\n%s\nwant:\n%s", string(content), customContent)
	}
}

func TestParseBaseCommitTrailer(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantSHA   string
		wantFound bool
	}{
		{
			name:      "valid 40-char SHA",
			message:   "Checkpoint\n\nBase-Commit: abc123def456789012345678901234567890abcd\n",
			wantSHA:   "abc123def456789012345678901234567890abcd",
			wantFound: true,
		},
		{
			name:      "no trailer",
			message:   "Simple commit message",
			wantSHA:   "",
			wantFound: false,
		},
		{
			name:      "short hash rejected",
			message:   "Message\n\nBase-Commit: abc123\n",
			wantSHA:   "",
			wantFound: false,
		},
		{
			name:      "with multiple trailers",
			message:   "Session\n\nBase-Commit: 0123456789abcdef0123456789abcdef01234567\nEntire-Strategy: linear-shadow\n",
			wantSHA:   "0123456789abcdef0123456789abcdef01234567",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSHA, gotFound := ParseBaseCommitTrailer(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseBaseCommitTrailer() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotSHA != tt.wantSHA {
				t.Errorf("ParseBaseCommitTrailer() sha = %v, want %v", gotSHA, tt.wantSHA)
			}
		})
	}
}

func TestEntireSessionID(t *testing.T) {
	claudeSessionID := "8f76b0e8-b8f1-4a87-9186-848bdd83d62e"

	result := EntireSessionID(claudeSessionID)

	// Should match format: YYYY-MM-DD-<claude-session-id>
	pattern := `^\d{4}-\d{2}-\d{2}-` + regexp.QuoteMeta(claudeSessionID) + `$`
	matched, err := regexp.MatchString(pattern, result)
	if err != nil {
		t.Fatalf("regex error: %v", err)
	}
	if !matched {
		t.Errorf("EntireSessionID() = %q, want format YYYY-MM-DD-%s", result, claudeSessionID)
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
			result := EntireSessionID(tt.claudeSessionID)

			// Should end with the original Claude session ID
			suffix := "-" + tt.claudeSessionID
			if len(result) < len(suffix) || result[len(result)-len(suffix):] != suffix {
				t.Errorf("EntireSessionID(%q) = %q, should end with %q", tt.claudeSessionID, result, suffix)
			}

			// Should start with date prefix (11 chars: YYYY-MM-DD-)
			if len(result) < 11 {
				t.Errorf("EntireSessionID(%q) = %q, too short for date prefix", tt.claudeSessionID, result)
			}
		})
	}
}

func TestSessionMetadataDir(t *testing.T) {
	claudeSessionID := "abc123"

	result := SessionMetadataDir(claudeSessionID)

	// Should match format: .entire/metadata/YYYY-MM-DD-<claude-session-id>
	pattern := `^\.entire/metadata/\d{4}-\d{2}-\d{2}-` + regexp.QuoteMeta(claudeSessionID) + `$`
	matched, err := regexp.MatchString(pattern, result)
	if err != nil {
		t.Fatalf("regex error: %v", err)
	}
	if !matched {
		t.Errorf("SessionMetadataDir() = %q, want format .entire/metadata/YYYY-MM-DD-%s", result, claudeSessionID)
	}
}

func TestParseSessionTrailer(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantID    string
		wantFound bool
	}{
		{
			name:      "single session trailer",
			message:   "Update logic\n\nEntire-Session: 2025-12-10-abc123def\n",
			wantID:    "2025-12-10-abc123def",
			wantFound: true,
		},
		{
			name:      "no trailer",
			message:   "Simple commit message",
			wantID:    "",
			wantFound: false,
		},
		{
			name:      "trailer with extra spaces",
			message:   "Message\n\nEntire-Session:   2025-12-10-xyz789   \n",
			wantID:    "2025-12-10-xyz789",
			wantFound: true,
		},
		{
			name:      "multiple trailers returns first",
			message:   "Merge\n\nEntire-Session: session-1\nEntire-Session: session-2\n",
			wantID:    "session-1",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotFound := ParseSessionTrailer(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseSessionTrailer() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotID != tt.wantID {
				t.Errorf("ParseSessionTrailer() id = %v, want %v", gotID, tt.wantID)
			}
		})
	}
}

func TestParseAllSessionTrailers(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{
			name:    "single session trailer",
			message: "Update logic\n\nEntire-Session: 2025-12-10-abc123def\n",
			want:    []string{"2025-12-10-abc123def"},
		},
		{
			name:    "no trailer",
			message: "Simple commit message",
			want:    nil,
		},
		{
			name:    "multiple session trailers",
			message: "Merge commit\n\nEntire-Session: session-1\nEntire-Session: session-2\nEntire-Session: session-3\n",
			want:    []string{"session-1", "session-2", "session-3"},
		},
		{
			name:    "duplicate session IDs are deduplicated",
			message: "Merge\n\nEntire-Session: session-1\nEntire-Session: session-2\nEntire-Session: session-1\n",
			want:    []string{"session-1", "session-2"},
		},
		{
			name:    "trailers with extra spaces",
			message: "Message\n\nEntire-Session:   session-a   \nEntire-Session:  session-b \n",
			want:    []string{"session-a", "session-b"},
		},
		{
			name:    "mixed with other trailers",
			message: "Merge\n\nEntire-Session: session-1\nEntire-Metadata: .entire/metadata/xyz\nEntire-Session: session-2\n",
			want:    []string{"session-1", "session-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseAllSessionTrailers(tt.message)
			if len(got) != len(tt.want) {
				t.Errorf("ParseAllSessionTrailers() returned %d items, want %d", len(got), len(tt.want))
				t.Errorf("got: %v, want: %v", got, tt.want)
				return
			}
			for i, wantID := range tt.want {
				if got[i] != wantID {
					t.Errorf("ParseAllSessionTrailers()[%d] = %v, want %v", i, got[i], wantID)
				}
			}
		})
	}
}
