package trailers

import (
	"slices"
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
		{
			name: "indented pseudo-trailer paragraph starts a new block",
			msg:  "Message\n\nBody.\n\n  Entire-Checkpoint: deadbeefcafe\n",
			want: "Message\n\nBody.\n\n  Entire-Checkpoint: deadbeefcafe\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "tab-indented pseudo-trailer paragraph starts a new block",
			msg:  "Message\n\nBody.\n\n\tEntire-Checkpoint: deadbeefcafe\n",
			want: "Message\n\nBody.\n\n\tEntire-Checkpoint: deadbeefcafe\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "trailer block ending in a continuation line is joined",
			msg:  "Message\n\nSigned-off-by: Test User <test@example.com>\n continuation text\n",
			want: "Message\n\nSigned-off-by: Test User <test@example.com>\n continuation text\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "checkpoint-shaped body line starts a new block",
			msg:  "Message\n\nEntire-Checkpoint: deadbeefcafe\n\nBody continues here.\n",
			want: "Message\n\nEntire-Checkpoint: deadbeefcafe\n\nBody continues here.\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			// git reads a block holding a line it generates as trailers even
			// with other lines in it; a new paragraph would push them out.
			name: "mixed block with a git-generated line is joined",
			msg:  "Message\n\nSigned-off-by: Test User <test@example.com>\nReviewed on the call\n",
			want: "Message\n\nSigned-off-by: Test User <test@example.com>\nReviewed on the call\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "hash line in a mixed block is joined",
			msg:  "Message\n\nSigned-off-by: Test User <test@example.com>\n# retained message text\n",
			want: "Message\n\nSigned-off-by: Test User <test@example.com>\n# retained message text\nEntire-Checkpoint: abc123def456\n",
		},
		{
			// Read without a comment prefix the hash line is prose, which only
			// a block holding a git-generated line may contain.
			name: "hash line in a block without a git-generated line starts a new block",
			msg:  "Message\n\nReviewed-by: Test User <test@example.com>\n# retained message text\n",
			want: "Message\n\nReviewed-by: Test User <test@example.com>\n# retained message text\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "trailer without a space after the colon is joined",
			msg:  "Message\n\nEntire-Checkpoint:deadbeefcafe\n",
			want: "Message\n\nEntire-Checkpoint:deadbeefcafe\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "trailer-shaped subject starts a new block",
			msg:  "Entire-Checkpoint: deadbeefcafe\n",
			want: "Entire-Checkpoint: deadbeefcafe\n\nEntire-Checkpoint: abc123def456\n",
		},
		{
			name: "cherry-pick note block is joined",
			msg:  "Message\n\nEntire-Checkpoint: deadbeefcafe\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n",
			want: "Message\n\nEntire-Checkpoint: deadbeefcafe\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\nEntire-Checkpoint: abc123def456\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AppendCheckpointTrailer(tt.msg, "abc123def456")
			if got != tt.want {
				t.Errorf("AppendCheckpointTrailer() = %q, want %q", got, tt.want)
			}
			for _, commentPrefix := range []string{"", DefaultCommentPrefix} {
				if !slices.Contains(ParseAllCheckpointsFromFinalTrailerBlock(got, commentPrefix), checkpointID.MustCheckpointID("abc123def456")) {
					t.Errorf("ParseAllCheckpointsFromFinalTrailerBlock(%q) does not see the checkpoint AppendCheckpointTrailer just appended:\n%q", commentPrefix, got)
				}
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
		{"  Entire-Checkpoint: abc123def456", false},
		{"\tEntire-Checkpoint: abc123def456", false},
		{"Entire-Checkpoint:abc123def456", true},
		{"Entire-Checkpoint:\tabc123def456", true},
		{"  Entire-Checkpoint:abc123def456", false},
		{"https://example.com/issue/1", true}, // git parses it as key "https" too
		// git's key is letters, digits and dashes, then optional spaces or
		// tabs before the colon.
		{"Reviewed-by : User <user@example.com>", true},
		{"Reviewed-by\t: User <user@example.com>", true},
		{"1st-reviewer: User", true},
		{"-x: y", true},
		{"Reviewed by: User", false},
		{": value", false},
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
	t.Parallel()

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
			t.Parallel()

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

func TestParseAllCheckpointsFromFinalTrailerBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		message       string
		commentPrefix string
		want          []string
	}{
		{
			name:    "checkpoint-shaped body line ignored",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\nSigned-off-by: Test User <test@example.com>\n",
			want:    nil,
		},
		{
			name:    "final trailer accepted after checkpoint-shaped body line",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\nBody continues here.\n\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			want:    []string{"b2c3d4e5f6a1"},
		},
		{
			name:    "multiple final checkpoint trailers",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\nEntire-Checkpoint: b2c3d4e5f6a1\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
		},
		{
			name:    "checkpoint key embedded in another trailer value ignored",
			message: "Message\n\nNotes: Entire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "indented checkpoint is not a trailer",
			message: "Message\n\n  Entire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "tab-indented checkpoint is not a trailer",
			message: "Message\n\n\tEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "no space after the colon",
			message: "Message\n\nEntire-Checkpoint:a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "tab after the colon",
			message: "Message\n\nSigned-off-by: Test User <test@example.com>\nEntire-Checkpoint:\ta1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			// git accepts whitespace between a trailer's key and its colon.
			name:    "another trailer spaced before its colon",
			message: "Message\n\nReviewed-by : Test User <test@example.com>\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "continuation after checkpoint remains in trailer block",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n continuation text\nSigned-off-by: Test User <test@example.com>\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "continuation of another trailer does not become checkpoint",
			message: "Message\n\nNotes: details\n  Entire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "subject line is never a trailer block",
			message: "Entire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "trailer-shaped subject paragraph is not a trailer block",
			message: "Entire-Checkpoint: a1b2c3d4e5f6\nSigned-off-by: Test User <test@example.com>\n",
			want:    nil,
		},
		{
			name:    "leading blank line leaves an empty subject",
			message: "\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			// Earlier versions recorded Entire's explanation below the trailer
			// under commit.cleanup=scissors.
			name:          "comment lines in the block are skipped",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# Remove the Entire-Checkpoint trailer above if you want.\n# Last Prompt: add login\n",
			commentPrefix: "#",
			want:          []string{"a1b2c3d4e5f6"},
		},
		{
			name:          "comment lines between trailers are skipped",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# note\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			commentPrefix: "#",
			want:          []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
		},
		{
			// commit.cleanup=whitespace keeps git's own comment block too.
			name:          "trailing comment paragraphs are ignored",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# Please enter the commit message for your changes.\n#\n# On branch main\n",
			commentPrefix: "#",
			want:          []string{"a1b2c3d4e5f6"},
		},
		{
			name:          "an indented line after a comment continues nothing",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# note\n  continuation text\n",
			commentPrefix: "#",
			want:          nil,
		},
		{
			name:          "configured prefix",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n; Remove the Entire-Checkpoint trailer above if you want.\n",
			commentPrefix: ";",
			want:          []string{"a1b2c3d4e5f6"},
		},
		{
			name:          "another prefix's lines are content",
			message:       "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# note\n",
			commentPrefix: ";",
			want:          nil,
		},
		{
			name:          "comment lines do not lift a trailer out of the subject",
			message:       "# note\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			commentPrefix: "#",
			want:          nil,
		},
		{
			// git cherry-pick -x appends this line to the commit's trailer block.
			name:    "cherry-pick note belongs to the block",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "a quarter trailers with a git-generated one is a block",
			message: "Message\n\nSigned-off-by: A <a@example.com>\np1\np2\np3\np4\np5\np6\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "under a quarter trailers is not a block",
			message: "Message\n\nSigned-off-by: A <a@example.com>\np1\np2\np3\np4\np5\np6\np7\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "prose needs a git-generated trailer in the block",
			message: "Message\n\nNotes: A\nsome prose\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "Signed-off-by is git-generated only with its space",
			message: "Message\n\nSigned-off-by:A\nsome prose\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "a continuation is not prose",
			message: "Message\n\nSigned-off-by: A <a@example.com>\n  wrapped value\nsome prose\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    []string{"a1b2c3d4e5f6"},
		},
		{
			name:    "a git-generated block is still never the subject",
			message: "Signed-off-by: A <a@example.com>\nsome prose\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:    nil,
		},
		{
			name:    "empty prefix reads comment-shaped lines as content",
			message: "Message\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# note\n",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseAllCheckpointsFromFinalTrailerBlock(tt.message, tt.commentPrefix)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseAllCheckpointsFromFinalTrailerBlock() = %v, want %v", got, tt.want)
			}
			for i, wantID := range tt.want {
				if got[i].String() != wantID {
					t.Errorf("ParseAllCheckpointsFromFinalTrailerBlock()[%d] = %v, want %v", i, got[i], wantID)
				}
			}
		})
	}
}

