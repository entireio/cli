package logging

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Context keys for logging values.
// Using private types to avoid key collisions.
type contextKey int

const (
	componentKey contextKey = iota
	agentKey
)

// WithComponent adds a component name to the context.
// Component names help identify the subsystem generating logs (e.g., "hooks", "strategy", "session").
func WithComponent(ctx context.Context, component string) context.Context {
	return context.WithValue(ctx, componentKey, component)
}

// WithAgent adds an agent name to the context.
// Agent names identify the AI agent generating activity (e.g., "claude-code", "cursor", "aider").
func WithAgent(ctx context.Context, agentName types.AgentName) context.Context {
	return context.WithValue(ctx, agentKey, string(agentName))
}
