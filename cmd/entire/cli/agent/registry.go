package agent

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Factory creates a new agent instance
type Factory func() Agent

// Register adds an agent factory to the registry.
// Called from init() in each agent implementation.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Get retrieves an agent by name.
//
//nolint:ireturn // Factory pattern requires returning the interface
func Get(name string) (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent: %s (available: %v)", name, List())
	}
	return factory(), nil
}

// List returns all registered agent names in sorted order.
func List() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Detect attempts to auto-detect which agent is being used.
// Checks each registered agent's DetectPresence method.
//
//nolint:ireturn // Factory pattern requires returning the interface
func Detect() (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	for _, factory := range registry {
		ag := factory()
		if present, err := ag.DetectPresence(); err == nil && present {
			return ag, nil
		}
	}
	return nil, fmt.Errorf("no agent detected (available: %v)", List())
}

// Agent name constants
const (
	AgentNameClaudeCode = "claude-code"
	AgentNameOpenCode   = "opencode"
	AgentNameCursor     = "cursor"
	AgentNameWindsurf   = "windsurf"
	AgentNameAider      = "aider"
	AgentNameGemini     = "gemini"
)

// DefaultAgentName is the default when none specified
const DefaultAgentName = AgentNameClaudeCode

// Default returns the default agent.
// Returns nil if the default agent is not registered.
//
//nolint:ireturn,errcheck // Factory pattern returns interface; error is acceptable to ignore for default
func Default() Agent {
	a, _ := Get(DefaultAgentName)
	return a
}
