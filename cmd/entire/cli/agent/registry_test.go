package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func TestRegistryOperations(t *testing.T) {
	// Save original registry state and restore after test
	originalRegistry := make(map[types.AgentName]Factory)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	// Clear registry for testing
	registry = make(map[types.AgentName]Factory)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("Register and Get", func(t *testing.T) {
		Register(types.AgentName("test-agent"), func() Agent {
			return &mockAgent{}
		})

		agent, err := Get(types.AgentName("test-agent"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if agent.Name() != mockAgentName {
			t.Errorf("expected Name() %q, got %q", mockAgentName, agent.Name())
		}
	})

	t.Run("Get unknown agent returns error", func(t *testing.T) {
		_, err := Get(types.AgentName("nonexistent-agent"))
		if err == nil {
			t.Error("expected error for unknown agent")
		}
		if !strings.Contains(err.Error(), "unknown agent") {
			t.Errorf("expected 'unknown agent' in error, got: %v", err)
		}
	})

	t.Run("List returns registered agents", func(t *testing.T) {
		// Clear and register fresh
		registryMu.Lock()
		registry = make(map[types.AgentName]Factory)
		registryMu.Unlock()

		Register(types.AgentName("agent-b"), func() Agent { return &mockAgent{} })
		Register(types.AgentName("agent-a"), func() Agent { return &mockAgent{} })

		names := List()
		if len(names) != 2 {
			t.Errorf("expected 2 agents, got %d", len(names))
		}
		// List should return sorted
		if names[0] != types.AgentName("agent-a") || names[1] != types.AgentName("agent-b") {
			t.Errorf("expected sorted list [agent-a, agent-b], got %v", names)
		}
	})
}

func TestDetect(t *testing.T) {
	// Save original registry state
	originalRegistry := make(map[types.AgentName]Factory)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]Factory)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("returns error when no agents detected", func(t *testing.T) {
		// Register an agent that won't be detected
		Register(types.AgentName("undetected"), func() Agent {
			return &mockAgent{} // DetectPresence returns false
		})

		_, err := Detect(context.Background())
		if err == nil {
			t.Error("expected error when no agent detected")
		}
		if !strings.Contains(err.Error(), "no agent detected") {
			t.Errorf("expected 'no agent detected' in error, got: %v", err)
		}
	})

	t.Run("returns detected agent", func(t *testing.T) {
		// Clear registry
		registryMu.Lock()
		registry = make(map[types.AgentName]Factory)
		registryMu.Unlock()

		// Register an agent that will be detected
		Register(types.AgentName("detected"), func() Agent {
			return &detectableAgent{}
		})

		agent, err := Detect(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if agent.Name() != types.AgentName("detectable") {
			t.Errorf("expected Name() %q, got %q", "detectable", agent.Name())
		}
	})
}

// detectableAgent is a mock that returns true for DetectPresence
type detectableAgent struct {
	mockAgent
}

func (d *detectableAgent) Name() types.AgentName {
	return types.AgentName("detectable")
}

func (d *detectableAgent) DetectPresence(_ context.Context) (bool, error) {
	return true, nil
}

func TestAgentNameConstants(t *testing.T) {
	if AgentNameClaudeCode != "claude-code" {
		t.Errorf("expected AgentNameClaudeCode %q, got %q", "claude-code", AgentNameClaudeCode)
	}
	if AgentNameGemini != "gemini" {
		t.Errorf("expected AgentNameGemini %q, got %q", "gemini", AgentNameGemini)
	}
}

func TestDefaultAgentName(t *testing.T) {
	// DefaultAgentName is for the `entire enable` setup flow when no agent is
	// detected. It is NOT used for agent attribution fallbacks — those use
	// AgentTypeUnknown ("Unknown") or "Unknown" in the DB.
	if DefaultAgentName != AgentNameClaudeCode {
		t.Errorf("expected DefaultAgentName %q, got %q", AgentNameClaudeCode, DefaultAgentName)
	}
}

func TestDefault(t *testing.T) {
	// Default() returns nil if default agent is not registered
	// This test verifies the function doesn't panic
	originalRegistry := make(map[types.AgentName]Factory)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]Factory)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	agent := Default()
	if agent != nil {
		t.Error("expected nil when default agent not registered")
	}

	// Register the default agent
	Register(DefaultAgentName, func() Agent {
		return &mockAgent{}
	})

	agent = Default()
	if agent == nil {
		t.Error("expected non-nil agent after registering default")
	}
}

func TestAllProtectedDirs(t *testing.T) {
	// Save original registry state
	originalRegistry := make(map[types.AgentName]Factory)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]Factory)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("empty registry returns empty", func(t *testing.T) {
		dirs := AllProtectedDirs()
		if len(dirs) != 0 {
			t.Errorf("expected empty dirs, got %v", dirs)
		}
	})

	t.Run("collects dirs from registered agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]Factory)
		registryMu.Unlock()

		Register(types.AgentName("agent-a"), func() Agent {
			return &protectedDirAgent{dirs: []string{".agent-a"}}
		})
		Register(types.AgentName("agent-b"), func() Agent {
			return &protectedDirAgent{dirs: []string{".agent-b", ".shared"}}
		})

		dirs := AllProtectedDirs()
		if len(dirs) != 3 {
			t.Fatalf("expected 3 dirs, got %d: %v", len(dirs), dirs)
		}
		// AllProtectedDirs returns sorted
		expected := []string{".agent-a", ".agent-b", ".shared"}
		for i, d := range dirs {
			if d != expected[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, d, expected[i])
			}
		}
	})

	t.Run("deduplicates across agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]Factory)
		registryMu.Unlock()

		Register(types.AgentName("agent-x"), func() Agent {
			return &protectedDirAgent{dirs: []string{".shared"}}
		})
		Register(types.AgentName("agent-y"), func() Agent {
			return &protectedDirAgent{dirs: []string{".shared"}}
		})

		dirs := AllProtectedDirs()
		if len(dirs) != 1 {
			t.Errorf("expected 1 dir (deduplicated), got %d: %v", len(dirs), dirs)
		}
	})
}

// protectedDirAgent is a mock that returns configurable protected dirs.
type protectedDirAgent struct {
	mockAgent

	dirs []string
}

func (p *protectedDirAgent) ProtectedDirs() []string { return p.dirs }
