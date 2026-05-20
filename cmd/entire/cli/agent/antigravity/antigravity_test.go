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
	t.Run("no antigravity directories", func(t *testing.T) {
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

	t.Run("with .agents directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)
		if err := os.Mkdir(".agents", 0o755); err != nil {
			t.Fatalf("failed to create .agents: %v", err)
		}

		ag := &AntigravityAgent{}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})

	t.Run("with .gemini/jetski directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)
		if err := os.MkdirAll(filepath.Join(".gemini", "jetski"), 0o755); err != nil {
			t.Fatalf("failed to create .gemini/jetski: %v", err)
		}

		ag := &AntigravityAgent{}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})
}
