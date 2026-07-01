package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestAgent_ImplementsAgentAndHookSupport(t *testing.T) {
	t.Parallel()
	var _ agent.Agent = (*AntigravityAgent)(nil)
	var _ agent.HookSupport = (*AntigravityAgent)(nil)
}

func TestAgent_NameAndType(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	if a.Name() != agent.AgentNameAntigravity {
		t.Errorf("Name() = %q", a.Name())
	}
	if a.Type() != agent.AgentTypeAntigravity {
		t.Errorf("Type() = %q", a.Type())
	}
}

func TestAgent_Registered(t *testing.T) {
	t.Parallel()
	_, err := agent.Get(agent.AgentNameAntigravity)
	if err != nil {
		t.Fatalf("agent not registered: %v", err)
	}
}

func TestDetectPresence(t *testing.T) {
	t.Run("no hooks installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &AntigravityAgent{}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false")
		}
	})

	t.Run("hooks installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &AntigravityAgent{}
		if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
			t.Fatalf("InstallHooks: %v", err)
		}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true after InstallHooks")
		}
	})
}

func TestGetSessionDir_HonorsTestOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "brain")
	t.Setenv("ENTIRE_TEST_ANTIGRAVITY_BRAIN_DIR", override)

	got, err := (&AntigravityAgent{}).GetSessionDir("/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if got != override {
		t.Fatalf("GetSessionDir() = %q, want %q", got, override)
	}
}

func TestResolveSessionFile_UsesAntigravityBrainTranscriptPath(t *testing.T) {
	t.Parallel()

	got := (&AntigravityAgent{}).ResolveSessionFile("/home/me/.gemini/antigravity-cli/brain", "conv-123")
	want := filepath.Join(
		"/home/me/.gemini/antigravity-cli/brain",
		"conv-123",
		".system_generated",
		"logs",
		"transcript_full.jsonl",
	)
	if got != want {
		t.Fatalf("ResolveSessionFile() = %q, want %q", got, want)
	}
}

func TestWriteSession_WritesTranscriptData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(
		t.TempDir(),
		"brain",
		"conv-123",
		".system_generated",
		"logs",
		"transcript_full.jsonl",
	)
	data := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE"}` + "\n")
	err := (&AntigravityAgent{}).WriteSession(context.Background(), &agent.AgentSession{
		SessionID:  "conv-123",
		AgentName:  agent.AgentNameAntigravity,
		SessionRef: path,
		NativeData: data,
	})
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored transcript: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("restored transcript = %q, want %q", got, data)
	}
}

func TestWriteSession_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	ag := &AntigravityAgent{}
	validPath := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	cases := []struct {
		name    string
		session *agent.AgentSession
	}{
		{name: "nil session", session: nil},
		{name: "wrong agent", session: &agent.AgentSession{
			AgentName:  agent.AgentNameGemini,
			SessionRef: validPath,
			NativeData: []byte("{}\n"),
		}},
		{name: "empty ref", session: &agent.AgentSession{
			AgentName:  agent.AgentNameAntigravity,
			NativeData: []byte("{}\n"),
		}},
		{name: "empty data", session: &agent.AgentSession{
			AgentName:  agent.AgentNameAntigravity,
			SessionRef: validPath,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ag.WriteSession(context.Background(), tc.session); err == nil {
				t.Fatal("WriteSession() error = nil, want error")
			}
		})
	}
}
