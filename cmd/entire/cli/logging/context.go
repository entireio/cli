package logging

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Context keys for logging values.
// Using private types to avoid key collisions.
type contextKey int

const (
	componentKey contextKey = iota
	agentKey
	sessionKey
	loggerKey
)

// sessionIDAttrKey is named because log() must also recognize it among
// caller-supplied attrs.
const sessionIDAttrKey = "session_id"

// WithLogger attaches a Logger to the context. The exit point closes it by
// reading it back out — the logger has no other owner — so don't stash it
// anywhere that outlives the command.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFromContext returns the Logger attached by WithLogger, or nil when
// logging is not file-backed. Its methods are nil-safe, so a nil result can be
// closed or asked for its Slog without a guard.
func LoggerFromContext(ctx context.Context) *Logger {
	if ctx == nil {
		return nil
	}
	if l, ok := ctx.Value(loggerKey).(*Logger); ok {
		return l
	}
	return nil
}

// WithSessionID adds a session ID to the context, so lines logged under it are
// filterable by session. Re-stamping a derived context shadows the outer
// session for that scope only.
//
// It deliberately does not validate: this is an slog attribute, not a path, so
// guarding traversal belongs where the ID is resolved from the filesystem.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey, sessionID)
}

// SessionLoggerFromContext returns the context's logger stamped with its
// session ID, for packages that hold an injected *slog.Logger and call it
// without a context (redact) — those calls never reach log(), so they cannot
// pick the attribute up themselves.
//
// Only the session is stamped. redact tags its own lines with
// component=redaction, and slog does not dedupe attrs, so adding component here
// would emit the key twice.
func SessionLoggerFromContext(ctx context.Context) *slog.Logger {
	l := LoggerFromContext(ctx).Slog()
	if l == nil {
		return nil
	}
	if sessionID := sessionIDFromContext(ctx); sessionID != "" {
		return l.With(slog.String(sessionIDAttrKey, sessionID))
	}
	return l
}

// sessionIDFromContext returns the session ID attached by WithSessionID, or "".
func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s, ok := ctx.Value(sessionKey).(string); ok {
		return s
	}
	return ""
}

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
