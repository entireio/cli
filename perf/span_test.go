package perf

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStart_CreatesRootSpan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, span := Start(ctx, "test_op")
	require.NotNil(t, span, "Start() returned nil span")
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

func TestChildStepKey_Deduplication(t *testing.T) {
	t.Parallel()

	const (
		stepCheck  = "check_content"
		stepSave   = "save_state"
		stepUnique = "unique_step"
	)

	seen := make(map[string]int)

	// First occurrence keeps original name
	if got := childStepKey(stepCheck, seen); got != stepCheck {
		t.Errorf("first check_content = %q, want %q", got, stepCheck)
	}
	if got := childStepKey(stepSave, seen); got != stepSave {
		t.Errorf("first save_state = %q, want %q", got, stepSave)
	}

	// Second occurrence gets ~1 suffix
	if got := childStepKey(stepCheck, seen); got != "check_content~1" {
		t.Errorf("second check_content = %q, want %q", got, "check_content~1")
	}
	if got := childStepKey(stepSave, seen); got != "save_state~1" {
		t.Errorf("second save_state = %q, want %q", got, "save_state~1")
	}

	// Third occurrence gets ~2
	if got := childStepKey(stepCheck, seen); got != "check_content~2" {
		t.Errorf("third check_content = %q, want %q", got, "check_content~2")
	}

	// Unique names are unaffected
	if got := childStepKey(stepUnique, seen); got != stepUnique {
		t.Errorf("unique_step = %q, want %q", got, stepUnique)
	}
}

func TestEnd_DuplicateChildNames_AllPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, parent := Start(ctx, "post-commit")

	// Simulate a loop creating children with the same names (e.g., per-session spans)
	_, c1 := Start(ctx, "check_content")
	c1.duration = 3000 * time.Millisecond
	c1.ended = true

	_, c2 := Start(ctx, "save_state")
	c2.duration = 5 * time.Millisecond
	c2.ended = true

	// Second iteration — same names
	_, c3 := Start(ctx, "check_content")
	c3.duration = 0
	c3.ended = true
	c3.err = errors.New("test error")

	_, c4 := Start(ctx, "save_state")
	c4.duration = 2 * time.Millisecond
	c4.ended = true

	parent.End()

	if len(parent.children) != 4 {
		t.Fatalf("expected 4 children, got %d", len(parent.children))
	}

	// Verify the children have correct names for deduplication.
	// End() uses childStepKey which gives: check_content, save_state, check_content~1, save_state~1
	// We verify the structure is intact so End() can produce unique keys.
	names := make([]string, len(parent.children))
	for i, c := range parent.children {
		names[i] = c.name
	}
	// All 4 children exist with their original names (dedup happens at serialization)
	if names[0] != "check_content" || names[1] != "save_state" ||
		names[2] != "check_content" || names[3] != "save_state" {
		t.Errorf("children names = %v, expected [check_content save_state check_content save_state]", names)
	}
}

func TestEnd_SerializesLoopIterationsWithNumericKeys(t *testing.T) {
	t.Parallel()

	iter0 := testEndedSpan("process_sessions", 100*time.Millisecond)
	iter0.isLoopIter = true
	iter1 := testEndedSpan("process_sessions", 200*time.Millisecond)
	iter1.isLoopIter = true
	processSessions := testEndedSpan("process_sessions", 300*time.Millisecond, iter0, iter1)
	root := testEndedSpan("post-commit", 350*time.Millisecond, processSessions)

	attrs := testAttrMap(t, appendChildStepAttrs(nil, root, ""))

	require.Equal(t, int64(300), attrs["steps.process_sessions_ms"])
	require.Equal(t, int64(100), attrs["steps.process_sessions.0_ms"])
	require.Equal(t, int64(200), attrs["steps.process_sessions.1_ms"])
}

func TestEnd_SerializesNestedNamedSpansWithNames(t *testing.T) {
	t.Parallel()

	readSession := testEndedSpan("why_checkpoint_read_session", 120*time.Millisecond)
	checkpointInfo := testEndedSpan("why_checkpoint_info", 200*time.Millisecond, readSession)
	enrichCommits := testEndedSpan("enrich_commits", 300*time.Millisecond, checkpointInfo)
	root := testEndedSpan("why", 500*time.Millisecond, enrichCommits)

	attrs := testAttrMap(t, appendChildStepAttrs(nil, root, ""))

	require.Equal(t, int64(300), attrs["steps.enrich_commits_ms"])
	require.Equal(t, int64(200), attrs["steps.enrich_commits.why_checkpoint_info_ms"])
	require.Equal(t, int64(120), attrs["steps.enrich_commits.why_checkpoint_info.why_checkpoint_read_session_ms"])
	require.NotContains(t, attrs, "steps.enrich_commits.0_ms")
}

