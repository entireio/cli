package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create .cursor so DetectPresence can find it; InstallHooks will create .cursor/hooks.json
	if err := os.MkdirAll(filepath.Join(tempDir, ".cursor"), 0o750); err != nil {
		t.Fatal(err)
	}

	c := &CursorAgent{}
	count, err := c.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 7 {
		t.Errorf("InstallHooks() count = %d, want 7", count)
	}

	data, err := os.ReadFile(filepath.Join(tempDir, ".cursor", CursorHooksFileName))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var file CursorHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	if file.Version != 1 {
		t.Errorf("version = %d, want 1", file.Version)
	}
	if len(file.Hooks.SessionStart) != 1 || len(file.Hooks.Stop) != 1 {
		t.Errorf("SessionStart=%d Stop=%d, want 1 each", len(file.Hooks.SessionStart), len(file.Hooks.Stop))
	}
}

func TestAreHooksInstalled_NotInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	c := &CursorAgent{}
	if c.AreHooksInstalled() {
		t.Error("AreHooksInstalled() = true, want false when no hooks")
	}
}

func TestAreHooksInstalled_AfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	if err := os.MkdirAll(filepath.Join(tempDir, ".cursor"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := &CursorAgent{}
	_, err := c.InstallHooks(false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AreHooksInstalled() {
		t.Error("AreHooksInstalled() = false, want true after install")
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	if err := os.MkdirAll(filepath.Join(tempDir, ".cursor"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := &CursorAgent{}
	_, _ = c.InstallHooks(false, false)
	if err := c.UninstallHooks(); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}
	if c.AreHooksInstalled() {
		t.Error("AreHooksInstalled() = true after uninstall, want false")
	}
}
