package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Note: t.Parallel is incompatible with t.Chdir.

func TestInstallHooks_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	count, err := (&PiAgent{}).InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("extension not written: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, `const ENTIRE_CMD = "entire"`) {
		t.Error("production ENTIRE_CMD missing")
	}
	if !strings.Contains(body, "hooks pi ") {
		t.Error("missing call to `entire hooks pi`")
	}
	if !strings.Contains(body, entireMarker) {
		t.Error("entireMarker missing")
	}
	if strings.Contains(body, "go run") {
		t.Error("production extension should not contain 'go run'")
	}
}

func TestInstallHooks_LocalDev(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := (&PiAgent{}).InstallHooks(context.Background(), true, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".pi", "extensions", "entire", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"$(git rev-parse --show-toplevel)"/scripts/entire-dev`) {
		t.Error("local-dev extension should delegate to the entire-dev launcher via git rev-parse")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}

	c1, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 1 {
		t.Errorf("first install count = %d", c1)
	}
	c2, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c2 != 0 {
		t.Errorf("second install (idempotent) count = %d", c2)
	}
}

func TestInstallHooks_RewritesOnModeChange(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	c, err := a.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if c != 1 {
		t.Errorf("expected rewrite on mode change, got %d", c)
	}
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !a.AreHooksInstalled(context.Background()) {
		t.Fatal("AreHooksInstalled should be true after install")
	}
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	if a.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled should be false after uninstall")
	}
	// Idempotent uninstall.
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestAreHooksInstalled_RejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("// user's own extension\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if (&PiAgent{}).AreHooksInstalled(context.Background()) {
		t.Error("should not claim a non-Entire file")
	}
}

func TestInstallHooks_RefusesForeignFileWithoutForce(t *testing.T) {
	// User has their own extension at the same path. Without --force we must
	// not clobber it. With --force we replace it.
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("// user's own extension\nconsole.log('mine');\n")
	if err := os.WriteFile(path, userContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Without force: should refuse, leave file untouched.
	_, err := (&PiAgent{}).InstallHooks(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error when foreign file exists and force=false")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(userContent) {
		t.Errorf("foreign file was modified: %q", got)
	}

	// With force: should overwrite.
	if _, err := (&PiAgent{}).InstallHooks(context.Background(), false, true); err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), entireMarker) {
		t.Error("force install should write Entire-owned file")
	}
}
