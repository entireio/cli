package strategy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectHookManagers_None(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	managers := detectHookManagers(tmpDir)
	if len(managers) != 0 {
		t.Errorf("expected 0 managers, got %d", len(managers))
	}
}

func TestDetectHookManagers_Husky(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	huskyDir := filepath.Join(tmpDir, ".husky", "_")
	if err := os.MkdirAll(huskyDir, 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Husky" {
		t.Errorf("expected Husky, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".husky/" {
		t.Errorf("expected .husky/, got %s", managers[0].ConfigPath)
	}
	if !managers[0].OverwritesHooks {
		t.Error("Husky should have OverwritesHooks=true")
	}
}

func TestDetectHookManagers_Lefthook(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" { //nolint:goconst // test assertion, not a magic string
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook.yml" {
		t.Errorf("expected lefthook.yml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("Lefthook should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_LefthookDotPrefix(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".lefthook.yml" {
		t.Errorf("expected .lefthook.yml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookToml(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.toml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.toml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook.toml" {
		t.Errorf("expected lefthook.toml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookLocal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook-local.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook-local.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook-local.yml" {
		t.Errorf("expected lefthook-local.yml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookDedup(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Both lefthook.yml and .lefthook.yml present — should only report once
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager (dedup), got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
}

func TestDetectHookManagers_PreCommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".pre-commit-config.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .pre-commit-config.yaml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "pre-commit" {
		t.Errorf("expected pre-commit, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".pre-commit-config.yaml" {
		t.Errorf("expected .pre-commit-config.yaml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("pre-commit should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_Overcommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".overcommit.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .overcommit.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Overcommit" {
		t.Errorf("expected Overcommit, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".overcommit.yml" {
		t.Errorf("expected .overcommit.yml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("Overcommit should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_Multiple(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Create Husky + pre-commit
	if err := os.MkdirAll(filepath.Join(tmpDir, ".husky", "_"), 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".pre-commit-config.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .pre-commit-config.yaml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 2 {
		t.Fatalf("expected 2 managers, got %d", len(managers))
	}

	names := make(map[string]bool)
	for _, m := range managers {
		names[m.Name] = true
	}
	if !names["Husky"] {
		t.Error("expected Husky to be detected")
	}
	if !names["pre-commit"] {
		t.Error("expected pre-commit to be detected")
	}
}

func TestHookManagerWarning_Husky(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
	}

	warning := hookManagerWarning(managers, "entire")

	// Should contain all 4 hook file references
	for _, hook := range gitHookNames {
		if !strings.Contains(warning, ".husky/"+hook+":") {
			t.Errorf("warning should contain .husky/%s:", hook)
		}
	}

	// Should contain the actual command lines from buildHookSpecs
	specs := buildHookSpecs("entire")
	for _, spec := range specs {
		cmdLine := extractCommandLine(spec.content)
		if cmdLine == "" {
			t.Errorf("failed to extract command line for %s", spec.name)
			continue
		}
		if !strings.Contains(warning, cmdLine) {
			t.Errorf("warning should contain command line %q for %s", cmdLine, spec.name)
		}
	}

	// Should mention Husky by name and warn about overwriting
	if !strings.Contains(warning, "Warning: Husky detected") {
		t.Error("warning should start with 'Warning: Husky detected'")
	}
	if !strings.Contains(warning, "may overwrite hooks") {
		t.Error("warning should mention 'may overwrite hooks'")
	}
}

func TestHookManagerWarning_GitHooksManager(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Lefthook", ConfigPath: "lefthook.yml", OverwritesHooks: false},
	}

	warning := hookManagerWarning(managers, "entire")

	// Category B: should be a Note, not a Warning
	if !strings.Contains(warning, "Note: Lefthook detected") {
		t.Error("warning should contain 'Note: Lefthook detected'")
	}
	if !strings.Contains(warning, "run 'entire enable' to restore") {
		t.Error("warning should mention running 'entire enable'")
	}

	// Should NOT contain hook file copy-paste instructions
	if strings.Contains(warning, "prepare-commit-msg:") {
		t.Error("category B warning should not contain hook file instructions")
	}
}

func TestHookManagerWarning_Empty(t *testing.T) {
	t.Parallel()

	warning := hookManagerWarning(nil, "entire")
	if warning != "" {
		t.Errorf("expected empty string for nil managers, got %q", warning)
	}

	warning = hookManagerWarning([]hookManager{}, "entire")
	if warning != "" {
		t.Errorf("expected empty string for empty managers, got %q", warning)
	}
}

func TestHookManagerWarning_LocalDev(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
	}

	warning := hookManagerWarning(managers, "go run ./cmd/entire/main.go")

	// Should use the local dev prefix in command lines
	if !strings.Contains(warning, "go run ./cmd/entire/main.go hooks git") {
		t.Error("warning should use local dev command prefix")
	}
}

func TestHookManagerWarning_Multiple(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
		{Name: "Lefthook", ConfigPath: "lefthook.yml", OverwritesHooks: false},
	}

	warning := hookManagerWarning(managers, "entire")

	if !strings.Contains(warning, "Warning: Husky detected") {
		t.Error("should contain Husky warning")
	}
	if !strings.Contains(warning, "Note: Lefthook detected") {
		t.Error("should contain Lefthook note")
	}
}

func TestExtractCommandLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "standard hook",
			content: "#!/bin/sh\n# Entire CLI hooks\nentire hooks git post-commit 2>/dev/null || true\n",
			want:    "entire hooks git post-commit 2>/dev/null || true",
		},
		{
			name:    "multiple comments",
			content: "#!/bin/sh\n# comment 1\n# comment 2\nentire hooks git pre-push \"$1\" || true\n",
			want:    `entire hooks git pre-push "$1" || true`,
		},
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "only comments",
			content: "#!/bin/sh\n# just a comment\n",
			want:    "",
		},
		{
			name:    "whitespace around command",
			content: "#!/bin/sh\n# comment\n  entire hooks git commit-msg \"$1\" || exit 1  \n",
			want:    `entire hooks git commit-msg "$1" || exit 1`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractCommandLine(tt.content)
			if got != tt.want {
				t.Errorf("extractCommandLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckAndWarnHookManagers_NoManagers(t *testing.T) {
	// Needs t.Chdir (via initHooksTestRepo), cannot be parallel
	initHooksTestRepo(t)

	var buf bytes.Buffer
	CheckAndWarnHookManagers(&buf)

	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestCheckAndWarnHookManagers_WithHusky(t *testing.T) {
	// Needs t.Chdir (via initHooksTestRepo), cannot be parallel
	tmpDir, _ := initHooksTestRepo(t)

	// Create .husky/_/ directory
	if err := os.MkdirAll(filepath.Join(tmpDir, ".husky", "_"), 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}

	var buf bytes.Buffer
	CheckAndWarnHookManagers(&buf)

	output := buf.String()
	if !strings.Contains(output, "Warning: Husky detected") {
		t.Errorf("expected warning output, got %q", output)
	}
}
