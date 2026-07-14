package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// legacyFallbackTranscriptPath reads its metadata-dir argument from the
// attacker-influenceable Entire-Metadata commit trailer and feeds the result to
// an unrooted os.ReadFile. A crafted trailer must not be able to redirect that
// read anywhere other than Entire-owned metadata under .entire/metadata/, so
// anything outside that subtree (traversal, absolute/volume paths, and in-repo
// or CWD-relative dirs like "notes" or ".") must fail closed (return "").
func TestLegacyFallbackTranscriptPath(t *testing.T) {
	t.Parallel()

	legit := paths.EntireMetadataDir + "/sess-123"
	legitTask := paths.EntireMetadataDir + "/sess-123/tasks/toolu_abc"

	tests := []struct {
		name        string
		metadataDir string
		want        string
	}{
		{
			name:        "valid session metadata dir",
			metadataDir: legit,
			want:        filepath.Join(legit, paths.TranscriptFileNameLegacy),
		},
		{
			name:        "valid task metadata dir",
			metadataDir: legitTask,
			want:        filepath.Join(legitTask, paths.TranscriptFileNameLegacy),
		},
		{
			name:        "empty fails closed",
			metadataDir: "",
			want:        "",
		},
		{
			name:        "leading parent traversal fails closed",
			metadataDir: "../../../etc/passwd",
			want:        "",
		},
		{
			name:        "embedded traversal escaping the base fails closed",
			metadataDir: paths.EntireMetadataDir + "/../../../../etc/passwd",
			want:        "",
		},
		{
			name:        "bare dot-dot fails closed",
			metadataDir: "..",
			want:        "",
		},
		{
			name:        "absolute path fails closed",
			metadataDir: "/etc/passwd",
			want:        "",
		},
		{
			name:        "in-repo dir outside .entire/metadata fails closed",
			metadataDir: "notes",
			want:        "",
		},
		{
			name:        "current dir fails closed",
			metadataDir: ".",
			want:        "",
		},
		{
			name:        "the metadata root itself (not a session dir) fails closed",
			metadataDir: ".entire",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := legacyFallbackTranscriptPath(tt.metadataDir); got != tt.want {
				t.Errorf("legacyFallbackTranscriptPath(%q) = %q, want %q", tt.metadataDir, got, tt.want)
			}
		})
	}
}

func TestRewindCmd_IsDeprecated(t *testing.T) {
	t.Parallel()

	cmd := newRewindCmd()
	if cmd.Deprecated == "" {
		t.Error("rewind command should have Deprecated field set")
	}
	if !strings.Contains(cmd.Deprecated, "removed") {
		t.Errorf("Deprecated message should announce removal, got: %s", cmd.Deprecated)
	}
}
