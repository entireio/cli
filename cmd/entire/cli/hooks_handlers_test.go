package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
)

func TestCurrentSessionIDWithFallback_UsesPersisted(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init error: %v", err)
	}

	t.Cleanup(paths.ClearRepoRootCache)
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()

	persisted := "2024-01-02-session123"
	if err := paths.WriteCurrentSession(persisted); err != nil {
		t.Fatalf("WriteCurrentSession error: %v", err)
	}

	got := currentSessionIDWithFallback("session123")
	if got != persisted {
		t.Fatalf("currentSessionIDWithFallback() = %q, want %q", got, persisted)
	}
}

func TestCurrentSessionIDWithFallback_FallsBackToModel(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init error: %v", err)
	}

	t.Cleanup(paths.ClearRepoRootCache)
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()

	modelID := "model123"
	got := currentSessionIDWithFallback(modelID)
	if got == "" {
		t.Fatal("currentSessionIDWithFallback() returned empty string")
	}
	if !strings.HasSuffix(got, "-"+modelID) {
		t.Fatalf("currentSessionIDWithFallback() = %q, want suffix %q", got, "-"+modelID)
	}
}

func TestCurrentSessionIDWithFallback_InvalidPersistedFallsBack(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init error: %v", err)
	}

	t.Cleanup(paths.ClearRepoRootCache)
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()

	// Write an invalid session ID (contains path separator)
	invalid := "2024-01-02/malicious-path"
	if err := paths.WriteCurrentSession(invalid); err != nil {
		t.Fatalf("WriteCurrentSession error: %v", err)
	}

	modelID := "model123"
	got := currentSessionIDWithFallback(modelID)

	// Should fall back to model ID, not return the invalid persisted value
	if got == invalid {
		t.Fatal("currentSessionIDWithFallback() returned invalid session ID, should have fallen back")
	}
	if !strings.HasSuffix(got, "-"+modelID) {
		t.Fatalf("currentSessionIDWithFallback() = %q, want suffix %q", got, "-"+modelID)
	}
}

func TestCurrentSessionIDWithFallback_MismatchedPersistedFallsBack(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init error: %v", err)
	}

	t.Cleanup(paths.ClearRepoRootCache)
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()

	persisted := "2024-01-02-session123"
	if err := paths.WriteCurrentSession(persisted); err != nil {
		t.Fatalf("WriteCurrentSession error: %v", err)
	}

	modelID := "model456"
	got := currentSessionIDWithFallback(modelID)
	if got == "" {
		t.Fatal("currentSessionIDWithFallback() returned empty string")
	}
	if got == persisted {
		t.Fatal("currentSessionIDWithFallback() returned persisted ID for mismatched model session")
	}
	if !strings.HasSuffix(got, "-"+modelID) {
		t.Fatalf("currentSessionIDWithFallback() = %q, want suffix %q", got, "-"+modelID)
	}
}
