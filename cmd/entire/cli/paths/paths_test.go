package paths

import (
	"path/filepath"
	"runtime"
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
		{name: "dot-dot prefixed child inside parent", parent: "/a/b", child: "/a/b/..generated/schema.json", want: true},

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

func TestIsRelativeTraversal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{name: "exact dot-dot", rel: "..", want: true},
		{name: "dot-dot child", rel: filepath.Join("..", "outside.txt"), want: true},
		{name: "slash dot-dot child", rel: "../outside.txt", want: true},
		{name: "backslash dot-dot child", rel: `..\outside.txt`, want: true},
		{name: "dot-dot prefixed name", rel: filepath.Join("..generated", "schema.json"), want: false},
		{name: "slash dot-dot prefixed name", rel: "../generated/schema.json", want: true},
		{name: "backslash dot-dot prefixed name", rel: `..\generated\schema.json`, want: true},
		{name: "ordinary child", rel: filepath.Join("dir", "file.txt"), want: false},
		{name: "current dir", rel: ".", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsRelativeTraversal(tt.rel)
			if got != tt.want {
				t.Errorf("IsRelativeTraversal(%q) = %v, want %v", tt.rel, got, tt.want)
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

func TestToRelativePath_MSYSPaths(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("MSYS path handling is Windows-only")
	}
	tests := []struct {
		name    string
		absPath string
		cwd     string
		want    string
	}{
		{
			name:    "msys with drive letter",
			absPath: "/c/Users/test/repo/docs/red.md",
			cwd:     "C:/Users/test/repo",
			want:    "docs\\red.md",
		},
		{
			name:    "msys without drive letter",
			absPath: "/Users/test/repo/docs/red.md",
			cwd:     "C:/Users/test/repo",
			want:    "docs\\red.md",
		},
		{
			name:    "msys without drive letter different cwd drive",
			absPath: "/Users/test/repo/docs/red.md",
			cwd:     "D:/Users/test/repo",
			want:    "docs\\red.md",
		},
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

func TestToRelativePath_AllowsDotDotPrefixedRepoPath(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	absPath := filepath.Join(cwd, "..generated", "schema.json")
	want := filepath.Join("..generated", "schema.json")

	got := ToRelativePath(absPath, cwd)
	if got != want {
		t.Errorf("ToRelativePath(%q, %q) = %q, want %q", absPath, cwd, got, want)
	}
}

func TestToRelativePath_RejectsDotDotTraversal(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	absPath := filepath.Join(filepath.Dir(cwd), "..generated", "schema.json")

	got := ToRelativePath(absPath, cwd)
	if got != "" {
		t.Errorf("ToRelativePath(%q, %q) = %q, want empty string", absPath, cwd, got)
	}
}

func TestNormalizeMSYSPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "msys drive c", path: "/c/Users/test/repo", want: "C:/Users/test/repo"},
		{name: "msys drive d", path: "/d/work/project", want: "D:/work/project"},
		{name: "already windows", path: "C:/Users/test/repo", want: "C:/Users/test/repo"},
		{name: "unix absolute", path: "/home/user/repo", want: "/home/user/repo"},
		{name: "relative path", path: "docs/red.md", want: "docs/red.md"},
		{name: "root slash only", path: "/", want: "/"},
		{name: "short path", path: "/c", want: "/c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeMSYSPath(tt.path)
			// On non-Windows, normalizeMSYSPath is a no-op
			if runtime.GOOS == "windows" {
				if got != tt.want {
					t.Errorf("normalizeMSYSPath(%q) = %q, want %q", tt.path, got, tt.want)
				}
			} else {
				if got != tt.path {
					t.Errorf("normalizeMSYSPath(%q) should be no-op on %s, got %q", tt.path, runtime.GOOS, got)
				}
			}
		})
	}
}
