package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsSubpath(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   bool
	}{
		// Basic containment
		{name: "child inside parent", parent: "/a/b", child: "/a/b/c", want: true},
		{name: "equal paths", parent: "/a/b", child: "/a/b", want: true},
		{name: "child outside parent", parent: "/a/b", child: "/a/c", want: false},
		{name: "parent prefix but not subpath", parent: "/a/b", child: "/a/bc", want: false},

		// Traversal attacks
		{name: "dot-dot escape", parent: "/a/b", child: "/a/b/../../../etc/passwd", want: false},
		{name: "dot-dot at end", parent: "/a/b", child: "/a/b/..", want: false},
		{name: "dot-dot in middle", parent: "/a/b/c", child: "/a/b/c/../../d", want: false},

		// Relative paths
		{name: "relative child inside", parent: ".entire", child: ".entire/metadata/test", want: true},
		{name: "relative equal", parent: ".entire", child: ".entire", want: true},
		{name: "relative outside", parent: ".entire", child: "src/main.go", want: false},
		{name: "relative prefix not subpath", parent: ".entire", child: ".entirefile", want: false},

		// Edge cases
		{name: "root parent", parent: "/", child: "/anything", want: true},
		{name: "dot current dir", parent: ".", child: "foo/bar", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsSubpath(tt.parent, tt.child)
			if got != tt.want {
				t.Errorf("IsSubpath(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
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
		{name: "tmp path", in: "/tmp/e2e-repo-123/docs/red.md", want: filepath.Join(os.TempDir(), "e2e-repo-123/docs/red.md")},
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

func TestToRelativePath_MSYSAndUnixPaths(t *testing.T) {
	t.Parallel()

	// On Unix, /home/user/... is a valid absolute path so filepath.Rel handles it.
	// On Windows, /home/user/... is not absolute and gets dropped by the Unix filter.
	// These tests verify behavior that is consistent across platforms.
	tests := []struct {
		name    string
		absPath string
		cwd     string
		want    string
	}{
		// Relative paths pass through unchanged on all platforms
		{name: "relative path unchanged", absPath: "docs/red.md", cwd: "/repo", want: "docs/red.md"},

		// MSYS drive paths: /c/ → C:/ conversion happens, then platform-specific handling.
		// On Unix, C:/Users/... is not absolute so it passes through as-is.
		// On Windows, C:/Users/... is absolute so filepath.Rel resolves it.
		// We test the NormalizeMSYSPath conversion separately above.

		// Paths outside cwd return empty
		{name: "outside cwd", absPath: "/other/repo/file.txt", cwd: "/my/repo", want: ""},
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
