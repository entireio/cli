package trailers

import (
	"testing"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestAppendCheckpointTrailer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "no existing trailers",
			msg:  "feat: add attach command\n",
			want: "feat: add attach command\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "existing non-checkpoint trailer block",
			msg:  "feat: add attach command\n\nSigned-off-by: Test User <test@example.com>\n",
			want: "feat: add attach command\n\nSigned-off-by: Test User <test@example.com>\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "existing checkpoint trailer block",
			msg:  "feat: add attach command\n\nEntire-Checkpoint: deadbeefcafe\n",
			want: "feat: add attach command\n\nEntire-Checkpoint: deadbeefcafe\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "subject with colon is not trailer block",
			msg:  "docs: update readme\n",
			want: "docs: update readme\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "body text containing colon-space is not trailer block",
			msg:  "fix: login\n\nThis fixes the error: connection refused\n",
			want: "fix: login\n\nThis fixes the error: connection refused\n\nEntire-Checkpoint: abc123def456\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AppendCheckpointTrailer(tt.msg, "abc123def456")
			if got != tt.want {
				t.Errorf("AppendCheckpointTrailer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsTrailerLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want bool
	}{
		{"Signed-off-by: User <user@example.com>", true},
		{"Entire-Checkpoint: abc123def456", true},
		{"not a trailer", false},
		{"error: connection refused", true}, // "error" is a valid trailer key format
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			t.Parallel()
			if got := IsTrailerLine(tt.line); got != tt.want {
				t.Errorf("IsTrailerLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

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

func TestParseAllCheckpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{
			name:    "single checkpoint trailer",
			message: "Add feature\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "no trailer",
			message: "Simple commit message",
			want:    nil,
		},
		{
			name:    "multiple checkpoint trailers from squash merge",
			message: "Soph/test branch (#2)\n\n* random_letter script\n\nEntire-Checkpoint: 0aa0814d9839\n\n* random color\n\nEntire-Checkpoint: 33fb587b6fbb\n",
			want:    []string{"0aa0814d9839", "33fb587b6fbb"},
		},
		{
			name:    "duplicate checkpoint IDs are deduplicated",
			message: "Merge\n\nEntire-Checkpoint: a1b2c3d4e5f6\nEntire-Checkpoint: b2c3d4e5f6a1\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
		},
		{
			name:    "invalid checkpoint IDs are skipped",
			message: "Merge\n\nEntire-Checkpoint: a1b2c3d4e5f6\nEntire-Checkpoint: tooshort\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			want:    []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
		},
		{
			name:    "mixed with other trailers",
			message: "Merge\n\nEntire-Checkpoint: a1b2c3d4e5f6\nEntire-Session: session-1\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			want:    []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
		},
		{
			name:    "mixed hex and ULID checkpoint trailers",
			message: "Squash (#3)\n\n* legacy\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n* new\n\nEntire-Checkpoint: 01KVBJCWYA4YW6J5M9GP655HZN\n",
			want:    []string{"a1b2c3d4e5f6", "01KVBJCWYA4YW6J5M9GP655HZN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseAllCheckpoints(tt.message)
			if len(got) != len(tt.want) {
				t.Errorf("ParseAllCheckpoints() returned %d items, want %d", len(got), len(tt.want))
				t.Errorf("got: %v, want: %v", got, tt.want)
				return
			}
			for i, wantID := range tt.want {
				expectedID := checkpointID.MustCheckpointID(wantID)
				if got[i] != expectedID {
					t.Errorf("ParseAllCheckpoints()[%d] = %v, want %v", i, got[i], expectedID)
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
			name:      "valid ULID checkpoint trailer",
			message:   "Add feature\n\nEntire-Checkpoint: 01KVBJCWYA4YW6J5M9GP655HZN\n",
			wantID:    "01KVBJCWYA4YW6J5M9GP655HZN",
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

// TestHasOPFApplied covers the Entire-OPF-Applied trailer reader. The
// trailer marks a v1 commit whose blobs have been redacted by the
// OpenAI Privacy Filter (8-layer); commits without it carry 7-layer
// content and are eligible for the pre-push rewrite to add OPF.
func TestHasOPFApplied(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{"present_lowercase_true", "Checkpoint: a1b2c3d4e5f6\n\nEntire-OPF-Applied: true\n", true},
		{"absent", "Checkpoint: a1b2c3d4e5f6\n", false},
		{"present_among_other_trailers", "msg\n\nEntire-Session: 2026-01\nEntire-OPF-Applied: true\nEntire-Strategy: manual-commit\n", true},
		{"value_false_not_applied", "msg\n\nEntire-OPF-Applied: false\n", false},
		{"value_other_not_applied", "msg\n\nEntire-OPF-Applied: yes\n", false},
		{"empty_message", "", false},
		{"trailer_with_extra_spaces", "msg\n\nEntire-OPF-Applied:   true   \n", true},
		{"body_mention_not_trailer", "msg\n\nThis paragraph mentions a string that looks like metadata.\nEntire-OPF-Applied: true\n\nSigned-off-by: Test User <test@example.com>\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HasOPFApplied(tc.message); got != tc.want {
				t.Errorf("HasOPFApplied(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

// TestAppendOPFAppliedTrailer covers the formatter. Appending to a
// message without a trailer block inserts a blank line; appending to
// one with a trailer block joins directly. Idempotent — appending to
// a message that already has the trailer must not duplicate it.
func TestAppendOPFAppliedTrailer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "no_existing_trailers",
			msg:  "Checkpoint: a1b2c3d4e5f6\n",
			want: "Checkpoint: a1b2c3d4e5f6\n\nEntire-OPF-Applied: true\n",
		},
		{
			name: "existing_trailer_block",
			msg:  "Checkpoint: a1\n\nEntire-Session: s\nEntire-Strategy: manual-commit\n",
			want: "Checkpoint: a1\n\nEntire-Session: s\nEntire-Strategy: manual-commit\nEntire-OPF-Applied: true\n",
		},
		{
			name: "idempotent_when_already_applied",
			msg:  "Checkpoint: a1\n\nEntire-OPF-Applied: true\n",
			want: "Checkpoint: a1\n\nEntire-OPF-Applied: true\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AppendOPFAppliedTrailer(tt.msg)
			if got != tt.want {
				t.Errorf("AppendOPFAppliedTrailer():\n got=%q\nwant=%q", got, tt.want)
			}
		})
	}
}
