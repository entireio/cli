package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTokenUsage_SubagentTokensCompleteRoundTripAndClear(t *testing.T) {
	t.Parallel()

	complete := true
	usage := &TokenUsage{
		InputTokens:            3,
		SubagentTokens:         &TokenUsage{OutputTokens: 2},
		SubagentTokensComplete: &complete,
	}
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped TokenUsage
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if roundTripped.SubagentTokensComplete == nil || !*roundTripped.SubagentTokensComplete {
		t.Fatalf("round trip completeness = %v, want true", roundTripped.SubagentTokensComplete)
	}

	cleared := WithClearedSubagentTokens(&roundTripped, false)
	if cleared == &roundTripped || cleared.SubagentTokens != nil || cleared.SubagentTokensComplete == nil || *cleared.SubagentTokensComplete {
		t.Fatalf("cleared usage = %+v, want independent explicitly incomplete copy", cleared)
	}
	if roundTripped.SubagentTokens == nil || roundTripped.SubagentTokensComplete == nil || !*roundTripped.SubagentTokensComplete {
		t.Fatalf("clear mutated input: %+v", roundTripped)
	}

	usageCopy := AddTokenUsage(nil, usage)
	if usageCopy.SubagentTokensComplete == nil || !*usageCopy.SubagentTokensComplete {
		t.Fatalf("AddTokenUsage copy dropped completeness: %+v", usageCopy)
	}
}

func TestAddTokenUsage(t *testing.T) {
	t.Parallel()

	if got := AddTokenUsage(nil, nil); got != nil {
		t.Errorf("AddTokenUsage(nil, nil) = %+v, want nil", got)
	}

	only := &TokenUsage{InputTokens: 3}
	if got := AddTokenUsage(nil, only); got == nil || got.InputTokens != 3 {
		t.Errorf("AddTokenUsage(nil, x) = %+v, want a copy of x", got)
	}
	if got := AddTokenUsage(only, nil); got == only {
		t.Error("AddTokenUsage must not return an input pointer (would alias caller state)")
	}

	a := &TokenUsage{InputTokens: 1, OutputTokens: 2, APICallCount: 1, SubagentTokens: &TokenUsage{InputTokens: 10}}
	b := &TokenUsage{InputTokens: 4, OutputTokens: 5, APICallCount: 2, SubagentTokens: &TokenUsage{InputTokens: 20}}
	got := AddTokenUsage(a, b)
	if got.InputTokens != 5 || got.OutputTokens != 7 || got.APICallCount != 3 {
		t.Errorf("top-level sum = %+v", got)
	}
	if got.SubagentTokens == nil || got.SubagentTokens.InputTokens != 30 {
		t.Errorf("subagent sum = %+v, want InputTokens 30", got.SubagentTokens)
	}
	if a.InputTokens != 1 || a.SubagentTokens.InputTokens != 10 {
		t.Error("AddTokenUsage mutated an input")
	}
}

func TestAddTokenUsage_CompletenessCombination(t *testing.T) {
	t.Parallel()

	complete := true
	incomplete := false
	for _, operands := range [][2]*TokenUsage{
		{{SubagentTokensComplete: &complete}, {SubagentTokensComplete: &incomplete}},
		{{SubagentTokensComplete: &incomplete}, {SubagentTokensComplete: &complete}},
	} {
		got := AddTokenUsage(operands[0], operands[1])
		if got.SubagentTokensComplete == nil || *got.SubagentTokensComplete {
			t.Fatalf("AddTokenUsage(%v, %v) completeness = %v, want false", *operands[0].SubagentTokensComplete, *operands[1].SubagentTokensComplete, got.SubagentTokensComplete)
		}
	}

	unknown := &TokenUsage{}
	for _, operands := range [][2]*TokenUsage{
		{{SubagentTokensComplete: &complete}, unknown},
		{unknown, {SubagentTokensComplete: &complete}},
	} {
		if got := AddTokenUsage(operands[0], operands[1]); got.SubagentTokensComplete != nil {
			t.Fatalf("AddTokenUsage with unknown coverage = %v, want nil", *got.SubagentTokensComplete)
		}
	}
}