// StripEditorComments must leave exactly what git's cleanup records, so the
// final trailer block it exposes is the one the commit will carry.
func TestStripEditorComments(t *testing.T) {
	t.Parallel()

	const scissors = "------------------------ >8 ------------------------"
	tests := []struct {
		name          string
		message       string
		commentPrefix string
		want          string
	}{
		{
			name:          "default prefix drops hash lines",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# Please enter the commit message\n#\n",
			commentPrefix: "#",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n",
		},
		{
			name:          "configured prefix drops its lines and keeps hash lines",
			message:       "# Release notes\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n; Please enter the commit message\n;\n",
			commentPrefix: ";",
			want:          "# Release notes\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n",
		},
		{
			name:          "multi-character prefix",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n// Please enter the commit message\n",
			commentPrefix: "//",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
		},
		{
			name:          "everything from the scissors line down is dropped",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# " + scissors + "\ndiff --git a/f b/f\n+Entire-Checkpoint: b2c3d4e5f6a1\n",
			commentPrefix: "#",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
		},
		{
			name:          "scissors line with the configured prefix",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n; " + scissors + "\ndiff --git a/f b/f\n",
			commentPrefix: ";",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
		},
		{
			// git cuts only at a scissors line ending in "\n", so it keeps the
			// diff below a CRLF one and drops just the comment lines.
			name:          "a CRLF scissors line is not a cut",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# " + scissors + "\r\ndiff --git a/f b/f\n",
			commentPrefix: "#",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\ndiff --git a/f b/f\n",
		},
		{
			name:          "prose mentioning the marker is not a scissors line",
			message:       "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\nExplain " + scissors + " here.\n",
			commentPrefix: "#",
			want:          "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\nExplain " + scissors + " here.\n",
		},
		{
			name:          "another prefix's scissors line is content",
			message:       "Subject\n\n# " + scissors + "\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			commentPrefix: ";",
			want:          "Subject\n\n# " + scissors + "\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
		},
		{
			name:          "empty prefix keeps the message as written",
			message:       "Subject\n\n# not a comment\n",
			commentPrefix: "",
			want:          "Subject\n\n# not a comment\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := StripEditorComments(tt.message, tt.commentPrefix); got != tt.want {
				t.Errorf("StripEditorComments(%q, %q) = %q, want %q", tt.message, tt.commentPrefix, got, tt.want)
			}
		})
	}
}

