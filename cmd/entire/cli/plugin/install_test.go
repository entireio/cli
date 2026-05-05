package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
)

func TestInstallLocal_Symlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("local install uses symlinks; Windows path differs")
	}

	root := t.TempDir()
	m := &Manager{Root: root}

	src := filepath.Join(t.TempDir(), "entire-foo")
	mustMkdir(t, src)
	mustWriteExec(t, filepath.Join(src, "entire-foo"), "#!/bin/sh\nexit 0\n")

	p, err := m.InstallLocal(InstallLocalOptions{SourceDir: src})
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if p.Kind != KindLocal {
		t.Errorf("kind = %v; want local", p.Kind)
	}
	if p.Name != "foo" {
		t.Errorf("name = %q; want foo", p.Name)
	}

	// Re-installing without --force fails.
	if _, err := m.InstallLocal(InstallLocalOptions{SourceDir: src}); err == nil {
		t.Errorf("expected error on re-install without --force")
	}

	// With --force, succeeds.
	if _, err := m.InstallLocal(InstallLocalOptions{SourceDir: src, Force: true}); err != nil {
		t.Errorf("InstallLocal --force: %v", err)
	}
}

func TestInstallLocal_RejectsBuiltinName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("symlink path")
	}

	root := t.TempDir()
	m := &Manager{Root: root}

	rootCmd := &cobra.Command{Use: "entire"}
	rootCmd.AddCommand(&cobra.Command{Use: "status"})

	src := filepath.Join(t.TempDir(), "entire-status")
	mustMkdir(t, src)
	mustWriteExec(t, filepath.Join(src, "entire-status"), "#!/bin/sh\nexit 0\n")

	_, err := m.InstallLocal(InstallLocalOptions{SourceDir: src, RootCmd: rootCmd})
	if err == nil {
		t.Fatalf("expected conflict error for built-in name")
	}
}

func TestInstallLocal_RejectsBadDirName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("symlink path")
	}

	root := t.TempDir()
	m := &Manager{Root: root}

	src := filepath.Join(t.TempDir(), "not-prefixed")
	mustMkdir(t, src)
	mustWriteExec(t, filepath.Join(src, "not-prefixed"), "#!/bin/sh\nexit 0\n")

	if _, err := m.InstallLocal(InstallLocalOptions{SourceDir: src}); err == nil {
		t.Errorf("expected error for non-prefixed dir name")
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("symlink path")
	}

	root := t.TempDir()
	m := &Manager{Root: root}

	src := filepath.Join(t.TempDir(), "entire-foo")
	mustMkdir(t, src)
	mustWriteExec(t, filepath.Join(src, "entire-foo"), "#!/bin/sh\nexit 0\n")

	if _, err := m.InstallLocal(InstallLocalOptions{SourceDir: src}); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := m.Remove("foo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := m.Remove("foo"); err == nil {
		t.Errorf("expected error removing already-removed plugin")
	}

	// Source must still exist after removing the symlink.
	if _, err := os.ReadDir(src); err != nil {
		t.Errorf("source dir was disturbed by Remove: %v", err)
	}
}
