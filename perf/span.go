package perf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Span tracks timing for an operation and its substeps.
// A Span is not safe for concurrent use from multiple goroutines.
type Span struct {
	name     string
	start    time.Time
	parent   *Span
	children []*Span
	duration time.Duration
	attrs    []slog.Attr
	ctx      context.Context
	ended    bool
	err      error
}

// Start begins a new span. If ctx already has a span, the new one becomes a child.
// Returns the updated context and the span. Call span.End() when the operation completes.
func Start(ctx context.Context, name string, attrs ...slog.Attr) (context.Context, *Span) {
	parent := spanFromContext(ctx)
	s := &Span{
		name:   name,
		start:  time.Now(),
		parent: parent,
		attrs:  attrs,
		ctx:    ctx,
	}
	if parent != nil {
		parent.children = append(parent.children, s)
	}
	return contextWithSpan(ctx, s), s
}

// RecordError marks the span as errored. Only the first non-nil error is stored;
// subsequent calls are no-ops. Call this before End() on error paths.
func (s *Span) RecordError(err error) {
	if err == nil || s.err != nil {
		return
	}
	s.err = err
}

// End completes the span. For root spans, emits a single DEBUG log line
// with the full timing tree. For child spans, records the duration only.
// Safe to call multiple times -- subsequent calls are no-ops.
func (s *Span) End() {
	if s.ended {
		return
	}
	s.ended = true
	s.duration = time.Since(s.start)

	// Only root spans emit log output
	if s.parent != nil {
		return
	}

	// Build log attributes: op, duration_ms, error flag, then child step durations.
	// Component is set via context so it appears exactly once in the log line.
	logCtx := logging.WithComponent(s.ctx, "perf")
	attrs := make([]any, 0, 3+2*len(s.children)+len(s.attrs))
	attrs = append(attrs, slog.String("op", s.name))
	attrs = append(attrs, slog.Int64("duration_ms", s.duration.Milliseconds()))
	if s.err != nil {
		attrs = append(attrs, slog.Bool("error", true))
	}

	// Add child step durations (and error flags) as flat keys
	for _, child := range s.children {
		// Auto-end children that were not explicitly ended
		if !child.ended {
			child.End()
		}
		key := fmt.Sprintf("steps.%s_ms", child.name)
		attrs = append(attrs, slog.Int64(key, child.duration.Milliseconds()))
		if child.err != nil {
			errKey := fmt.Sprintf("steps.%s_err", child.name)
			attrs = append(attrs, slog.Bool(errKey, true))
		}
	}

	// Add any extra attributes from Start()
	for _, a := range s.attrs {
		attrs = append(attrs, a)
	}

	logging.Debug(logCtx, "perf", attrs...)
}
