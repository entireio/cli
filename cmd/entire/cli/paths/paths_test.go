package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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

func TestWorktreeRoot_ResolvesSymlinks(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.

	// Create a real directory and a symlink to it
	realDir := t.TempDir()
	symlinkDir := filepath.Join(t.TempDir(), "symlink-repo")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}

	// Initialize a git repo in the real directory
	ctx := t.Context()
	cmd := exec.CommandContext(ctx, "git", "init", realDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "-C", realDir, "config", "user.email", "test@test.com")
	if err := cmd.Run(); err != nil {
		t.Fatalf("git config failed: %v", err)
	}
	cmd = exec.CommandContext(ctx, "git", "-C", realDir, "config", "user.name", "Test")
	if err := cmd.Run(); err != nil {
		t.Fatalf("git config failed: %v", err)
	}

	// cd into the symlink path and call WorktreeRoot
	t.Chdir(symlinkDir)

	ClearWorktreeRootCache()
	root, err := WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}

	// Resolve the real dir for comparison (handles /tmp -> /private/tmp on macOS)
	resolvedReal, evalErr := filepath.EvalSymlinks(realDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", realDir, evalErr)
	}

	if root != resolvedReal {
		t.Errorf("WorktreeRoot() via symlink = %q, want resolved real path %q", root, resolvedReal)
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
