package grok

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestAgentIdentity(t *testing.T) {
	t.Parallel()

	g := NewGrokAgent()
	if got := string(g.Name()); got != "grok" {
		t.Errorf("Name = %q, want grok", got)
	}
	if got := string(g.Type()); got != "Grok Build" {
		t.Errorf("Type = %q, want Grok Build", got)
	}
	if g.Description() == "" {
		t.Error("Description is empty")
	}
	if !g.IsPreview() {
		t.Error("IsPreview = false; the integration is new and unproven at scale")
	}
	if got := g.ProtectedDirs(); len(got) != 1 || got[0] != ".grok" {
		t.Errorf("ProtectedDirs = %v, want [.grok]", got)
	}
}

func TestRegisteredInGlobalRegistry(t *testing.T) {
	t.Parallel()

	got, err := agent.Get(agent.AgentNameGrok)
	if err != nil {
		t.Fatalf("agent.Get(grok): %v", err)
	}
	if got.Type() != agent.AgentTypeGrok {
		t.Errorf("registered agent Type = %q, want %q", got.Type(), agent.AgentTypeGrok)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	if got, want := g.FormatResumeCommand("abc-123"), "grok --resume abc-123"; got != want {
		t.Errorf("FormatResumeCommand = %q, want %q", got, want)
	}
}

// TestGetSessionDir_EncodesTheWorkingDirectory pins the encoding Grok uses to
// name a session group: the absolute path percent-encoded with separators
// escaped too. Getting this wrong means every transcript lookup misses.
func TestGetSessionDir_EncodesTheWorkingDirectory(t *testing.T) {
	t.Setenv("GROK_HOME", filepath.Join("/tmp", "grokhome"))

	g := &GrokAgent{}
	dir, err := g.GetSessionDir("/repo/project")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}

	group := filepath.Base(dir)
	if group != "%2Frepo%2Fproject" {
		t.Errorf("group = %q, want %%2Frepo%%2Fproject", group)
	}
	if strings.Contains(group, "/") {
		t.Errorf("group %q still contains a path separator", group)
	}
	if want := filepath.Join("/tmp", "grokhome", "sessions"); filepath.Dir(dir) != want {
		t.Errorf("parent = %q, want %q", filepath.Dir(dir), want)
	}
}

func TestEncodeCWD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "/a/b", "%2Fa%2Fb"},
		{"space", "/a b", "%2Fa%20b"},
		{"non-ascii", "/a/’b", "%2Fa%2F%E2%80%99b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := encodeCWD(tt.in); got != tt.want {
				t.Errorf("encodeCWD(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	got := g.ResolveSessionFile("/base/group", "sess-1")
	want := filepath.Join("/base/group", "sess-1", "updates.jsonl")
	if got != want {
		t.Errorf("ResolveSessionFile = %q, want %q", got, want)
	}
}

// TestGetSessionDir_HonorsGrokHome matters for the E2E runner, which isolates
// every run behind its own GROK_HOME.
func TestGetSessionDir_HonorsGrokHome(t *testing.T) {
	t.Setenv("GROK_HOME", "/custom/home")

	g := &GrokAgent{}
	dir, err := g.GetSessionDir("/repo")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}
	if !strings.HasPrefix(dir, filepath.Join("/custom/home", "sessions")) {
		t.Errorf("dir = %q, want it under /custom/home/sessions", dir)
	}

	base, err := g.GetSessionBaseDir()
	if err != nil {
		t.Fatalf("GetSessionBaseDir: %v", err)
	}
	if base != filepath.Join("/custom/home", "sessions") {
		t.Errorf("base = %q, want /custom/home/sessions", base)
	}
}

func TestWriteSessionRejectsForeignAgent(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	err := g.WriteSession(t.Context(), &agent.AgentSession{
		AgentName:  agent.AgentNameClaudeCode,
		SessionRef: filepath.Join(t.TempDir(), "updates.jsonl"),
		NativeData: []byte("{}\n"),
	})
	if err == nil {
		t.Fatal("expected an error writing a Claude Code session through the Grok agent")
	}
}

func TestReadWriteSessionRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ref := filepath.Join(dir, "nested", "updates.jsonl")
	data := []byte(`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","content":[{"type":"diff","path":"/abs/f.go"}]}}}` + "\n")

	g := &GrokAgent{}
	if err := g.WriteSession(t.Context(), &agent.AgentSession{
		AgentName:  agent.AgentNameGrok,
		SessionRef: ref,
		NativeData: data,
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	got, err := g.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: ref})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if string(got.NativeData) != string(data) {
		t.Error("NativeData changed across the round trip")
	}
	if len(got.ModifiedFiles) != 1 || got.ModifiedFiles[0] != "/abs/f.go" {
		t.Errorf("ModifiedFiles = %v, want [/abs/f.go]", got.ModifiedFiles)
	}
}

func TestReadSessionRequiresRef(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	if _, err := g.ReadSession(&agent.HookInput{SessionID: "s1"}); err == nil {
		t.Error("expected an error when SessionRef is empty")
	}
}
