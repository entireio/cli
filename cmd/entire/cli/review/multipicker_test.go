package review_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/review"
)

// TestPickAgents_TooFewEligibleReturnsError verifies that calling PickAgents
// with fewer than 2 choices returns an error — it is the caller's
// responsibility to route single-agent flows through the single-agent path.
func TestPickAgents_TooFewEligibleReturnsError(t *testing.T) {
	t.Parallel()
	_, err := review.PickAgents(context.Background(), []review.AgentChoice{
		{Name: "claude-code", Label: "claude-code (1 skill configured)"},
	})
	if err == nil {
		t.Fatal("expected error for single-element eligible list")
	}
	// Must NOT be ErrPickerCancelled or ErrNoAgentsSelected — it's a caller
	// contract violation, not a user action.
	if errors.Is(err, review.ErrPickerCancelled) {
		t.Errorf("should not return ErrPickerCancelled for too-few-eligible")
	}
	if errors.Is(err, review.ErrNoAgentsSelected) {
		t.Errorf("should not return ErrNoAgentsSelected for too-few-eligible")
	}
}

// TestPickAgents_EmptyEligibleReturnsError covers the zero-length case.
func TestPickAgents_EmptyEligibleReturnsError(t *testing.T) {
	t.Parallel()
	_, err := review.PickAgents(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty eligible list")
	}
	if errors.Is(err, review.ErrPickerCancelled) || errors.Is(err, review.ErrNoAgentsSelected) {
		t.Errorf("wrong error sentinel for empty eligible: %v", err)
	}
}

// TestPickAgents_CancelledContextReturnsPickerCancelled verifies that a
// pre-cancelled context causes PickAgents to return ErrPickerCancelled
// (not a raw context.Canceled). The huh RunWithContext method returns an
// error for a cancelled context, which PickAgents maps to ErrPickerCancelled.
func TestPickAgents_CancelledContextReturnsPickerCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling PickAgents

	_, err := review.PickAgents(ctx, []review.AgentChoice{
		{Name: "claude-code", Label: "claude-code (1 skill configured)"},
		{Name: "codex", Label: "codex (2 skills configured)"},
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, review.ErrPickerCancelled) {
		t.Errorf("expected ErrPickerCancelled, got: %v", err)
	}
}

// TestPickedAgentsSentinels verifies the exported error sentinels are distinct
// values so callers can distinguish them cleanly.
func TestPickedAgentsSentinels(t *testing.T) {
	t.Parallel()
	if errors.Is(review.ErrPickerCancelled, review.ErrNoAgentsSelected) {
		t.Error("ErrPickerCancelled and ErrNoAgentsSelected must be distinct")
	}
	if errors.Is(review.ErrNoAgentsSelected, review.ErrPickerCancelled) {
		t.Error("ErrNoAgentsSelected and ErrPickerCancelled must be distinct")
	}
}

func TestAgentMultiSelectRendersAllEligibleAgents(t *testing.T) {
	t.Parallel()

	var picked []string
	field := review.ExposedBuildAgentMultiSelect([]huh.Option[string]{
		huh.NewOption("claude-code   (3 skills configured)", "claude-code").Selected(true),
		huh.NewOption("codex   (1 skills configured)", "codex").Selected(true),
	}, &picked).WithWidth(80)
	field.Focus()

	view := field.View()
	for _, want := range []string{"claude-code", "codex"} {
		if !strings.Contains(view, want) {
			t.Fatalf("agent picker did not render %q:\n%s", want, view)
		}
	}
}
