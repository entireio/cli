package trailers

import (
	"testing"
)

func TestFormatMetadata(t *testing.T) {
	message := "Update authentication logic"
	metadataDir := ".entire/metadata/2025-01-28-abc123"

	expected := "Update authentication logic\n\nEntire-Metadata: .entire/metadata/2025-01-28-abc123\n"
	got := FormatMetadata(message, metadataDir)

	if got != expected {
		t.Errorf("FormatMetadata() = %q, want %q", got, expected)
	}
}

func TestParseMetadata(t *testing.T) {
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
			gotDir, gotFound := ParseMetadata(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseMetadata() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotDir != tt.wantDir {
				t.Errorf("ParseMetadata() dir = %v, want %v", gotDir, tt.wantDir)
			}
		})
	}
}

func TestFormatTaskMetadata(t *testing.T) {
	message := "Task: Implement feature X"
	taskMetadataDir := ".entire/metadata/2025-01-28-abc123/tasks/toolu_xyz"

	expected := "Task: Implement feature X\n\nEntire-Metadata-Task: .entire/metadata/2025-01-28-abc123/tasks/toolu_xyz\n"
	got := FormatTaskMetadata(message, taskMetadataDir)

	if got != expected {
		t.Errorf("FormatTaskMetadata() = %q, want %q", got, expected)
	}
}

func TestParseTaskMetadata(t *testing.T) {
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
			gotDir, gotFound := ParseTaskMetadata(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseTaskMetadata() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotDir != tt.wantDir {
				t.Errorf("ParseTaskMetadata() dir = %v, want %v", gotDir, tt.wantDir)
			}
		})
	}
}

func TestParseBaseCommit(t *testing.T) {
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
			gotSHA, gotFound := ParseBaseCommit(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseBaseCommit() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotSHA != tt.wantSHA {
				t.Errorf("ParseBaseCommit() sha = %v, want %v", gotSHA, tt.wantSHA)
			}
		})
	}
}

func TestParseSession(t *testing.T) {
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
			gotID, gotFound := ParseSession(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseSession() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotID != tt.wantID {
				t.Errorf("ParseSession() id = %v, want %v", gotID, tt.wantID)
			}
		})
	}
}

func TestParseAllSessions(t *testing.T) {
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
			got := ParseAllSessions(tt.message)
			if len(got) != len(tt.want) {
				t.Errorf("ParseAllSessions() returned %d items, want %d", len(got), len(tt.want))
				t.Errorf("got: %v, want: %v", got, tt.want)
				return
			}
			for i, wantID := range tt.want {
				if got[i] != wantID {
					t.Errorf("ParseAllSessions()[%d] = %v, want %v", i, got[i], wantID)
				}
			}
		})
	}
}

func TestParseCheckpoint(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantID    string
		wantFound bool
	}{
		{
			name:      "valid checkpoint trailer",
			message:   "Add feature\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			wantID:    "a1b2c3d4e5f6",
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
			message:   "Message\n\nEntire-Checkpoint:   a1b2c3d4e5f6   \n",
			wantID:    "a1b2c3d4e5f6",
			wantFound: true,
		},
		{
			name:      "too short checkpoint ID",
			message:   "Message\n\nEntire-Checkpoint: abc123\n",
			wantID:    "",
			wantFound: false,
		},
		{
			name:      "too long checkpoint ID",
			message:   "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6789\n",
			wantID:    "",
			wantFound: false,
		},
		{
			name:      "invalid characters in checkpoint ID",
			message:   "Message\n\nEntire-Checkpoint: a1b2c3d4e5gg\n",
			wantID:    "",
			wantFound: false,
		},
		{
			name:      "uppercase hex rejected",
			message:   "Message\n\nEntire-Checkpoint: A1B2C3D4E5F6\n",
			wantID:    "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotFound := ParseCheckpoint(tt.message)
			if gotFound != tt.wantFound {
				t.Errorf("ParseCheckpoint() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotID.String() != tt.wantID {
				t.Errorf("ParseCheckpoint() id = %v, want %v", gotID.String(), tt.wantID)
			}
		})
	}
}
