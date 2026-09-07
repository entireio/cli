package review

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func TestSlotActionOptionsOnlyModelRemoveCancel(t *testing.T) {
	t.Parallel()
	options := slotActionOptions()
	keys := make([]string, 0, len(options))
	values := make([]string, 0, len(options))
	for _, opt := range options {
		keys = append(keys, opt.Key)
		values = append(values, opt.Value)
	}
	wantKeys := []string{"Change model", "Remove", "Cancel"}
	wantValues := []string{"model", "remove", "cancel"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("slot action labels = %v, want %v", keys, wantKeys)
	}
	if !reflect.DeepEqual(values, wantValues) {
		t.Fatalf("slot action values = %v, want %v", values, wantValues)
	}
}

func TestGuidedProfileTaskPreservesExistingCustomTask(t *testing.T) {
	t.Parallel()
	const (
		generated = "built-in generated task"
		existing  = "saved custom task"
		custom    = "new custom task"
	)
	if got := guidedProfileTask(DefaultProfileName, generated, existing, ""); got != existing {
		t.Fatalf("guidedProfileTask without new custom task = %q, want existing %q", got, existing)
	}
	if got := guidedProfileTask(DefaultProfileName, generated, existing, custom); got != custom {
		t.Fatalf("guidedProfileTask with new custom task = %q, want %q", got, custom)
	}
	if got := guidedProfileTask(DefaultProfileName, generated, "", ""); got != generated {
		t.Fatalf("guidedProfileTask without existing task = %q, want generated %q", got, generated)
	}
}

func TestReviewModelSelectOptionsPreservesCurrentCustomModel(t *testing.T) {
	t.Parallel()
	const current = "my-custom-model"
	options, picked := reviewModelSelectOptions(context.Background(), "unknown-agent", current)
	if picked != current {
		t.Fatalf("picked = %q, want current custom model %q", picked, current)
	}
	values := make(map[string]bool, len(options))
	for _, opt := range options {
		values[opt.Value] = true
	}
	if !values[reviewModelDefaultSentinel] {
		t.Fatal("default model option missing")
	}
	if !values[current] {
		t.Fatalf("current custom model option %q missing", current)
	}
	if !values[reviewModelCustomSentinel] {
		t.Fatal("custom model option missing")
	}
}

// noReviewerFor never matches any agent to a review-runner adapter, so every
// installed agent is filtered out of launchableInstalledAgentNames.
func noReviewerFor(string) reviewtypes.AgentReviewer { return nil }

// TestRunReviewGuidedSetup_NoLaunchableAgentsHintsAgentAdd calls the real
// guided-setup entry point (picker.go) with no launchable agents and asserts
// the error hints at `entire agent add`, not the stale `entire configure
// --agent` flag (issue #2249).
func TestRunReviewGuidedSetup_NoLaunchableAgentsHintsAgentAdd(t *testing.T) {
	t.Parallel()
	_, _, err := RunReviewGuidedSetup(
		context.Background(),
		&bytes.Buffer{},
		[]types.AgentName{"claude-code"},
		noReviewerFor,
		"",
		false, // firstRun=false: skips the interactive confirm form
		nil,
	)
	if err == nil {
		t.Fatal("expected error when no agents are launchable")
	}
	if !strings.Contains(err.Error(), "entire agent add") {
		t.Errorf("error should hint at `entire agent add`, got: %v", err)
	}
	if strings.Contains(err.Error(), "configure --agent") {
		t.Errorf("error should NOT hint at the stale `configure --agent` flag, got: %v", err)
	}
}

// TestDefaultReviewProfileForInstalledAgents_NoneInstalledHintsAgentAdd calls
// the real default-profile builder (profile.go) with no installed agents and
// asserts the error hints at `entire agent add`, not the stale `entire
// configure --agent` flag (issue #2249).
func TestDefaultReviewProfileForInstalledAgents_NoneInstalledHintsAgentAdd(t *testing.T) {
	t.Parallel()
	_, err := defaultReviewProfileForInstalledAgents(
		context.Background(),
		"",
		nil, // installed
		noReviewerFor,
	)
	if err == nil {
		t.Fatal("expected error when no agents are installed")
	}
	if !strings.Contains(err.Error(), "entire agent add") {
		t.Errorf("error should hint at `entire agent add`, got: %v", err)
	}
	if strings.Contains(err.Error(), "configure --agent") {
		t.Errorf("error should NOT hint at the stale `configure --agent` flag, got: %v", err)
	}
}
