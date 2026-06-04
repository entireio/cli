package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Note: these tests use t.Setenv, so t.Parallel() is not called.

func TestTitleTee_WritesSnapshotAndStaysSilent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_ANTIGRAVITY_STATUS_DIR", dir)

	payload := `{"conversation_id":"conv-9","agent_state":"working","context_window":{"total_input_tokens":500,"total_output_tokens":25,"context_window_size":100000,"current_usage":{"input_tokens":400,"output_tokens":25,"cache_creation_input_tokens":50,"cache_read_input_tokens":300}}}`

	cmd := newAntigravityTitleTeeCmd()

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(payload))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// stdout must be empty — agy renders it verbatim as the window title
	if out.Len() != 0 {
		t.Errorf("stdout not empty: %q", out.String())
	}

	// snapshot file must exist
	snapFile := filepath.Join(dir, "conv-9.jsonl")
	if _, err := os.Stat(snapFile); err != nil {
		t.Errorf("snapshot file not created: %v", err)
	}
}

func TestTitleTee_WrapPipesPayloadThrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_ANTIGRAVITY_STATUS_DIR", dir)

	payload := `{"conversation_id":"conv-10","agent_state":"idle","context_window":{"total_input_tokens":100,"total_output_tokens":5}}`

	cmd := newAntigravityTitleTeeCmd()
	cmd.SetArgs([]string{"--wrap", "cat"})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(payload))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := strings.TrimSpace(out.String())
	want := strings.TrimSpace(payload)
	if got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestTitleTee_WrapStillCapturesSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_ANTIGRAVITY_STATUS_DIR", dir)

	payload := `{"conversation_id":"conv-11","agent_state":"working","context_window":{"total_input_tokens":700,"total_output_tokens":40}}`

	cmd := newAntigravityTitleTeeCmd()
	cmd.SetArgs([]string{"--wrap", "cat"})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(payload))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// --wrap cat must pipe the payload through verbatim...
	if got, want := strings.TrimSpace(out.String()), strings.TrimSpace(payload); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}

	// ...AND the snapshot must still be captured.
	snapFile := filepath.Join(dir, "conv-11.jsonl")
	if _, err := os.Stat(snapFile); err != nil {
		t.Errorf("snapshot file not created under --wrap: %v", err)
	}
}

func TestTitleTee_GarbageInputAndFailingWrapNeverError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_ANTIGRAVITY_STATUS_DIR", dir)

	cmd := newAntigravityTitleTeeCmd()
	cmd.SetArgs([]string{"--wrap", "false"})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("not json"))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
}