// TestAddTokenUsage_TruncatesDeepSubagentChains pins MaxSubagentDepth. Token usage
// is read back from per-session metadata.json blobs on the shared checkpoint
// branch, so the chain depth is not trustworthy; an unbounded chain reaching the
// root CheckpointSummary is a write-amplification vector, because that summary is
// re-marshalled with indentation (O(depth²) in output size).
func TestAddTokenUsage_TruncatesDeepSubagentChains(t *testing.T) {
	t.Parallel()

	// Build a chain far deeper than any real agent reports (real chains are depth 1).
	deep := &TokenUsage{InputTokens: 1}
	for range MaxSubagentDepth * 3 {
		deep = &TokenUsage{InputTokens: 1, SubagentTokens: deep}
	}

	depth := 0
	for got := AddTokenUsage(deep, deep); got != nil; got = got.SubagentTokens {
		depth++
		if depth > MaxSubagentDepth*2 {
			t.Fatalf("chain not truncated: walked %d levels", depth)
		}
	}
	if depth != MaxSubagentDepth+1 {
		t.Errorf("result depth = %d, want %d (MaxSubagentDepth + the top level)", depth, MaxSubagentDepth+1)
	}
}

// TestAddTokenUsage_KeepsRealDepthIntact is the companion guard: the cap must not
// clip the depth-1 chains agents actually produce.
func TestAddTokenUsage_KeepsRealDepthIntact(t *testing.T) {
	t.Parallel()

	got := AddTokenUsage(
		&TokenUsage{InputTokens: 1, SubagentTokens: &TokenUsage{InputTokens: 10}},
		&TokenUsage{InputTokens: 2, SubagentTokens: &TokenUsage{InputTokens: 20}},
	)
	if got.SubagentTokens == nil || got.SubagentTokens.InputTokens != 30 {
		t.Fatalf("subagent total = %+v, want InputTokens 30", got.SubagentTokens)
	}
	if got.SubagentTokens.SubagentTokens != nil {
		t.Error("must not synthesize a nested level that the inputs did not have")
	}
}

// Model on a subagent entry lets cost be weighted per model. A sum on one
// model keeps it; a sum across models (an unrecorded model counts as its own)
// clears it and records the split in ByModel, so no tokens are attributed to a
// model that did not produce them.
func TestAddTokenUsage_PerModel(t *testing.T) {
	t.Parallel()

	haiku := &TokenUsage{InputTokens: 10, OutputTokens: 4, APICallCount: 1, Model: "claude-haiku-4-5"}
	opus := &TokenUsage{InputTokens: 100, OutputTokens: 40, APICallCount: 2, Model: "claude-opus-4-8"}
	unknown := &TokenUsage{InputTokens: 7, OutputTokens: 3, APICallCount: 1}

	same := AddTokenUsage(haiku, haiku)
	if same.Model != "claude-haiku-4-5" || same.ByModel != nil {
		t.Errorf("same model: Model %q ByModel %v, want kept and no split", same.Model, same.ByModel)
	}
	if got := AddTokenUsage(nil, haiku); got.Model != "claude-haiku-4-5" {
		t.Errorf("copy of one operand must keep Model, got %q", got.Model)
	}
	if got := AddTokenUsage(&TokenUsage{}, haiku); got.Model != "claude-haiku-4-5" || got.ByModel != nil {
		t.Errorf("a zero seed must not add an unrecorded bucket: Model %q ByModel %v", got.Model, got.ByModel)
	}

	mixed := AddTokenUsage(AddTokenUsage(opus, haiku), AddTokenUsage(unknown, haiku))
	if mixed.Model != "" {
		t.Errorf("mixed models must clear Model, got %q", mixed.Model)
	}
	want := []TokenUsage{
		{InputTokens: 7, OutputTokens: 3, APICallCount: 1},
		{InputTokens: 20, OutputTokens: 8, APICallCount: 2, Model: "claude-haiku-4-5"},
		{InputTokens: 100, OutputTokens: 40, APICallCount: 2, Model: "claude-opus-4-8"},
	}
	if !reflect.DeepEqual(mixed.ByModel, want) {
		t.Errorf("ByModel = %+v, want %+v", mixed.ByModel, want)
	}
	if mixed.InputTokens != 127 || mixed.OutputTokens != 51 || mixed.APICallCount != 5 {
		t.Errorf("totals = %+v, want the sum of every model", mixed)
	}

	// The split survives a later sum with a single-model entry.
	again := AddTokenUsage(mixed, opus)
	if len(again.ByModel) != 3 || again.ByModel[2].InputTokens != 200 {
		t.Errorf("ByModel after a further add = %+v", again.ByModel)
	}

	// Nested subagent levels keep their own split.
	nested := AddTokenUsage(&TokenUsage{SubagentTokens: haiku}, &TokenUsage{SubagentTokens: opus})
	if nested.SubagentTokens == nil || len(nested.SubagentTokens.ByModel) != 2 {
		t.Errorf("subagent level = %+v, want a two-model split", nested.SubagentTokens)
	}
}

