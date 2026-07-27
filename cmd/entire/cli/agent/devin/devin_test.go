package devin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// liveSessionID is the word-pair session ID captured from the live devin run.
const liveSessionID = "snowy-efraasia"

func TestIdentity(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if d.Name() != agent.AgentNameDevin {
		t.Errorf("Name = %q", d.Name())
	}
	if d.Type() != agent.AgentTypeDevin {
		t.Errorf("Type = %q", d.Type())
	}
	if !d.IsPreview() {
		t.Error("IsPreview = false, want true")
	}
	dirs := d.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".devin" {
		t.Errorf("ProtectedDirs = %v", dirs)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if got := d.FormatResumeCommand(liveSessionID); got != "devin -r "+liveSessionID {
		t.Errorf("FormatResumeCommand = %q", got)
	}
	if got := d.FormatResumeCommand(""); got != "devin -c" {
		t.Errorf("FormatResumeCommand empty = %q", got)
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	got := d.ResolveSessionFile("/data/transcripts", liveSessionID)
	want := filepath.Join("/data/transcripts", liveSessionID+".json")
	if got != want {
		t.Errorf("ResolveSessionFile = %q, want %q", got, want)
	}
}

func TestGetSessionDir_TestOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", dir)
	d := &DevinAgent{}
	got, err := d.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}
	if got != dir {
		t.Errorf("GetSessionDir = %q, want override %q", got, dir)
	}
}

func TestGetSessionDir_XDGDataHome(t *testing.T) {
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	d := &DevinAgent{}
	got, err := d.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}
	want := filepath.Join(xdg, "devin", "cli", "transcripts")
	if got != want {
		t.Errorf("GetSessionDir = %q, want %q", got, want)
	}
}

func TestGetSessionDir_FlatLayoutIgnoresRepoPath(t *testing.T) {
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	d := &DevinAgent{}
	a, err := d.GetSessionDir("/repo/one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.GetSessionDir("/repo/two")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("session dir differs by repo (%q vs %q); Devin's transcript dir is flat", a, b)
	}
}

func TestDetectPresence(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	d := &DevinAgent{}
	present, err := d.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence: %v", err)
	}
	if present {
		t.Error("DetectPresence = true without .devin dir")
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".devin"), 0o750); err != nil {
		t.Fatal(err)
	}
	present, err = d.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence: %v", err)
	}
	if !present {
		t.Error("DetectPresence = false with .devin dir")
	}
}

func TestReadWriteSession_RoundTrip(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	path := filepath.Join(t.TempDir(), liveSessionID+".json")
	if err := os.WriteFile(path, []byte(sampleTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := d.ReadSession(&agent.HookInput{SessionID: liveSessionID, SessionRef: path})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if session.SessionID != liveSessionID || session.AgentName != agent.AgentNameDevin {
		t.Errorf("session identity = %q/%q", session.SessionID, session.AgentName)
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData is empty")
	}
	if len(session.ModifiedFiles) != 2 {
		t.Errorf("ModifiedFiles = %v, want 2 entries", session.ModifiedFiles)
	}

	// Write back to a new location (restore path).
	restored := filepath.Join(t.TempDir(), "restored", liveSessionID+".json")
	session.SessionRef = restored
	if err := d.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	data, err := os.ReadFile(restored)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(data) != sampleTranscript {
		t.Error("restored transcript differs from original")
	}
}

func TestReadSession_MissingRef(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if _, err := d.ReadSession(&agent.HookInput{SessionID: "x"}); err == nil {
		t.Error("expected error for missing SessionRef")
	}
}

func TestWriteSession_Validation(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	ctx := context.Background()

	if err := d.WriteSession(ctx, nil); err == nil {
		t.Error("expected error for nil session")
	}
	if err := d.WriteSession(ctx, &agent.AgentSession{
		AgentName:  agent.AgentNameClaudeCode,
		SessionRef: "/tmp/x.json",
		NativeData: []byte("{}"),
	}); err == nil {
		t.Error("expected error for wrong agent")
	}
	if err := d.WriteSession(ctx, &agent.AgentSession{
		AgentName:  agent.AgentNameDevin,
		SessionRef: filepath.Join(t.TempDir(), "x.json"),
	}); err == nil {
		t.Error("expected error for empty NativeData")
	}
}

func TestRegistryRegistration(t *testing.T) {
	t.Parallel()
	ag, err := agent.Get(agent.AgentNameDevin)
	if err != nil {
		t.Fatalf("agent.Get(devin): %v", err)
	}
	if ag.Type() != agent.AgentTypeDevin {
		t.Errorf("registered agent type = %q", ag.Type())
	}
	if _, ok := ag.(agent.HookSupport); !ok {
		t.Error("registered devin agent does not implement HookSupport")
	}
}
