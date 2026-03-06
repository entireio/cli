package perf

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStart_CreatesRootSpan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, span := Start(ctx, "test_op")
	if span == nil {
		t.Fatal("Start() returned nil span")
	}
	if span.name != "test_op" {
		t.Errorf("span.name = %q, want %q", span.name, "test_op")
	}
	if span.parent != nil {
		t.Error("root span should have nil parent")
	}

	got := spanFromContext(ctx)
	if got != span {
		t.Error("spanFromContext should return the span set by Start")
	}

	span.End()
}

func TestStart_NestsChildSpan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "parent")
	_, child := Start(ctx, "child")

	if child.parent != parent {
		t.Error("child span should reference parent")
	}
	if len(parent.children) != 1 {
		t.Fatalf("parent should have 1 child, got %d", len(parent.children))
	}
	if parent.children[0] != child {
		t.Error("parent.children[0] should be the child span")
	}

	child.End()
	parent.End()
}

func TestEnd_RecordsDuration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, span := Start(ctx, "timed_op")
	time.Sleep(10 * time.Millisecond)
	span.End()

	if span.duration < 10*time.Millisecond {
		t.Errorf("span.duration = %v, want >= 10ms", span.duration)
	}
}

func TestEnd_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, span := Start(ctx, "double_end")
	time.Sleep(10 * time.Millisecond)
	span.End()

	firstDuration := span.duration

	time.Sleep(10 * time.Millisecond)
	span.End()

	if span.duration != firstDuration {
		t.Errorf("second End() changed duration from %v to %v", firstDuration, span.duration)
	}
}

func TestEnd_AutoEndsChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "parent")
	_, child := Start(ctx, "child")

	time.Sleep(10 * time.Millisecond)
	parent.End()

	if !child.ended {
		t.Error("child should be auto-ended when parent ends")
	}
	if child.duration == 0 {
		t.Error("child should have non-zero duration after auto-end")
	}
}

func TestSpanFromContext_ReturnsNilWhenEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if got := spanFromContext(ctx); got != nil {
		t.Errorf("spanFromContext on empty context = %v, want nil", got)
	}
}

func TestStart_MultipleChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "parent")

	ctx2, child1 := Start(ctx, "child1")
	_ = ctx2
	child1.End()

	ctx3, child2 := Start(ctx, "child2")
	_ = ctx3
	child2.End()

	parent.End()

	if len(parent.children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(parent.children))
	}
	if parent.children[0].name != "child1" {
		t.Errorf("first child name = %q, want %q", parent.children[0].name, "child1")
	}
	if parent.children[1].name != "child2" {
		t.Errorf("second child name = %q, want %q", parent.children[1].name, "child2")
	}
}

func TestRecordError_MarksSpanAsErrored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, span := Start(ctx, "op")
	testErr := errors.New("something failed")
	span.RecordError(testErr)

	if !errors.Is(span.err, testErr) {
		t.Errorf("span.err = %v, want %v", span.err, testErr)
	}

	span.End()
}

func TestRecordError_NilIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, span := Start(ctx, "op")
	span.RecordError(nil)

	if span.err != nil {
		t.Errorf("span.err = %v, want nil", span.err)
	}

	span.End()
}

func TestRecordError_FirstErrorWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, span := Start(ctx, "op")
	firstErr := errors.New("first")
	secondErr := errors.New("second")

	span.RecordError(firstErr)
	span.RecordError(secondErr)

	if !errors.Is(span.err, firstErr) {
		t.Errorf("span.err = %v, want %v (first error)", span.err, firstErr)
	}

	span.End()
}

func TestRecordError_ChildErrorFlagInOutput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "parent")
	_, child := Start(ctx, "failing_step")

	child.RecordError(errors.New("step failed"))
	child.End()
	parent.End()

	// Verify the child has the error recorded
	if child.err == nil {
		t.Error("child span should have error recorded")
	}
	// The error flag will appear as "steps.failing_step_err: true" in log output.
	// We verify the span state directly since log output goes to slog.
	if parent.children[0].err == nil {
		t.Error("parent's child should have error recorded")
	}
}

func TestEnd_NoErrorFlagByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "parent")
	_, child := Start(ctx, "ok_step")

	child.End()
	parent.End()

	if child.err != nil {
		t.Errorf("child span should have nil error, got %v", child.err)
	}
	if parent.err != nil {
		t.Errorf("parent span should have nil error, got %v", parent.err)
	}
}
