package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeCLIStub writes an empty shell-script binary into dir.
func writeCLIStub(t *testing.T, dir, binary string, perm os.FileMode) string {
	t.Helper()
	if runtime.GOOS == windowsOS {
		t.Skip("stub is a shell script")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// useHomeAndPath points os.UserHomeDir at home and PATH at pathDir for one
// test, so the developer's own CLIs are never found.
func useHomeAndPath(t *testing.T, home, pathDir string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("PATH", pathDir)
}

func TestSummaryCLIPath(t *testing.T) {
	// No t.Parallel: t.Setenv.
	installDir := func(home string) string { return filepath.Join(home, ".opencode", "bin") }
	check := func(t *testing.T, name, want string) {
		t.Helper()
		got, ok := SummaryCLIPath(AgentNameOpenCode)
		if got != want || ok != (want != "") {
			t.Fatalf("SummaryCLIPath = %q, %v; want %q (%s)", got, ok, want, name)
		}
		if IsSummaryCLIAvailable(AgentNameOpenCode) != ok {
			t.Fatalf("IsSummaryCLIAvailable disagrees with SummaryCLIPath (%s)", name)
		}
	}

	t.Run("installed, not on PATH", func(t *testing.T) {
		home := t.TempDir()
		installed := writeCLIStub(t, installDir(home), openCodeBinary, 0o700)
		useHomeAndPath(t, home, t.TempDir())
		check(t, "the installed binary", installed)
	})
	t.Run("PATH wins over the install directory", func(t *testing.T) {
		home, pathDir := t.TempDir(), t.TempDir()
		writeCLIStub(t, installDir(home), openCodeBinary, 0o700)
		onPath := writeCLIStub(t, pathDir, openCodeBinary, 0o700)
		useHomeAndPath(t, home, pathDir)
		check(t, "the PATH binary", onPath)
	})
	t.Run("not installed", func(t *testing.T) {
		useHomeAndPath(t, t.TempDir(), t.TempDir())
		check(t, "nothing", "")
	})
	t.Run("installed file not executable", func(t *testing.T) {
		home := t.TempDir()
		writeCLIStub(t, installDir(home), openCodeBinary, 0o600)
		useHomeAndPath(t, home, t.TempDir())
		check(t, "nothing", "")
	})
	t.Run("relative home never runs a repository file", func(t *testing.T) {
		repo := t.TempDir()
		writeCLIStub(t, installDir(repo), openCodeBinary, 0o700)
		t.Chdir(repo)
		useHomeAndPath(t, ".", t.TempDir())
		check(t, "nothing", "")
	})
	t.Run("agents without an install directory use PATH only", func(t *testing.T) {
		home := t.TempDir()
		writeCLIStub(t, filepath.Join(home, ".opencode", "bin"), "claude", 0o700)
		useHomeAndPath(t, home, t.TempDir())
		if path, ok := SummaryCLIPath(AgentNameClaudeCode); ok {
			t.Fatalf("SummaryCLIPath(claude-code) = %q off PATH, want not found", path)
		}
	})
}
