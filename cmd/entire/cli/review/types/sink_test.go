package types

import (
	"testing"
	"time"
)

func TestAgentStatus_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status AgentStatus
		want   string
	}{
		{AgentStatusSucceeded, "succeeded"},
		{AgentStatusFailed, "failed"},
		{AgentStatusCancelled, "cancelled"},
		{AgentStatusUnknown, "unknown"},
		{AgentStatus(99), "unknown"}, // any unrecognised value falls to default
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.status.String(); got != tc.want {
				t.Errorf("AgentStatus(%d).String() = %q, want %q", int(tc.status), got, tc.want)
			}
		})
	}
}

func TestRunSummary_ZeroValueIsValid(t *testing.T) {
	t.Parallel()
	// Zero value should be usable without any constructor — mirrors
	// TestRunConfigZeroValueIsValid in reviewer_test.go.
	var s RunSummary
	if s.AgentRuns != nil {
		t.Errorf("zero RunSummary.AgentRuns should be nil, got %v", s.AgentRuns)
	}
	if !s.StartedAt.IsZero() {
		t.Errorf("zero RunSummary.StartedAt should be zero time, got %v", s.StartedAt)
	}
	if !s.FinishedAt.IsZero() {
		t.Errorf("zero RunSummary.FinishedAt should be zero time, got %v", s.FinishedAt)
	}
	if s.Cancelled {
		t.Errorf("zero RunSummary.Cancelled should be false")
	}
}

// stubSink is a minimal Sink used only for the compile-time check below.
type stubSink struct{}

func (stubSink) AgentEvent(_ string, _ Event) {}
func (stubSink) RunFinished(_ RunSummary)     {}

// Compile-time assertion: stubSink must satisfy Sink.
var _ Sink = stubSink{}

func TestSinkInterface_CompileTimeCheck(t *testing.T) {
	t.Parallel()
	// The var _ declaration above is the real check; this test ensures the
	// file is compiled (and keeps the stub from being dead code in tests).
	var s Sink = stubSink{}
	s.AgentEvent("agent", Started{})
	s.RunFinished(RunSummary{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
}
