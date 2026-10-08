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
