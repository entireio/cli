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
	name       string
	start      time.Time
	parent     *Span
	children   []*Span
	duration   time.Duration
	attrs      []slog.Attr
	ctx        context.Context
	ended      bool
	err        error
	isLoopIter bool
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

	logCtx := logging.WithComponent(s.ctx, "perf")
	attrs := make([]any, 0, 3+len(s.attrs)+countChildStepAttrs(s))
	attrs = append(attrs, slog.String("op", s.name))
	attrs = append(attrs, slog.Int64("duration_ms", s.duration.Milliseconds()))
	if s.err != nil {
		attrs = append(attrs, slog.Bool("error", true))
	}

	attrs = appendChildStepAttrs(attrs, s, "")

	for _, a := range s.attrs {
		attrs = append(attrs, a)
	}

	logging.Debug(logCtx, "perf", attrs...)
}

// appendChildStepAttrs emits the full child timing tree under parent.
//
// Normal children use their span names, with ~N suffixes for duplicate sibling
// names. Loop iterations (from LoopSpan.Iteration) keep the historical numeric
// keys: steps.<loopName>.0_ms, steps.<loopName>.1_ms, etc.
func appendChildStepAttrs(attrs []any, parent *Span, parentKey string) []any {
	seen := make(map[string]int, len(parent.children))
	loopIndex := 0
	for _, child := range parent.children {
		if !child.ended {
			child.End()
		}

		var stepKey string
		if child.isLoopIter && parentKey != "" {
			stepKey = fmt.Sprintf("%s.%d", parentKey, loopIndex)
			loopIndex++
		} else {
			stepKey = childStepKey(child.name, seen)
			if parentKey != "" {
				stepKey = parentKey + "." + stepKey
			}
		}

		attrs = append(attrs, slog.Int64("steps."+stepKey+"_ms", child.duration.Milliseconds()))
		if child.err != nil {
			attrs = append(attrs, slog.Bool("steps."+stepKey+"_err", true))
		}

		attrs = appendChildStepAttrs(attrs, child, stepKey)
	}
	return attrs
}

func countChildStepAttrs(parent *Span) int {
	count := 0
	for _, child := range parent.children {
		count++
		if child.err != nil {
			count++
		}
		count += countChildStepAttrs(child)
	}
	return count
}

// childStepKey returns a unique key for a child span name.
// First occurrence keeps the original name; subsequent get ~1, ~2, etc.
// Uses "~" separator to avoid collision with grandchild "." indexing
// (e.g. steps.foo.0_ms for loop iterations).
// The seen map is updated in place.
func childStepKey(name string, seen map[string]int) string {
	n := seen[name]
	seen[name] = n + 1
	if n == 0 {
		return name
	}
	return fmt.Sprintf("%s~%d", name, n)
}

// LoopSpan wraps a Span that groups loop iterations. Each call to Iteration
// creates a child span representing one pass through the loop.
//
// Usage:
//
//	ctx, loop := perf.StartLoop(ctx, "process_sessions")
//	for _, item := range items {
//	    iterCtx, iterSpan := loop.Iteration(ctx)
//	    doWork(iterCtx, item)
//	    iterSpan.End()
//	}
//	loop.End()
type LoopSpan struct {
	span *Span
}

// StartLoop begins a new loop span. The returned context contains the loop span
// and should be passed to Iteration. Call loop.End() after the loop completes.
func StartLoop(ctx context.Context, name string, attrs ...slog.Attr) (context.Context, *LoopSpan) {
	ctx, s := Start(ctx, name, attrs...)
	return ctx, &LoopSpan{span: s}
}

// Iteration creates a child span for a single loop iteration. The caller must
// call End() on the returned span when the iteration completes.
func (l *LoopSpan) Iteration(ctx context.Context) (context.Context, *Span) {
	ctx, s := Start(ctx, l.span.name)
	s.isLoopIter = true
	return ctx, s
}

// End completes the loop span, auto-ending any unended iteration children first
// so their durations are captured at loop-end time rather than deferring to the
// root span's End() (which may run much later).
func (l *LoopSpan) End() {
	for _, child := range l.span.children {
		if !child.ended {
			child.End()
		}
	}
	l.span.End()
}
