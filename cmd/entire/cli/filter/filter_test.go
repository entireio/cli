package filter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestContent_NoFilter(t *testing.T) {
	// When no filter is configured (settings return nil), content passes through unchanged
	content := []byte("sensitive data: api_key=sk-12345")

	result, err := Content(context.Background(), content, "test.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(result, content) {
		t.Errorf("expected content to pass through unchanged, got %q", result)
	}
}

func TestRunFilter_Success(t *testing.T) {
	// Test with cat - should pass content through unchanged
	content := []byte("hello world")

	result, err := runFilter(context.Background(), content, []string{"cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(result, content) {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestRunFilter_WithArgs(t *testing.T) {
	// Test with sed - should transform content
	content := []byte("secret_key")

	result, err := runFilter(context.Background(), content, []string{"sed", "s/secret/REDACTED/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []byte("REDACTED_key")
	if !bytes.Equal(result, expected) {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestRunFilter_CommandNotFound(t *testing.T) {
	content := []byte("test")

	_, err := runFilter(context.Background(), content, []string{"nonexistent-filter-command-xyz"})
	if err == nil {
		t.Fatal("expected error for nonexistent command")
	}

	if !errors.Is(err, ErrFilterNotFound) {
		t.Errorf("expected ErrFilterNotFound, got: %v", err)
	}
}

func TestRunFilter_NonZeroExit(t *testing.T) {
	content := []byte("test")

	// Use sh -c "exit 1" to simulate a failed command
	_, err := runFilter(context.Background(), content, []string{"sh", "-c", "echo error >&2; exit 1"})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}

	if !errors.Is(err, ErrFilterFailed) {
		t.Errorf("expected ErrFilterFailed, got: %v", err)
	}
}

func TestRunFilter_Timeout(t *testing.T) {
	// Skip in short mode as this test takes time
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}

	content := []byte("test")

	// Create a context with a very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// sleep command will exceed our timeout
	_, err := runFilter(ctx, content, []string{"sleep", "10"})
	if err == nil {
		t.Fatal("expected error for timeout")
	}

	if !errors.Is(err, ErrFilterTimeout) {
		t.Errorf("expected ErrFilterTimeout, got: %v", err)
	}
}

func TestRunFilter_LargeContent(t *testing.T) {
	// Test with larger content to ensure piping works correctly
	content := bytes.Repeat([]byte("x"), 1024*1024) // 1MB

	result, err := runFilter(context.Background(), content, []string{"cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(result, content) {
		t.Errorf("content mismatch for large input")
	}
}

func TestRunFilter_EmptyContent(t *testing.T) {
	content := []byte{}

	result, err := runFilter(context.Background(), content, []string{"cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestRunFilter_MultilineContent(t *testing.T) {
	content := []byte("line1\nline2\nline3\n")

	result, err := runFilter(context.Background(), content, []string{"cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(result, content) {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestRunFilter_EchoFoo_ReplacesContent(t *testing.T) {
	// Test that ["echo", "foo"] completely replaces input with "foo\n"
	// This verifies the filter command's stdout becomes the output,
	// regardless of what was passed as input
	content := []byte("this is sensitive data with api_key=sk-12345 that should be completely replaced")

	result, err := runFilter(context.Background(), content, []string{"echo", "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// echo adds a newline
	expected := []byte("foo\n")
	if !bytes.Equal(result, expected) {
		t.Errorf("expected %q, got %q", expected, result)
	}

	// Verify the original sensitive content is NOT in the output
	if bytes.Contains(result, []byte("api_key")) {
		t.Error("filtered output should not contain original sensitive content")
	}
	if bytes.Contains(result, []byte("sk-12345")) {
		t.Error("filtered output should not contain original secret")
	}
}

func TestRunFilter_ScriptFilter(t *testing.T) {
	// Create a temporary script that acts as a filter
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "filter.sh")

	script := `#!/bin/sh
sed 's/api_key=[^[:space:]]*/api_key=REDACTED/g'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	content := []byte("config: api_key=sk-12345 other=value")

	result, err := runFilter(context.Background(), content, []string{scriptPath})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []byte("config: api_key=REDACTED other=value")
	if !bytes.Equal(result, expected) {
		t.Errorf("expected %q, got %q", expected, result)
	}
}
