package cli

import (
	"bytes"
	"encoding/base64"
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

// The base64url form is what InstallTitleTee writes on Windows hosts; the
// tee must decode it and pipe the payload through exactly like --wrap.
func TestTitleTee_WrapBase64StillCapturesSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_ANTIGRAVITY_STATUS_DIR", dir)

	payload := `{"conversation_id":"conv-b64","context_window":{"total_input_tokens":1,"total_output_tokens":1,"context_window_size":10}}`

	cmd := newAntigravityTitleTeeCmd()
	cmd.SetArgs([]string{"--wrap-b64", base64.RawURLEncoding.EncodeToString([]byte("cat"))})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(payload))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.String() != payload {
		t.Errorf("wrapped cat did not receive the payload verbatim: got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "conv-b64.jsonl")); err != nil {
		t.Errorf("snapshot file not created under --wrap-b64: %v", err)
	}

	// A token that is not base64url is a config error, not a title error:
	// the snapshot is still captured and nothing reaches stdout.
	cmd = newAntigravityTitleTeeCmd()
	cmd.SetArgs([]string{"--wrap-b64", "not*base64!"})
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(payload))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute with a bad token: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout not empty for a bad token: %q", out.String())
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

// agy fires its title command on every state change during a turn, so
// title-tee must inherit nothing that scans session state or builds redactors:
// one real 35-second turn logged 32 "redaction configured" lines when it did.
//
// cobra.EnableTraverseRunHooks (root.go) runs EVERY ancestor's PersistentPreRun
// from the root down, not just the nearest, so title-tee cannot shadow an
// ancestor's hook with its own — the only way it inherits nothing is for no
// ancestor to define one. The command path is load-bearing and must not move
// to dodge this: `entire hooks antigravity title-tee` is persisted verbatim in
// users' agy settings.json by InstallTitleTee and matched by shape on
// uninstall, so re-parenting would silently strand every installed tee.
func TestTitleTee_InheritsNoPersistentPreRun(t *testing.T) {
	root := NewRootCmd()

	titleTee, _, err := root.Find([]string{"hooks", "antigravity", "title-tee"})
	if err != nil {
		t.Fatalf("`entire hooks antigravity title-tee` must stay at this exact path: %v", err)
	}
	if titleTee.Name() != "title-tee" {
		t.Fatalf("resolved %q, not title-tee; the persisted command path moved", titleTee.CommandPath())
	}

	// Every ancestor below the root: the root's own hook sets up logging and
	// belongs on every command. The per-agent hooks command is the one that
	// must stay clear — its hook scanned session state and built redactors.
	for p := titleTee; p != nil && p.Parent() != nil; p = p.Parent() {
		if p.PersistentPreRun != nil || p.PersistentPreRunE != nil {
			t.Errorf("%q defines a PersistentPreRun, which EnableTraverseRunHooks runs for title-tee too", p.CommandPath())
		}
	}

	// The lifecycle verbs still get one: that is where the hook session belongs.
	stop, _, err := root.Find([]string{"hooks", "antigravity", "stop"})
	if err != nil {
		t.Fatalf("find stop verb: %v", err)
	}
	if stop.PersistentPreRun == nil && stop.PersistentPreRunE == nil {
		t.Error("the stop verb lost its hook-session PersistentPreRun")
	}
}
