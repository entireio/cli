package antigravity

import (
	"context"
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