// A delta between two cumulative snapshots keeps only the models that spent
// tokens in between.
func TestSubtractTokenUsage_PerModel(t *testing.T) {
	t.Parallel()

	baseline := &TokenUsage{InputTokens: 10, OutputTokens: 4, APICallCount: 1, Model: "claude-haiku-4-5"}
	later := AddTokenUsage(baseline, &TokenUsage{InputTokens: 50, OutputTokens: 20, APICallCount: 1, Model: "claude-opus-4-8"})

	diff := SubtractTokenUsage(later, baseline)
	if diff.Model != "claude-opus-4-8" || diff.ByModel != nil {
		t.Errorf("delta spent only on opus: Model %q ByModel %+v", diff.Model, diff.ByModel)
	}
	if diff.InputTokens != 50 || diff.APICallCount != 1 {
		t.Errorf("delta = %+v", diff)
	}

	// A checkpoint with no new subagent tokens must not name a model.
	if got := SubtractTokenUsage(baseline, baseline); got.Model != "" || got.ByModel != nil {
		t.Errorf("an empty delta carries no label, got Model %q ByModel %+v", got.Model, got.ByModel)
	}
}

// A baseline that spent tokens on a model the snapshot lacks (one captured
// before models were recorded, or after a child's transcript went away) says
// nothing about which of the snapshot's models the delta belongs to, so the
// delta is left unlabelled rather than credited to a guessed model.
func TestSubtractTokenUsage_UnmatchedBaselineLeavesDeltaUnlabelled(t *testing.T) {
	t.Parallel()

	snapshot := AddTokenUsage(
		&TokenUsage{InputTokens: 80, APICallCount: 3, Model: "claude-opus-4-8"},
		&TokenUsage{InputTokens: 50, APICallCount: 2, Model: "claude-haiku-4-5"},
	)
	legacyBaseline := &TokenUsage{InputTokens: 100, APICallCount: 4} // no model recorded

	diff := SubtractTokenUsage(snapshot, legacyBaseline)
	want := &TokenUsage{InputTokens: 30, APICallCount: 1}
	if !reflect.DeepEqual(diff, want) {
		t.Errorf("delta = %+v, want unlabelled %+v", diff, want)
	}

	vanished := &TokenUsage{InputTokens: 20, APICallCount: 1, Model: "gpt-5.5"}
	diff = SubtractTokenUsage(snapshot, AddTokenUsage(vanished, &TokenUsage{InputTokens: 10, APICallCount: 1, Model: "claude-opus-4-8"}))
	if diff.Model != "" || diff.ByModel != nil || diff.InputTokens != 100 {
		t.Errorf("delta against a vanished model = %+v, want 100 unlabelled", diff)
	}
}

// When the same model shrank (a cumulative count went backwards), the excess
// is drained from the snapshot's other models so the split still sums to the
// clamped delta.
func TestSubtractTokenUsage_ShrunkModelDrainsOtherModels(t *testing.T) {
	t.Parallel()

	snapshot := AddTokenUsage(
		&TokenUsage{InputTokens: 80, APICallCount: 3, Model: "claude-opus-4-8"},
		&TokenUsage{InputTokens: 50, APICallCount: 2, Model: "claude-haiku-4-5"},
	)
	baseline := &TokenUsage{InputTokens: 100, APICallCount: 4, Model: "claude-opus-4-8"}

	diff := SubtractTokenUsage(snapshot, baseline)
	want := &TokenUsage{InputTokens: 30, APICallCount: 1, Model: "claude-haiku-4-5"}
	if !reflect.DeepEqual(diff, want) {
		t.Errorf("delta = %+v, want %+v", diff, want)
	}
}

