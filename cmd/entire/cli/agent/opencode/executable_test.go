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

// installOpenCode puts a shell-script opencode where OpenCode's install script
// puts the binary, under a fresh home, and leaves PATH without any opencode,
// as in a hook run by an OpenCode that was not started from the user's shell.
// The script may use shell builtins only.
func installOpenCode(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	useHome(t, home)
	t.Setenv("PATH", t.TempDir())
}
