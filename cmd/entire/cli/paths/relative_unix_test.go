//go:build unix

package paths

import (
	"testing"
)

func TestToRelativePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		absPath string
		cwd     string
		want    string
	}{
		// Standard relative conversion
		{name: "child of cwd", absPath: "/repo/docs/red.md", cwd: "/repo", want: "docs/red.md"},
		{name: "exact cwd", absPath: "/repo", cwd: "/repo", want: "."},
		{name: "outside cwd", absPath: "/other/file.txt", cwd: "/repo", want: ""},

		// Already relative — pass through
		{name: "relative unchanged", absPath: "docs/red.md", cwd: "/repo", want: "docs/red.md"},

		// Unix paths that are valid on this platform
		{name: "tmp is valid unix path", absPath: "/tmp/repo/file.txt", cwd: "/tmp/repo", want: "file.txt"},
		{name: "home is valid unix path", absPath: "/home/user/repo/f.go", cwd: "/home/user/repo", want: "f.go"},
		{name: "slash-c is valid unix path", absPath: "/c/Users/repo/f.go", cwd: "/c/Users/repo", want: "f.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ToRelativePath(tt.absPath, tt.cwd)
			if got != tt.want {
				t.Errorf("ToRelativePath(%q, %q) = %q, want %q", tt.absPath, tt.cwd, got, tt.want)
			}
		})
	}
}