// Usage is read back from pushed blobs. A split that does not sum to its
// entry, sits beside a Model, or is longer than MaxModelsPerUsage is not one
// this package wrote, so the entry is treated as counts under no model.
func TestAddTokenUsage_DistrustsMalformedSplit(t *testing.T) {
	t.Parallel()

	tooMany := &TokenUsage{InputTokens: MaxModelsPerUsage + 1}
	for i := range MaxModelsPerUsage + 1 {
		tooMany.ByModel = append(tooMany.ByModel, TokenUsage{InputTokens: 1, Model: string(rune('a' + i))})
	}
	for name, blob := range map[string]*TokenUsage{
		"overclaimed":     {InputTokens: 10, ByModel: []TokenUsage{{InputTokens: 500, Model: "a"}, {InputTokens: 1, Model: "b"}}},
		"underclaimed":    {InputTokens: 10, ByModel: []TokenUsage{{InputTokens: 4, Model: "a"}, {InputTokens: 1, Model: "b"}}},
		"negative":        {InputTokens: 10, ByModel: []TokenUsage{{InputTokens: 500, Model: "a"}, {InputTokens: -490, Model: "b"}}},
		"model and split": {InputTokens: 10, Model: "a", ByModel: []TokenUsage{{InputTokens: 4, Model: "a"}, {InputTokens: 6, Model: "b"}}},
		"too many":        tooMany,
	} {
		got := AddTokenUsage(blob, &TokenUsage{InputTokens: 1, Model: "c"})
		if got.InputTokens != blob.InputTokens+1 {
			t.Errorf("%s: total = %d, want %d", name, got.InputTokens, blob.InputTokens+1)
		}
		want := []TokenUsage{{InputTokens: blob.InputTokens}, {InputTokens: 1, Model: "c"}}
		if got.Model != "" || !reflect.DeepEqual(got.ByModel, want) {
			t.Errorf("%s: Model %q ByModel %+v, want the blob as unlabelled counts %+v", name, got.Model, got.ByModel, want)
		}
	}

	valid := &TokenUsage{InputTokens: 10, ByModel: []TokenUsage{{InputTokens: 4, Model: "a"}, {InputTokens: 6, Model: "b"}}}
	if got := AddTokenUsage(valid, nil); !reflect.DeepEqual(got.ByModel, valid.ByModel) {
		t.Errorf("a consistent split must be kept, got %+v", got.ByModel)
	}
}

// ByModel entries are flat: nested fields a hostile or malformed entry carries
// are dropped, so the split cannot reintroduce a deep chain.
func TestAddTokenUsage_ByModelEntriesStayFlat(t *testing.T) {
	t.Parallel()

	hostile := &TokenUsage{ByModel: []TokenUsage{
		{InputTokens: 1, Model: "a", SubagentTokens: &TokenUsage{InputTokens: 99}, ByModel: []TokenUsage{{InputTokens: 99}}},
		{InputTokens: 2, Model: "b"},
	}}
	got := AddTokenUsage(hostile, nil)
	for _, e := range got.ByModel {
		if e.SubagentTokens != nil || e.ByModel != nil {
			t.Fatalf("ByModel entry kept nested fields: %+v", e)
		}
	}
}

// The JSON names are shared with the tokens rewrite (#2155), so the two
// compose: "model" and "by_model", both omitted when unset.
func TestTokenUsage_ModelJSONNames(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(&TokenUsage{Model: "m", ByModel: []TokenUsage{{Model: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["model"]; !ok {
		t.Errorf("missing \"model\" in %s", data)
	}
	if _, ok := fields["by_model"]; !ok {
		t.Errorf("missing \"by_model\" in %s", data)
	}
	plain, err := json.Marshal(&TokenUsage{InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	clear(fields)
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["model"]; ok {
		t.Errorf("unset Model must be omitted: %s", plain)
	}
}

// LabelModel gives a hook-reported model to usage that records none of its
// own, and never relabels usage that already says which model produced it.
func TestLabelModel(t *testing.T) {
	t.Parallel()

	plain := &TokenUsage{InputTokens: 5, APICallCount: 1}
	got := LabelModel(plain, "claude-haiku-4-5")
	if got.Model != "claude-haiku-4-5" || got.InputTokens != 5 {
		t.Errorf("unlabelled usage = %+v, want the model added", got)
	}
	if plain.Model != "" {
		t.Error("LabelModel must not mutate its input")
	}
	if LabelModel(nil, "m") != nil {
		t.Error("nil usage stays nil: no tokens is not zero tokens")
	}
	if got := LabelModel(&TokenUsage{InputTokens: 1, Model: "own"}, "hook"); got.Model != "own" {
		t.Errorf("an entry's own model wins, got %q", got.Model)
	}
	split := AddTokenUsage(&TokenUsage{InputTokens: 1, Model: "a"}, &TokenUsage{InputTokens: 2, Model: "b"})
	if got := LabelModel(split, "hook"); got.Model != "" || len(got.ByModel) != 2 {
		t.Errorf("a split must not collapse to one model, got %+v", got)
	}
}
