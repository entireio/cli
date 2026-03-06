package perf

import "context"

type contextKey struct{}

// spanFromContext retrieves the current span from context, or nil if none.
func spanFromContext(ctx context.Context) *Span {
	if v := ctx.Value(contextKey{}); v != nil {
		if s, ok := v.(*Span); ok {
			return s
		}
	}
	return nil
}

// contextWithSpan returns a new context with the span stored.
func contextWithSpan(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}
