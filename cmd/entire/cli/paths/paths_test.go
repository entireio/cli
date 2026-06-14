package paths

import (
	"context"
	"os"
	"os/exec"
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

// TestAbsPath_EntirePrefixDetection locks down the input shapes that AbsPath
// must route through MainWorktreeRoot rather than WorktreeRoot. Look-alikes
// (".entirefile") and parent-escapes (".entire/../etc/passwd") must NOT match —
// they would otherwise be silently rebased onto the main repo even though
// they target a different location.
func TestAbsPath_EntirePrefixDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "bare entire dir", in: ".entire", want: true},
		{name: "settings file", in: ".entire/settings.json", want: true},
		{name: "nested metadata", in: ".entire/metadata/abc/full.jsonl", want: true},
		{name: "tmp dir", in: ".entire/tmp", want: true},
		{name: "messy slashes", in: ".entire//metadata/x", want: true},
		{name: "look-alike prefix", in: ".entirefile", want: false},
		{name: "parent escape", in: ".entire/../etc/passwd", want: false},
		{name: "non-entire", in: "src/main.go", want: false},
		{name: "absolute treated as non-match", in: "/foo/.entire/settings.json", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSubpath(EntireDir, tt.in); got != tt.want {
				t.Errorf("IsSubpath(%q, %q) = %v, want %v", EntireDir, tt.in, got, tt.want)
			}
		})
	}
}

// TestMainWorktreeRoot_LinkedWorktree verifies that when called from inside
// a linked worktree (e.g. Claude Code's agent-managed worktrees under
// .claude/worktrees/<branch>), MainWorktreeRoot returns the *main* repo's
// root, not the linked worktree root. Regression coverage for the case where
// git hooks fired from a linked worktree could not find .entire/settings.json
// and silently bailed.
func TestMainWorktreeRoot_LinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	mainRoot := initSeedRepo(t)
	worktreeDir := filepath.Join(mainRoot, ".claude", "worktrees", "feature-x")
	runGit(t, mainRoot, "worktree", "add", "-b", "feature-x", worktreeDir)

	t.Chdir(worktreeDir)
	ClearWorktreeRootCache()

	got, err := MainWorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("MainWorktreeRoot from linked worktree: %v", err)
	}

	if mustEvalSymlinks(t, got) != mustEvalSymlinks(t, mainRoot) {
		t.Errorf("MainWorktreeRoot = %q, want main repo %q", got, mainRoot)
	}

	// Sanity: ordinary WorktreeRoot from the same cwd points at the linked
	// worktree, not the main repo. This is what makes the bug possible and
	// why MainWorktreeRoot has to exist as a separate anchor.
	wtRoot, err := WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot: %v", err)
	}
	if mustEvalSymlinks(t, wtRoot) == mustEvalSymlinks(t, mainRoot) {
		t.Errorf("WorktreeRoot from linked worktree (%q) should differ from main (%q); test setup may be wrong", wtRoot, mainRoot)
	}
}

// TestAbsPath_EntireAnchoredAtMainWorktree verifies that AbsPath resolves
// .entire/* paths against the main worktree root even when called from inside
// a linked worktree, while non-entire paths still resolve against the current
// worktree. This is the load-bearing assertion for the hook fix: without it,
// a hook firing from a Claude-managed worktree would fail the IsSetUp check
// (.entire/ does not exist in the worktree) and silently bail.
func TestAbsPath_EntireAnchoredAtMainWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	mainRoot := initSeedRepo(t)
	worktreeDir := filepath.Join(mainRoot, ".claude", "worktrees", "feature-x")
	runGit(t, mainRoot, "worktree", "add", "-b", "feature-x", worktreeDir)

	t.Chdir(worktreeDir)
	ClearWorktreeRootCache()

	ctx := context.Background()

	// Pre-create the targets so we can compare via EvalSymlinks (TempDir on
	// macOS lives under /private/var/... but is reported as /var/...).
	if err := os.MkdirAll(filepath.Join(mainRoot, ".entire"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktreeDir, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	entireAbs, err := AbsPath(ctx, ".entire/settings.json")
	if err != nil {
		t.Fatalf("AbsPath(.entire/settings.json): %v", err)
	}
	wantEntireDir := mustEvalSymlinks(t, filepath.Join(mainRoot, ".entire"))
	if got := mustEvalSymlinks(t, filepath.Dir(entireAbs)); got != wantEntireDir {
		t.Errorf("AbsPath(.entire/settings.json) parent = %q, want %q", got, wantEntireDir)
	}

	// Non-entire relative paths must still anchor at the current worktree.
	srcAbs, err := AbsPath(ctx, "src/main.go")
	if err != nil {
		t.Fatalf("AbsPath(src/main.go): %v", err)
	}
	wantSrcDir := mustEvalSymlinks(t, filepath.Join(worktreeDir, "src"))
	if got := mustEvalSymlinks(t, filepath.Dir(srcAbs)); got != wantSrcDir {
		t.Errorf("AbsPath(src/main.go) parent = %q, want %q", got, wantSrcDir)
	}

	// Absolute inputs are returned unchanged regardless of prefix.
	absIn := filepath.Join(mainRoot, ".entire", "x")
	absOut, err := AbsPath(ctx, absIn)
	if err != nil {
		t.Fatalf("AbsPath(absolute): %v", err)
	}
	if absOut != absIn {
		t.Errorf("AbsPath(%q) = %q, want unchanged", absIn, absOut)
	}
}

func initSeedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main", ".")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "commit.gpgsign", "false")
	// At least one commit is required before `git worktree add -b` will succeed.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-q", "-m", "seed")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}
