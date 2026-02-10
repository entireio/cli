package strategy

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Factory creates a new strategy instance
type Factory func() Strategy

// Register adds a strategy factory to the registry.
// This is typically called from init() functions in strategy implementations.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Get retrieves a strategy by name.
// Returns an error if the strategy is not registered.
func Get(name string) (Strategy, error) { //nolint:ireturn // already present in codebase
	registryMu.RLock()
	defer registryMu.RUnlock()

	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown strategy: %s (available: %v)", name, List())
	}

	return factory(), nil
}

// List returns all registered strategy names in sorted order.
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

// Strategy name constants
const (
	StrategyNameManualCommit = "manual-commit"
	StrategyNameAutoCommit   = "auto-commit"
)

// DefaultStrategyName is the name of the default strategy.
// Manual-commit is the recommended strategy for most workflows.
const DefaultStrategyName = StrategyNameManualCommit

// Default returns the default strategy.
// Falls back to returning nil if no strategies are registered.
func Default() Strategy { //nolint:ireturn // already present in codebase
	s, err := Get(DefaultStrategyName)
	if err != nil {
		// Fallback: return the first registered strategy
		names := List()
		if len(names) > 0 {
			s, _ = Get(names[0]) //nolint:errcheck // Fallback to first strategy, error already handled above
		}
	}
	return s
}
