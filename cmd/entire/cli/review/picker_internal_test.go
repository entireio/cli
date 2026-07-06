package review

import (
	"context"
	"reflect"
	"testing"
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
	if got := guidedProfileTask(generated, existing, ""); got != existing {
		t.Fatalf("guidedProfileTask without new custom task = %q, want existing %q", got, existing)
	}
	if got := guidedProfileTask(generated, existing, custom); got != custom {
		t.Fatalf("guidedProfileTask with new custom task = %q, want %q", got, custom)
	}
	if got := guidedProfileTask(generated, "", ""); got != generated {
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

// TestGuidedProfileTask_NoBuiltinFallbackPersisted verifies setup never bakes
// the built-in default brief into the saved profile: with no custom, existing,
// or generated task, the persisted task stays empty and the runtime fallback
// (workerTask / profileTask) supplies defaults where needed. Persisting the
// built-in text made it indistinguishable from a user-configured task, so
// skill-bearing workers kept receiving the maximal-audit brief forever.
func TestGuidedProfileTask_NoBuiltinFallbackPersisted(t *testing.T) {
	t.Parallel()
	if got := guidedProfileTask("", "", ""); got != "" {
		t.Fatalf("guidedProfileTask with nothing user-provided = %q, want empty", got)
	}
}

// TestBuildCrewProfile_NoBuiltinTaskPersisted drives the REAL guided-setup
// profile constructor — not guidedProfileTask with hand-fed empty inputs —
// and asserts it does not seed the built-in brief. This is the test the
// dogfood review flagged as missing: the previous test passed with inputs
// production never produces, while buildCrewProfile still baked the default
// task into every interactively-configured profile.
func TestBuildCrewProfile_NoBuiltinTaskPersisted(t *testing.T) {
	t.Parallel()
	profile := buildCrewProfile(context.Background(), DefaultProfileName, []crewSlot{{agent: "claude-code"}})
	if profile.Task != "" {
		t.Errorf("buildCrewProfile persisted Task %q, want empty (built-in brief is runtime fallback only)", profile.Task)
	}
}
