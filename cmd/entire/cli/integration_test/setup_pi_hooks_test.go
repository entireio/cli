//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupPiHooks_ScaffoldsManagedExtension(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	output, err := env.RunCLIWithError("enable", "--agent", "pi")
	if err != nil {
		t.Fatalf("enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if _, err := os.Stat(extPath); err != nil {
		t.Fatalf("expected managed Pi extension scaffold at %s: %v", extPath, err)
	}

	if _, err := os.Stat(filepath.Join(env.RepoDir, ".pi", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .pi/settings.json mutation during enable")
	}
}

func TestSetupPiHooks_ScaffoldContainsAllRequiredHookCommands(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	output, err := env.RunCLIWithError("enable", "--agent", "pi")
	if err != nil {
		t.Fatalf("enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	data, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatalf("failed to read managed extension scaffold: %v", err)
	}
	content := string(data)

	requiredCommands := []string{
		"entire hooks pi session-start",
		"entire hooks pi user-prompt-submit",
		"entire hooks pi before-tool",
		"entire hooks pi after-tool",
		"entire hooks pi stop",
		"entire hooks pi session-end",
	}
	for _, command := range requiredCommands {
		if !strings.Contains(content, command) {
			t.Fatalf("managed scaffold missing required hook command %q", command)
		}
	}
}

func TestSetupPiHooks_EnableIsIdempotent(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	if output, err := env.RunCLIWithError("enable", "--agent", "pi"); err != nil {
		t.Fatalf("first enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	output, err := env.RunCLIWithError("enable", "--agent", "pi")
	if err != nil {
		t.Fatalf("second enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	if !strings.Contains(output, "already installed") {
		t.Fatalf("expected idempotent message on second enable, got: %s", output)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if _, err := os.Stat(extPath); err != nil {
		t.Fatalf("expected managed Pi extension scaffold at %s: %v", extPath, err)
	}

	if _, err := os.Stat(filepath.Join(env.RepoDir, ".pi", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .pi/settings.json mutation during repeated enable")
	}
}

func TestSetupPiHooks_PreservesExistingUserExtension(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(extPath), 0o755); err != nil {
		t.Fatalf("failed to create extension directory: %v", err)
	}
	customContent := "// custom user extension\nexport default function() {}\n"
	if err := os.WriteFile(extPath, []byte(customContent), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	output, err := env.RunCLIWithError("enable", "--agent", "pi")
	if err != nil {
		t.Fatalf("enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	data, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatalf("failed to read extension file: %v", err)
	}
	if string(data) != customContent {
		t.Fatalf("custom user extension should be preserved on enable without --force")
	}

	if _, err := os.Stat(filepath.Join(env.RepoDir, ".pi", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .pi/settings.json mutation during enable with user extension")
	}
}

func TestSetupPiHooks_ForceOverwritesExistingUserExtension(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(extPath), 0o755); err != nil {
		t.Fatalf("failed to create extension directory: %v", err)
	}
	if err := os.WriteFile(extPath, []byte("// custom user extension\n"), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	output, err := env.RunCLIWithError("enable", "--agent", "pi", "--force")
	if err != nil {
		t.Fatalf("enable --agent pi --force failed: %v\nOutput: %s", err, output)
	}

	data, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatalf("failed to read extension file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "entire-managed: pi-extension-v1") {
		t.Fatalf("force enable should replace with managed scaffold")
	}

	if _, err := os.Stat(filepath.Join(env.RepoDir, ".pi", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .pi/settings.json mutation during force enable")
	}
}

func TestSetupPiHooks_DisableUninstallRemovesManagedExtension(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	if output, err := env.RunCLIWithError("enable", "--agent", "pi"); err != nil {
		t.Fatalf("enable --agent pi failed: %v\nOutput: %s", err, output)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if _, err := os.Stat(extPath); err != nil {
		t.Fatalf("expected managed Pi extension scaffold at %s: %v", extPath, err)
	}

	output, err := env.RunCLIWithError("disable", "--uninstall", "--force")
	if err != nil {
		t.Fatalf("disable --uninstall --force failed: %v\nOutput: %s", err, output)
	}
	if !strings.Contains(output, "uninstalled successfully") {
		t.Fatalf("expected uninstall success message, got: %s", output)
	}

	if _, err := os.Stat(extPath); !os.IsNotExist(err) {
		t.Fatalf("expected managed Pi extension scaffold to be removed after uninstall")
	}
}

func TestSetupPiHooks_DisableUninstallPreservesUnmanagedExtension(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(extPath), 0o755); err != nil {
		t.Fatalf("failed to create extension directory: %v", err)
	}

	customContent := "// custom user extension\nexport default function() {}\n"
	if err := os.WriteFile(extPath, []byte(customContent), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	output, err := env.RunCLIWithError("disable", "--uninstall", "--force")
	if err != nil {
		t.Fatalf("disable --uninstall --force failed: %v\nOutput: %s", err, output)
	}
	if !strings.Contains(output, "uninstalled successfully") {
		t.Fatalf("expected uninstall success message, got: %s", output)
	}

	data, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatalf("expected unmanaged extension to be preserved, read error: %v", err)
	}
	if string(data) != customContent {
		t.Fatalf("unmanaged Pi extension content changed during uninstall")
	}
}