func TestEnd_SerializesDuplicateNestedSiblingNames(t *testing.T) {
	t.Parallel()

	firstInfo := testEndedSpan("why_checkpoint_info", 100*time.Millisecond)
	secondInfo := testEndedSpan("why_checkpoint_info", 150*time.Millisecond)
	enrichCommits := testEndedSpan("enrich_commits", 300*time.Millisecond, firstInfo, secondInfo)
	root := testEndedSpan("why", 500*time.Millisecond, enrichCommits)

	attrs := testAttrMap(t, appendChildStepAttrs(nil, root, ""))

	require.Equal(t, int64(100), attrs["steps.enrich_commits.why_checkpoint_info_ms"])
	require.Equal(t, int64(150), attrs["steps.enrich_commits.why_checkpoint_info~1_ms"])
}

func TestEnd_SerializesNestedErrorFlags(t *testing.T) {
	t.Parallel()

	readSession := testEndedSpan("why_checkpoint_read_session", 120*time.Millisecond)
	readSession.err = errors.New("read session failed")
	checkpointInfo := testEndedSpan("why_checkpoint_info", 200*time.Millisecond, readSession)
	checkpointInfo.err = errors.New("checkpoint info failed")
	enrichCommits := testEndedSpan("enrich_commits", 300*time.Millisecond, checkpointInfo)
	root := testEndedSpan("why", 500*time.Millisecond, enrichCommits)

	attrs := testAttrMap(t, appendChildStepAttrs(nil, root, ""))

	require.Equal(t, true, attrs["steps.enrich_commits.why_checkpoint_info_err"])
	require.Equal(t, true, attrs["steps.enrich_commits.why_checkpoint_info.why_checkpoint_read_session_err"])
}

func testEndedSpan(name string, duration time.Duration, children ...*Span) *Span {
	span := &Span{
		name:     name,
		duration: duration,
		ended:    true,
		children: children,
	}
	for _, child := range children {
		child.parent = span
	}
	return span
}

func testAttrMap(t *testing.T, attrs []any) map[string]any {
	t.Helper()

	out := make(map[string]any, len(attrs))
	for _, raw := range attrs {
		attr, ok := raw.(slog.Attr)
		if !ok {
			t.Fatalf("attr has type %T, want slog.Attr", raw)
		}
		out[attr.Key] = attr.Value.Any()
	}
	return out
}

func TestStartLoop_CreatesGroupSpan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, root := Start(ctx, "post-commit")

	loopCtx, loop := StartLoop(ctx, "process_sessions")
	_, iter0 := loop.Iteration(loopCtx)
	iter0.duration = 100 * time.Millisecond
	iter0.ended = true
	_, iter1 := loop.Iteration(loopCtx)
	iter1.duration = 200 * time.Millisecond
	iter1.ended = true
	loop.span.duration = 300 * time.Millisecond
	loop.span.ended = true

	root.End()

	// Verify loop span has 2 children (the iterations)
	if len(loop.span.children) != 2 {
		t.Fatalf("loop should have 2 children, got %d", len(loop.span.children))
	}
	if loop.span.children[0] != iter0 {
		t.Error("loop.children[0] should be iter0")
	}
	if loop.span.children[1] != iter1 {
		t.Error("loop.children[1] should be iter1")
	}
}

func TestStartLoop_IterationsAutoEnded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, root := Start(ctx, "hook")
	loopCtx, loop := StartLoop(ctx, "loop")
	_, iter0 := loop.Iteration(loopCtx)
	iter0.duration = 50 * time.Millisecond
	// iter0.ended intentionally NOT set — should be auto-ended when loop.End() runs
	loop.End()

	if !iter0.ended {
		t.Error("iteration should be auto-ended when loop.End() runs")
	}

	root.End()
}

func TestStartLoop_IterationWithErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ctx, root := Start(ctx, "hook")
	loopCtx, loop := StartLoop(ctx, "process_sessions")
	_, iter0 := loop.Iteration(loopCtx)
	iter0.duration = 100 * time.Millisecond
	iter0.ended = true
	_, iter1 := loop.Iteration(loopCtx)
	iter1.duration = 200 * time.Millisecond
	iter1.RecordError(errors.New("session failed"))
	iter1.ended = true
	loop.span.duration = 300 * time.Millisecond
	loop.span.ended = true

	root.End()

	if iter0.err != nil {
		t.Error("iter0 should have no error")
	}
	if iter1.err == nil {
		t.Error("iter1 should have error recorded")
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
