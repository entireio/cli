//go:build windows

package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeMSYSPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Drive letter paths
		{name: "lowercase drive", in: "/c/Users/Victor/repo", want: "C:/Users/Victor/repo"},
		{name: "uppercase drive", in: "/D/Projects/app", want: "D:/Projects/app"},
		{name: "drive root", in: "/c/", want: "C:/"},

		// /tmp mapping
		{name: "tmp path", in: "/tmp/e2e-repo-123/docs/red.md", want: filepath.Join(os.TempDir(), "e2e-repo-123", "docs", "red.md")},
		{name: "tmp root file", in: "/tmp/file.txt", want: filepath.Join(os.TempDir(), "file.txt")},

		// Paths that should NOT be converted
		{name: "already relative", in: "docs/red.md", want: "docs/red.md"},
		{name: "windows absolute", in: "C:/Users/Victor/repo", want: "C:/Users/Victor/repo"},
		{name: "unix absolute non-drive", in: "/home/user/docs/red.md", want: "/home/user/docs/red.md"},
		{name: "empty string", in: "", want: ""},
		{name: "single slash", in: "/", want: "/"},
		{name: "tmp exact no trailing slash", in: "/tmp", want: "/tmp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeMSYSPath(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeMSYSPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToRelativePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		absPath string
		cwd     string
		want    string
	}{
		// Standard relative conversion
		{name: "child of cwd", absPath: "C:/repo/docs/red.md", cwd: "C:/repo", want: "docs/red.md"},
		{name: "exact cwd", absPath: "C:/repo", cwd: "C:/repo", want: "."},
		{name: "outside cwd", absPath: "C:/other/file.txt", cwd: "C:/repo", want: ""},

		// Already relative — pass through
		{name: "relative unchanged", absPath: "docs/red.md", cwd: "C:/repo", want: "docs/red.md"},

		// MSYS drive paths get normalized and resolved
		{name: "msys drive path", absPath: "/c/repo/docs/red.md", cwd: "C:/repo", want: "docs/red.md"},

		// Container/sandbox paths are dropped (can't resolve on Windows)
		{name: "container path dropped", absPath: "/home/user/docs/red.md", cwd: "C:/repo", want: ""},
		{name: "unix opt path dropped", absPath: "/opt/app/file.txt", cwd: "C:/repo", want: ""},
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
