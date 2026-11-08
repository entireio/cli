package opencode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// useHome points os.UserHomeDir at home for one test, so the install
// directory fallback never finds the developer's own OpenCode.
func useHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
}

// writeOpenCodeStub writes a shell-script opencode into dir.
func writeOpenCodeStub(t *testing.T, dir, script string, perm os.FileMode) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), perm); err != nil {
		t.Fatal(err)
	}
	return bin
}

// installOpenCode puts a stub opencode where OpenCode's install script puts
// the binary, under a fresh home, and leaves PATH without any opencode, as in
// a hook run by an OpenCode that was not started from the user's shell. The
// stub may use shell builtins only.
func installOpenCode(t *testing.T, script string) string {
	t.Helper()
	home := t.TempDir()
	bin := writeOpenCodeStub(t, filepath.Join(home, ".opencode", "bin"), script, 0o700)
	useHome(t, home)
	t.Setenv("PATH", t.TempDir())
	return bin
}

func TestOpenCodeExecutable(t *testing.T) {
	// No t.Parallel: t.Setenv.
	t.Run("installed, not on PATH", func(t *testing.T) {
		bin := installOpenCode(t, "")
		if got := openCodeExecutable(); got != bin {
			t.Fatalf("openCodeExecutable() = %q, want the installed %q", got, bin)
		}
	})
	t.Run("PATH wins over the install directory", func(t *testing.T) {
		installOpenCode(t, "")
		pathDir := t.TempDir()
		onPath := writeOpenCodeStub(t, pathDir, "", 0o700)
		t.Setenv("PATH", pathDir)
		if got := openCodeExecutable(); got != onPath {
			t.Fatalf("openCodeExecutable() = %q, want the PATH binary %q", got, onPath)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		useHome(t, t.TempDir())
		t.Setenv("PATH", t.TempDir())
		if got := openCodeExecutable(); got != openCodeBinary {
			t.Fatalf("openCodeExecutable() = %q, want the bare %q", got, openCodeBinary)
		}
	})
	t.Run("installed file not executable", func(t *testing.T) {
		home := t.TempDir()
		writeOpenCodeStub(t, filepath.Join(home, ".opencode", "bin"), "", 0o600)
		useHome(t, home)
		t.Setenv("PATH", t.TempDir())
		if got := openCodeExecutable(); got != openCodeBinary {
			t.Fatalf("openCodeExecutable() = %q, want the bare %q", got, openCodeBinary)
		}
	})
	t.Run("relative home never runs a repository file", func(t *testing.T) {
		repo := t.TempDir()
		writeOpenCodeStub(t, filepath.Join(repo, ".opencode", "bin"), "", 0o700)
		t.Chdir(repo)
		useHome(t, ".")
		t.Setenv("PATH", t.TempDir())
		if got := openCodeExecutable(); got != openCodeBinary {
			t.Fatalf("openCodeExecutable() = %q, want the bare %q", got, openCodeBinary)
		}
	})
}