// A prose line that mentions the scissors marker must neither hide a real
// final trailer below it nor expose a body trailer above it.
func TestStripEditorComments_ProseMarkerKeepsTheFinalBlock(t *testing.T) {
	t.Parallel()

	const prose = "Explain ------------------------ >8 ------------------------ here."
	below := "Subject\n\n" + prose + "\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# Please enter the commit message\n"
	if got := ParseAllCheckpointsFromFinalTrailerBlock(StripEditorComments(below, "#"), "#"); len(got) != 1 || got[0].String() != "a1b2c3d4e5f6" {
		t.Errorf("final trailer below the prose line: got %v, want [a1b2c3d4e5f6]", got)
	}
	above := "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n" + prose + "\n\n# Please enter the commit message\n"
	if got := ParseAllCheckpointsFromFinalTrailerBlock(StripEditorComments(above, "#"), "#"); len(got) != 0 {
		t.Errorf("body trailer above the prose line must stay body text: got %v", got)
	}
}

// With core.commentChar=auto git picks the prefix before it writes its comment
// block, so the prefix is read back from git's own lines.
func TestAutoCommentPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "comment block written with semicolons",
			message: "# Release notes\n\n; Please enter the commit message for your changes.\n;\n; On branch main\n",
			want:    ";",
		},
		{
			name:    "comment block written with hashes",
			message: "\n# Please enter the commit message for your changes.\n#\n",
			want:    "#",
		},
		{
			name:    "scissors line names the prefix above a diff",
			message: "Subject\n\n@ Please enter the commit message\n@ ------------------------ >8 ------------------------\n@ Do not modify or remove the line above.\ndiff --git a/f b/f\n+line\n",
			want:    "@",
		},
		{
			name:    "no comment block falls back to the default",
			message: "Subject\n\nBody text\n",
			want:    "#",
		},
		{
			// git puts no blank line above its block when the message lacks
			// a final newline.
			name:    "comment block right below a message without a final newline",
			message: "Subject\n\n#42 is the issue\n; Please enter the commit message for your changes.\n;\n",
			want:    ";",
		},
		{
			// git writes its block below the message, never as all of it.
			name:    "a message of comment lines alone has no block",
			message: "#42 fix login\n",
			want:    ";",
		},
		{
			// git would have picked "#" for "Fix build".
			name:    "a trailing paragraph git could not have written is the user's",
			message: "Fix build\n\n$ make test passes\n",
			want:    "#",
		},
		{
			name:    "empty message falls back to the default",
			message: "",
			want:    "#",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AutoCommentPrefix(tt.message, true, AutoCountsEveryLine); got != tt.want {
				t.Errorf("AutoCommentPrefix(%q, true) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

// Without a comment block to read back (commit.status=false, a -m message),
// git's own rule decides: the first of "#;@!$%^&|:" that starts no line.
func TestAutoCommentPrefix_WithoutBlockFollowsGitsRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "a trailing ; line is the user's", message: "Release notes\n\n; reviewer note\n", want: "#"},
		{name: "a # line moves git to ;", message: "# Release notes\n\nBody\n", want: ";"},
		{name: "# and ; lines move git to @", message: "# Release notes\n; note\n", want: "@"},
		{name: "# inside a line does not count", message: "Fix issue #42\n", want: "#"},
		{name: "a CR also starts a line", message: "Subject\r\n#42 is the issue\r\n", want: ";"},
		{name: "empty message", message: "", want: "#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AutoCommentPrefix(tt.message, false, AutoCountsEveryLine); got != tt.want {
				t.Errorf("AutoCommentPrefix(%q, false) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

// Git 2.51 and later pick auto's character without looking at the empty and
// "#" lines that end the message, or an old "Conflicts:" block, as when git
// places a sign-off; the first line always counts. Each message is one git
// 2.51 writes or is handed, with the block it would add where mayHaveBlock.
func TestAutoCommentPrefix_SkipsTrailingCommentsSince251(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		message      string
		mayHaveBlock bool
		want         string
	}{
		{
			name:         "block below a trailing # paragraph",
			message:      "Subject\n\n#42 is the issue\n\n# Please enter the commit message for your changes.\n#\n",
			mayHaveBlock: true,
			want:         "#",
		},
		{
			name:         "block right below a trailing # line without a final newline",
			message:      "Subject\n\n#42 is the issue\n# Please enter the commit message for your changes.\n#\n",
			mayHaveBlock: true,
			want:         "#",
		},
		{name: "no block, trailing # paragraph", message: "Subject\n\n#42 is the issue\n", want: "#"},
		{name: "a # paragraph followed by prose counts", message: "Subject\n\n#42 is the issue\n\nBody\n", want: ";"},
		{name: "the first line always counts", message: "# Release notes\n\nBody\n", want: ";"},
		{name: "a message of one # line counts", message: "#42 fix login\n", want: ";"},
		{name: "an old Conflicts block is trailing too", message: "Subject\n\n#42 is the issue\n\nConflicts:\n\tfile\n", want: "#"},
		{name: "nothing below a scissors line counts", message: "Subject\n\n# ------------------------ >8 ------------------------\n#42 below the cut\n", want: "#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AutoCommentPrefix(tt.message, tt.mayHaveBlock, AutoSkipsTrailingComments); got != tt.want {
				t.Errorf("AutoCommentPrefix(%q, %v) = %q, want %q", tt.message, tt.mayHaveBlock, got, tt.want)
			}
		})
	}
}

// TestHasOPFApplied covers the Entire-OPF-Applied trailer reader. The
// trailer marks a v1 commit whose blobs have been redacted by the
// OpenAI Privacy Filter (OPF-applied, 9-layer); commits without it carry
// regex-only (8-layer) content and are eligible for the pre-push rewrite
// to add OPF.
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
		{"indented_trailer_not_applied", "msg\n\n  Entire-OPF-Applied: true\n", false},
		{"trailer_only_message_applied", "Entire-OPF-Applied: true\n", true},
		{"trailer_only_message_without_newline_applied", "Entire-OPF-Applied: true", true},
		{"subject_not_a_trailer_not_applied", "Rewrite blobs\nEntire-OPF-Applied: true\n", false},
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

func TestRemoveCheckpointTrailers(t *testing.T) {
	t.Parallel()
	dead := checkpointID.MustCheckpointID("a1b2c3d4e5f6")
	isDead := func(cpID checkpointID.CheckpointID) bool { return cpID == dead }

	tests := []struct {
		name        string
		msg         string
		want        string
		wantRemoved []checkpointID.CheckpointID
	}{
		{
			name:        "canonical trailer",
			msg:         "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:        "Subject\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "no space after the colon",
			msg:         "Subject\n\nSigned-off-by: A <a@b.c>\nEntire-Checkpoint:a1b2c3d4e5f6\n",
			want:        "Subject\n\nSigned-off-by: A <a@b.c>\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "tab and trailing space",
			msg:         "Subject\n\nEntire-Checkpoint:\ta1b2c3d4e5f6  \nEntire-Checkpoint: b2c3d4e5f6a1\n",
			want:        "Subject\n\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "editor comments below the trailer block",
			msg:         "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# Please enter the commit message\n",
			want:        "Subject\n\n# Please enter the commit message\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name: "mention in the body is not a trailer",
			msg:  "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6 was wrong, see below.\nMore prose here.\n",
			want: "Subject\n\nEntire-Checkpoint: a1b2c3d4e5f6 was wrong, see below.\nMore prose here.\n",
		},
		{
			// git accepts mixed trailer blocks; any whole trailer line goes.
			name:        "paragraph mixing prose and the trailer",
			msg:         "Subject\n\nSome prose\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:        "Subject\n\nSome prose\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "cherry-pick -x note below the trailer",
			msg:         "subject\n\nbody\n\nEntire-Checkpoint: a1b2c3d4e5f6\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n",
			want:        "subject\n\nbody\n\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "non-trailer line in the block",
			msg:         "subject\n\nSigned-off-by: A\nEntire-Checkpoint: a1b2c3d4e5f6\nnot a trailer line\n",
			want:        "subject\n\nSigned-off-by: A\nnot a trailer line\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "token with a space",
			msg:         "subject\n\nBREAKING CHANGE: api\nEntire-Checkpoint: a1b2c3d4e5f6\n",
			want:        "subject\n\nBREAKING CHANGE: api\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "skip-ci marker after the trailer",
			msg:         "subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n[skip ci]\n",
			want:        "subject\n\n[skip ci]\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "custom comment character below the trailer",
			msg:         "subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n; Please enter the commit message\n",
			want:        "subject\n\n; Please enter the commit message\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "no blank lines left behind",
			msg:         "subject\n\nbody\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n",
			want:        "subject\n\nbody\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "indented line from a squash message",
			msg:         "Squashed commit of the following:\n\ncommit 0123\n\n    part one\n\n    Entire-Checkpoint: a1b2c3d4e5f6\n",
			want:        "Squashed commit of the following:\n\ncommit 0123\n\n    part one\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "nothing below a scissors line is touched or trimmed",
			msg:         "subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# ------------------------ >8 ------------------------\n Entire-Checkpoint: a1b2c3d4e5f6\n\n\n",
			want:        "subject\n\n# ------------------------ >8 ------------------------\n Entire-Checkpoint: a1b2c3d4e5f6\n\n\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name:        "blank paragraph gap collapsed",
			msg:         "subject\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n# comment\n",
			want:        "subject\n\n# comment\n",
			wantRemoved: []checkpointID.CheckpointID{dead},
		},
		{
			name: "subject line is never a trailer",
			msg:  "Entire-Checkpoint: a1b2c3d4e5f6\n",
			want: "Entire-Checkpoint: a1b2c3d4e5f6\n",
		},
		{
			name: "live checkpoint kept",
			msg:  "Subject\n\nEntire-Checkpoint: b2c3d4e5f6a1\n",
			want: "Subject\n\nEntire-Checkpoint: b2c3d4e5f6a1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, removed := RemoveCheckpointTrailers(tt.msg, isDead)
			if got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}
			if len(removed) != len(tt.wantRemoved) {
				t.Fatalf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			for i := range removed {
				if removed[i] != tt.wantRemoved[i] {
					t.Errorf("removed[%d] = %v, want %v", i, removed[i], tt.wantRemoved[i])
				}
			}
		})
	}
}
