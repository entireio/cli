package tokenreport

import (
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// modelSonnet is the priced Anthropic model the fixtures default to.
const modelSonnet = "claude-sonnet-4-5"

// findContributor returns the contributor with the given key, failing the
// test when it is absent.
func findContributor(t *testing.T, got Attributed, kind ContributorKind, label, skill string) Contributor {
	t.Helper()
	for _, c := range got.Contributors {
		if c.Kind == kind && c.Label == label && c.Skill == skill {
			return c
		}
	}
	t.Fatalf("no contributor %s/%q/%q in %+v", kind, label, skill, got.Contributors)
	return Contributor{}
}

// hasContributor reports whether a contributor with the given key exists.
func hasContributor(got Attributed, kind ContributorKind, label, skill string) bool {
	for _, c := range got.Contributors {
		if c.Kind == kind && c.Label == label && c.Skill == skill {
			return true
		}
	}
	return false
}

func ref(id, tool, detail string) types.ToolUseRef {
	return types.ToolUseRef{ID: id, Tool: tool, Detail: detail}
}

func TestAttributeEmpty(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]*types.Attribution{
		"nil":      nil,
		"no calls": {Subagents: []types.SubagentRecord{{ToolUseID: "x", SubagentType: "Explore"}}},
	} {
		got := Attribute(in, nil)
		if got.Contributors == nil || len(got.Contributors) != 0 {
			t.Errorf("%s: contributors = %+v, want empty non-nil", name, got.Contributors)
		}
		if got.PricedUnits != 0 || len(got.Unpriced) != 0 {
			t.Errorf("%s: priced=%v unpriced=%v, want zero", name, got.PricedUnits, got.Unpriced)
		}
	}
}

func TestAttributeOutputEqualSplitAcrossEmittedTools(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{{
		Model:   modelSonnet,
		Usage:   types.TokenUsage{OutputTokens: 10, ThinkingTokens: 4, APICallCount: 1},
		Emitted: []types.ToolUseRef{ref("t1", "Bash", "go test"), ref("t2", "Read", "a.go"), ref("t3", "Grep", "")},
	}}}
	got := Attribute(a, nil)

	bash := findContributor(t, got, KindTool, "Bash", "")
	read := findContributor(t, got, KindTool, "Read", "")
	grep := findContributor(t, got, KindTool, "Grep", "")
	if bash.Usage.OutputTokens != 4 || read.Usage.OutputTokens != 3 || grep.Usage.OutputTokens != 3 {
		t.Errorf("output split = %d/%d/%d, want 4/3/3", bash.Usage.OutputTokens, read.Usage.OutputTokens, grep.Usage.OutputTokens)
	}
	// Thinking (4) and the non-thinking remainder (6) are split separately so
	// each row's thinking stays a subset of its output: 2/1/1 + 2/2/2.
	if bash.Usage.ThinkingTokens != 2 || read.Usage.ThinkingTokens != 1 || grep.Usage.ThinkingTokens != 1 {
		t.Errorf("thinking split = %d/%d/%d, want 2/1/1", bash.Usage.ThinkingTokens, read.Usage.ThinkingTokens, grep.Usage.ThinkingTokens)
	}
	if bash.Usage.APICallCount != 1 || read.Usage.APICallCount != 0 || grep.Usage.APICallCount != 0 {
		t.Errorf("call count split = %d/%d/%d, want 1/0/0", bash.Usage.APICallCount, read.Usage.APICallCount, grep.Usage.APICallCount)
	}
	for _, c := range []Contributor{bash, read, grep} {
		if c.Model != modelSonnet || c.Source != SourceTranscript {
			t.Errorf("%s: model=%q source=%q", c.Label, c.Model, c.Source)
		}
	}
	if hasContributor(got, KindText, LabelAssistantText, "") {
		t.Error("Assistant text row present although every call emitted tools")
	}
}

func TestAttributeFreshInputSplitByBytes(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{
			Usage:   types.TokenUsage{OutputTokens: 2, APICallCount: 1},
			Emitted: []types.ToolUseRef{ref("t1", "Bash", "go test"), ref("t2", "Read", "a.go")},
		},
		{
			Usage: types.TokenUsage{InputTokens: 1230, CacheCreationTokens: 123, CacheCreation1hTokens: 123, APICallCount: 1},
			Consumed: []types.ToolResultRef{
				{ToolUse: ref("t1", "Bash", "go test"), Bytes: 12000},
				{ToolUse: ref("t2", "Read", "a.go"), Bytes: 300},
			},
		},
	}}
	got := Attribute(a, nil)

	bash := findContributor(t, got, KindTool, "Bash", "")
	read := findContributor(t, got, KindTool, "Read", "")
	if bash.Usage.InputTokens != 1200 || read.Usage.InputTokens != 30 {
		t.Errorf("input split = %d/%d, want 1200/30", bash.Usage.InputTokens, read.Usage.InputTokens)
	}
	if bash.Usage.CacheCreationTokens != 120 || read.Usage.CacheCreationTokens != 3 {
		t.Errorf("cache write split = %d/%d, want 120/3", bash.Usage.CacheCreationTokens, read.Usage.CacheCreationTokens)
	}
	if bash.Usage.CacheCreation1hTokens != 120 || read.Usage.CacheCreation1hTokens != 3 {
		t.Errorf("1h split = %d/%d, want 120/3", bash.Usage.CacheCreation1hTokens, read.Usage.CacheCreation1hTokens)
	}
	// The second call's APICallCount rides with its output, which it emitted
	// no tool for → Assistant text.
	text := findContributor(t, got, KindText, LabelAssistantText, "")
	if text.Usage.APICallCount != 1 || text.Usage.InputTokens != 0 {
		t.Errorf("Assistant text = %+v, want 1 call and no input", text.Usage)
	}
	if hasContributor(got, KindPrompt, LabelPromptContext, "") {
		t.Error("prompt row present although the second call consumed results")
	}
}

func TestAttributeFreshInputEqualSplitWhenBytesUnknown(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{Emitted: []types.ToolUseRef{ref("t1", "Bash", ""), ref("t2", "Read", "")}},
		{
			Usage: types.TokenUsage{InputTokens: 7},
			Consumed: []types.ToolResultRef{
				{ToolUse: ref("t1", "Bash", "")},
				{ToolUse: ref("t2", "Read", "")},
			},
		},
	}}
	got := Attribute(a, nil)
	bash := findContributor(t, got, KindTool, "Bash", "")
	read := findContributor(t, got, KindTool, "Read", "")
	if bash.Usage.InputTokens != 4 || read.Usage.InputTokens != 3 {
		t.Errorf("input split = %d/%d, want 4/3", bash.Usage.InputTokens, read.Usage.InputTokens)
	}
}

func TestAttributeSyntheticRows(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		// First call: nothing consumed → prompt; no tools → text.
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{InputTokens: 500, CacheCreationTokens: 50, CacheReadTokens: 100, OutputTokens: 20, APICallCount: 1}},
		// Second call, again nothing consumed (a new user prompt) → prompt too.
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{InputTokens: 40, CacheReadTokens: 200, OutputTokens: 5, APICallCount: 1}},
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{CacheReadTokens: 300, OutputTokens: 1, APICallCount: 1}},
	}}
	got := Attribute(a, nil)

	prompt := findContributor(t, got, KindPrompt, LabelPromptContext, "")
	if prompt.Usage.InputTokens != 540 || prompt.Usage.CacheCreationTokens != 50 || prompt.Usage.OutputTokens != 0 {
		t.Errorf("prompt = %+v, want input 540, cache write 50, no output", prompt.Usage)
	}
	text := findContributor(t, got, KindText, LabelAssistantText, "")
	if text.Usage.OutputTokens != 26 || text.Usage.APICallCount != 3 || text.Usage.InputTokens != 0 {
		t.Errorf("text = %+v, want output 26, 3 calls, no input", text.Usage)
	}
	replay := findContributor(t, got, KindReplay, LabelContextReplay, "")
	if replay.Usage.CacheReadTokens != 600 || replay.Usage.InputTokens != 0 || replay.Usage.OutputTokens != 0 {
		t.Errorf("replay = %+v, want only 600 cache reads", replay.Usage)
	}
	replays := 0
	for _, c := range got.Contributors {
		if c.Kind == KindReplay {
			replays++
		}
	}
	if replays != 1 {
		t.Errorf("%d replay rows, want 1", replays)
	}
	if len(got.Contributors) != 3 {
		t.Errorf("%d contributors, want 3: %+v", len(got.Contributors), got.Contributors)
	}
	for _, c := range got.Contributors {
		if c.Model != "claude-opus-4-1" {
			t.Errorf("%s: model %q", c.Label, c.Model)
		}
	}
}

func TestAttributeUsageUnknownCallStillCreatesToolRows(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{UsageUnknown: true, Emitted: []types.ToolUseRef{ref("t1", "Bash", "ls")}},
		{UsageUnknown: true},
	}}
	got := Attribute(a, nil)
	bash := findContributor(t, got, KindTool, "Bash", "")
	if bash.Usage != (types.TokenUsage{}) {
		t.Errorf("Bash usage = %+v, want zero", bash.Usage)
	}
	if len(bash.Details) != 1 || bash.Details[0] != (Detail{Detail: "ls", Calls: 1}) {
		t.Errorf("Bash details = %+v, want one zero-token detail with 1 call", bash.Details)
	}
	if len(got.Contributors) != 1 {
		t.Errorf("contributors = %+v, want only the zero Bash row (no zero synthetic rows)", got.Contributors)
	}
}

func TestAttributeSkillAnnotationAndLoadedRow(t *testing.T) {
	t.Parallel()

	skillRef := types.ToolUseRef{ID: "s1", Tool: "Skill", Detail: "systematic-debugging", SkillName: "systematic-debugging"}
	a := &types.Attribution{Calls: []types.CallUsage{
		{Usage: types.TokenUsage{OutputTokens: 10, APICallCount: 1}, Emitted: []types.ToolUseRef{skillRef}},
		{
			ActiveSkill: "systematic-debugging",
			Usage:       types.TokenUsage{InputTokens: 500, OutputTokens: 8, APICallCount: 1},
			Consumed:    []types.ToolResultRef{{ToolUse: skillRef, Bytes: 2000}},
			Emitted:     []types.ToolUseRef{ref("b1", "Bash", "go test")},
		},
		{
			ActiveSkill: "systematic-debugging",
			Usage:       types.TokenUsage{InputTokens: 60, OutputTokens: 3, APICallCount: 1},
			Consumed:    []types.ToolResultRef{{ToolUse: ref("b1", "Bash", "go test"), Bytes: 600}},
		},
		{Usage: types.TokenUsage{OutputTokens: 6, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("b2", "Bash", "git status")}},
	}}
	got := Attribute(a, nil)

	loaded := findContributor(t, got, KindSkill, "systematic-debugging", "")
	if loaded.Usage.OutputTokens != 10 || loaded.Usage.InputTokens != 500 || loaded.Usage.APICallCount != 1 {
		t.Errorf("loaded skill row = %+v, want output 10 + input 500", loaded.Usage)
	}
	during := findContributor(t, got, KindTool, "Bash", "systematic-debugging")
	if during.Usage.OutputTokens != 8 || during.Usage.InputTokens != 60 {
		t.Errorf("Bash during skill = %+v, want output 8 + input 60", during.Usage)
	}
	plain := findContributor(t, got, KindTool, "Bash", "")
	if plain.Usage.OutputTokens != 6 || plain.Usage.InputTokens != 0 {
		t.Errorf("plain Bash = %+v, want output 6 only", plain.Usage)
	}
	// Assistant text while the skill was active is annotated too.
	text := findContributor(t, got, KindText, LabelAssistantText, "systematic-debugging")
	if text.Usage.OutputTokens != 3 {
		t.Errorf("annotated text = %+v, want output 3", text.Usage)
	}
}

func TestAttributeSubagentAbsorbsRecord(t *testing.T) {
	t.Parallel()

	agentRef := types.ToolUseRef{ID: "a1", Tool: "Agent", Detail: "Explore", SubagentType: "Explore", Model: "haiku"}
	rec := types.TokenUsage{InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50, APICallCount: 3, Model: "claude-haiku-4-5"}
	a := &types.Attribution{
		Calls: []types.CallUsage{
			{Model: "claude-opus-4-1", Usage: types.TokenUsage{OutputTokens: 7, CacheReadTokens: 10, APICallCount: 1}, Emitted: []types.ToolUseRef{agentRef}},
			{Model: "claude-opus-4-1", Usage: types.TokenUsage{InputTokens: 30, APICallCount: 1}, Consumed: []types.ToolResultRef{{ToolUse: agentRef, Bytes: 100}}},
		},
		Subagents: []types.SubagentRecord{{ToolUseID: "a1", SubagentType: "Explore", Model: "claude-haiku-4-5-20251001", Usage: &rec}},
	}
	got := Attribute(a, nil)

	sub := findContributor(t, got, KindSubagent, "Explore", "")
	want := types.TokenUsage{InputTokens: 130, CacheReadTokens: 900, OutputTokens: 57, APICallCount: 4}
	if sub.Usage != want {
		t.Errorf("subagent usage = %+v, want %+v (parent share + record incl. its cache reads)", sub.Usage, want)
	}
	if sub.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q, want record.Model to win", sub.Model)
	}
	if sub.Source != SourceTranscript {
		t.Errorf("source = %q, want %q", sub.Source, SourceTranscript)
	}
	// The parent's own replay stays on the parent's row.
	replay := findContributor(t, got, KindReplay, LabelContextReplay, "")
	if replay.Usage.CacheReadTokens != 10 {
		t.Errorf("replay = %d, want 10", replay.Usage.CacheReadTokens)
	}
	if len(sub.Details) != 1 || sub.Details[0].Detail != "Explore" || sub.Details[0].Calls != 1 || sub.Details[0].Tokens != 37 {
		t.Errorf("subagent details = %+v, want one Explore detail with 1 call and 37 parent-side tokens", sub.Details)
	}
}

func TestAttributeSubagentModelPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		rec   types.SubagentRecord
		model string
	}{
		{"record model", types.SubagentRecord{ToolUseID: "a1", Model: "rec-model", Usage: &types.TokenUsage{Model: "usage-model"}}, "rec-model"},
		{"usage model", types.SubagentRecord{ToolUseID: "a1", Usage: &types.TokenUsage{Model: "usage-model"}}, "usage-model"},
		{"ref alias", types.SubagentRecord{ToolUseID: "a1", Usage: &types.TokenUsage{}}, "haiku"},
		{"nil usage falls back to ref alias", types.SubagentRecord{ToolUseID: "a1"}, "haiku"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agentRef := types.ToolUseRef{ID: "a1", Tool: "Task", SubagentType: "Explore", Model: "haiku"}
			a := &types.Attribution{
				Calls:     []types.CallUsage{{Usage: types.TokenUsage{OutputTokens: 1, APICallCount: 1}, Emitted: []types.ToolUseRef{agentRef}}},
				Subagents: []types.SubagentRecord{tc.rec},
			}
			sub := findContributor(t, Attribute(a, nil), KindSubagent, "Explore", "")
			if sub.Model != tc.model {
				t.Errorf("model = %q, want %q", sub.Model, tc.model)
			}
		})
	}
}

func TestAttributeOrphanSubagentRecord(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{
		Calls: []types.CallUsage{{Usage: types.TokenUsage{OutputTokens: 1, APICallCount: 1}}},
		Subagents: []types.SubagentRecord{
			{ToolUseID: "before-window", SubagentType: "Plan", Model: modelSonnet, Usage: &types.TokenUsage{InputTokens: 10, CacheReadTokens: 20, OutputTokens: 5, APICallCount: 2}},
			{ToolUseID: "no-usage-yet", SubagentType: "Explore"},
		},
	}
	got := Attribute(a, nil)
	plan := findContributor(t, got, KindSubagent, "Plan", "")
	if plan.Source != SourceTaskRecord || plan.Model != modelSonnet {
		t.Errorf("orphan = source %q model %q", plan.Source, plan.Model)
	}
	if plan.Usage != (types.TokenUsage{InputTokens: 10, CacheReadTokens: 20, OutputTokens: 5, APICallCount: 2}) {
		t.Errorf("orphan usage = %+v", plan.Usage)
	}
	explore := findContributor(t, got, KindSubagent, "Explore", "")
	if explore.Usage != (types.TokenUsage{}) || explore.Source != SourceTaskRecord {
		t.Errorf("nil-usage orphan = %+v", explore)
	}
}

func TestAttributeUnresolvableResult(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{{
		Usage:    types.TokenUsage{InputTokens: 90, OutputTokens: 1, APICallCount: 1},
		Consumed: []types.ToolResultRef{{ToolUse: types.ToolUseRef{ID: "old"}, Bytes: 5}},
	}}}
	got := Attribute(a, nil)
	earlier := findContributor(t, got, KindTool, LabelEarlierResults, "")
	if earlier.Usage.InputTokens != 90 || len(earlier.Details) != 0 {
		t.Errorf("earlier results = %+v", earlier)
	}
}

func TestAttributeMergesSameKeyAndSorts(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{Model: modelSonnet, Usage: types.TokenUsage{OutputTokens: 10, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Bash", "go test")}},
		{Model: modelSonnet, Usage: types.TokenUsage{OutputTokens: 4, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t2", "Bash", "git log"), ref("t3", "Read", "b.go")}},
		{Model: modelSonnet, Usage: types.TokenUsage{CacheReadTokens: 1000, OutputTokens: 1, APICallCount: 1}},
	}}
	got := Attribute(a, nil)

	// Bash 12 output (units 60), Context replay 1000 cache read (units 100),
	// Read 2 output (units 10), Assistant text 1 output (units 5).
	labels := make([]string, 0, len(got.Contributors))
	for _, c := range got.Contributors {
		labels = append(labels, c.Label)
	}
	want := []string{LabelContextReplay, "Bash", "Read", LabelAssistantText}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("order = %v, want %v", labels, want)
	}
	bash := got.Contributors[1]
	if bash.Usage.OutputTokens != 12 || bash.Usage.APICallCount != 2 {
		t.Errorf("merged Bash = %+v", bash.Usage)
	}
	var total float64
	for _, c := range got.Contributors {
		total += c.CostShare
	}
	assertClose(t, total, 1)
	assertClose(t, bash.CostShare, 60.0/175)
}

func TestAttributeSortsByVolumeThenLabelWhenUnpriced(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{Usage: types.TokenUsage{OutputTokens: 9, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Read", ""), ref("t2", "Bash", ""), ref("t3", "Grep", "")}},
		{Usage: types.TokenUsage{OutputTokens: 5, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t4", "Zeta", "")}},
	}}
	got := Attribute(a, nil)
	labels := make([]string, 0, len(got.Contributors))
	for _, c := range got.Contributors {
		labels = append(labels, c.Label)
	}
	want := []string{"Zeta", "Bash", "Grep", "Read"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("order = %v, want %v", labels, want)
	}
	if got.PricedUnits != 0 {
		t.Errorf("priced units = %v, want 0 for unrecorded models", got.PricedUnits)
	}
	if len(got.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want empty (an unrecorded model has no name to list)", got.Unpriced)
	}
}

func TestAttributeDetailsTopThree(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{
			Model: modelSonnet,
			Usage: types.TokenUsage{OutputTokens: 100, APICallCount: 1},
			Emitted: []types.ToolUseRef{
				ref("t1", "Bash", "go test"), ref("t2", "Bash", "go test"), ref("t3", "Bash", "go test"), ref("t4", "Bash", "go test"),
				ref("t5", "Bash", "git log"), ref("t6", "Bash", "git log"), ref("t7", "Bash", "git log"),
				ref("t8", "Bash", "ls"), ref("t9", "Bash", "ls"),
				ref("t10", "Bash", "rm"),
			},
		},
		{
			Model:    modelSonnet,
			Usage:    types.TokenUsage{InputTokens: 1000, APICallCount: 1},
			Consumed: []types.ToolResultRef{{ToolUse: ref("t1", "Bash", "go test"), Bytes: 10}, {ToolUse: ref("old", "Bash", "cat"), Bytes: 10}},
		},
	}}
	got := Attribute(a, nil)
	bash := findContributor(t, got, KindTool, "Bash", "")

	// Output 100 over 10 refs = 10 each; input 1000 split 500/500 between
	// "go test" (t1) and "cat" (emitted before the window → counts as a call).
	wantDetails := []Detail{
		{Detail: "go test", Calls: 4, Tokens: 540},
		{Detail: "cat", Calls: 1, Tokens: 500},
		{Detail: "git log", Calls: 3, Tokens: 30},
	}
	if len(bash.Details) != len(wantDetails) {
		t.Fatalf("details = %+v, want %+v", bash.Details, wantDetails)
	}
	for i, d := range bash.Details {
		if d.Detail != wantDetails[i].Detail || d.Calls != wantDetails[i].Calls || d.Tokens != wantDetails[i].Tokens {
			t.Errorf("detail[%d] = %+v, want %+v", i, d, wantDetails[i])
		}
	}
	// Units: output 100×5 = 500, input 1000×1 = 1000 → priced 1500.
	assertClose(t, bash.Details[0].CostShare, (40*5+500.0)/1500)
	assertClose(t, bash.Details[1].CostShare, 500.0/1500)
	var sum int
	for _, d := range bash.Details {
		sum += d.Tokens
	}
	if vol := volume(&bash.Usage); sum > vol {
		t.Errorf("Σ details %d > volume %d", sum, vol)
	}
}

func TestAttributeCostSharesAcrossModels(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{InputTokens: 1000, OutputTokens: 100, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Bash", "")}},
		{Model: "gpt-5.4", Usage: types.TokenUsage{InputTokens: 2000, OutputTokens: 100, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t2", "Read", "")}},
	}}
	got := Attribute(a, nil)
	var total float64
	for _, c := range got.Contributors {
		total += c.CostShare
	}
	assertClose(t, total, 1)
	// Priced: claude 1000 + 500, gpt 2000 + 600 → 4100.
	assertCloseRel(t, got.PricedUnits, 4100)
	prompt := findContributor(t, got, KindPrompt, LabelPromptContext, "")
	assertClose(t, prompt.CostShare, 3000.0/4100)
	if prompt.Model != "" {
		t.Errorf("prompt row mixes models, want Model cleared, got %q", prompt.Model)
	}
}

func TestAttributeUnknownModelContributesVolumeOnly(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{Calls: []types.CallUsage{
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{OutputTokens: 100, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Bash", "")}},
		{Model: "mystery-9000", Usage: types.TokenUsage{OutputTokens: 400, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t2", "Read", "")}},
		{Model: "mystery-9000", Usage: types.TokenUsage{OutputTokens: 1, APICallCount: 1}},
		{Model: "other-unknown", Usage: types.TokenUsage{OutputTokens: 1, APICallCount: 1}},
	}}
	got := Attribute(a, nil)
	if !reflect.DeepEqual(got.Unpriced, []string{"mystery-9000", "other-unknown"}) {
		t.Errorf("unpriced = %v", got.Unpriced)
	}
	assertCloseRel(t, got.PricedUnits, 500)
	read := findContributor(t, got, KindTool, "Read", "")
	if read.Usage.OutputTokens != 400 || read.CostShare != 0 {
		t.Errorf("unknown-model Read = %+v", read)
	}
	bash := findContributor(t, got, KindTool, "Bash", "")
	assertClose(t, bash.CostShare, 1)
	// Priced rows sort first even when a volume-only row is larger.
	if got.Contributors[0].Label != "Bash" {
		t.Errorf("first = %q, want Bash", got.Contributors[0].Label)
	}
}

func TestAttributeWeightsOverride(t *testing.T) {
	t.Parallel()

	w := Weights{Provider: ProviderAnthropic, Family: FamilyAnthropic, Input: 1, Output: 2}
	a := &types.Attribution{Calls: []types.CallUsage{
		{Model: "mystery-9000", Usage: types.TokenUsage{InputTokens: 10, OutputTokens: 10, APICallCount: 1}},
		{Model: "gpt-5.4", Usage: types.TokenUsage{InputTokens: 10, OutputTokens: 10, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t", "Bash", "")}},
	}}
	got := Attribute(a, &w)
	if len(got.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want none under an override", got.Unpriced)
	}
	// Every call priced at 1×input + 2×output: 2 × (10 + 20).
	assertCloseRel(t, got.PricedUnits, 60)
	bash := findContributor(t, got, KindTool, "Bash", "")
	assertClose(t, bash.CostShare, 20.0/60)
}

func TestAttributeLongContextTierPerCall(t *testing.T) {
	t.Parallel()

	u := types.TokenUsage{InputTokens: 200_000, CacheReadTokens: 100_000, OutputTokens: 1000, APICallCount: 1}
	a := &types.Attribution{Calls: []types.CallUsage{{Model: "gpt-5.5", Usage: u}}}
	got := Attribute(a, nil)
	want := ComputeCostShares(&u, WeightsForCall("gpt-5.5", 300_000)).Units
	base := ComputeCostShares(&u, WeightsForCall("gpt-5.5", 0)).Units
	if want <= base {
		t.Fatalf("test premise: tiered units %v should exceed base %v", want, base)
	}
	assertCloseRel(t, got.PricedUnits, want)
}

func TestMergeContributors(t *testing.T) {
	t.Parallel()

	s1 := Attribute(&types.Attribution{Calls: []types.CallUsage{
		{Model: modelSonnet, Usage: types.TokenUsage{OutputTokens: 10, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Bash", "go test")}},
		{Model: modelSonnet, Usage: types.TokenUsage{CacheReadTokens: 100, OutputTokens: 2, APICallCount: 1}},
	}}, nil)
	s2 := Attribute(&types.Attribution{Calls: []types.CallUsage{
		{Model: "mystery", Usage: types.TokenUsage{OutputTokens: 30, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t1", "Bash", "git log"), ref("t2", "Bash", "go test")}},
		{Model: "claude-opus-4-1", Usage: types.TokenUsage{OutputTokens: 4, APICallCount: 1}, Emitted: []types.ToolUseRef{ref("t3", "Read", "")}},
	}}, nil)
	got := MergeContributors([]Attributed{s1, s2})

	if !reflect.DeepEqual(got.Unpriced, []string{"mystery"}) {
		t.Errorf("unpriced = %v", got.Unpriced)
	}
	// s1: Bash 50 + text 10 + replay 10 = 70; s2: Read 20. Total 90.
	assertCloseRel(t, got.PricedUnits, 90)
	bash := findContributor(t, got, KindTool, "Bash", "")
	if bash.Usage.OutputTokens != 40 || bash.Usage.APICallCount != 2 {
		t.Errorf("merged Bash = %+v", bash.Usage)
	}
	if bash.Model != "" {
		t.Errorf("merged Bash model = %q, want cleared (sonnet + recorded-but-unpriced mystery)", bash.Model)
	}
	assertClose(t, bash.CostShare, 50.0/90)
	wantDetails := []Detail{{Detail: "go test", Calls: 2, Tokens: 25, CostShare: 50.0 / 90}, {Detail: "git log", Calls: 1, Tokens: 15}}
	if len(bash.Details) != 2 {
		t.Fatalf("details = %+v", bash.Details)
	}
	for i, d := range bash.Details {
		if d.Detail != wantDetails[i].Detail || d.Calls != wantDetails[i].Calls || d.Tokens != wantDetails[i].Tokens {
			t.Errorf("detail[%d] = %+v, want %+v", i, d, wantDetails[i])
		}
		assertClose(t, d.CostShare, wantDetails[i].CostShare)
	}
	var total float64
	for _, c := range got.Contributors {
		total += c.CostShare
	}
	assertClose(t, total, 1)
	if len(MergeContributors(nil).Contributors) != 0 {
		t.Error("merging nothing should yield no contributors")
	}
}

func TestAttributeOrphanRecordWithoutTypeIsUnknown(t *testing.T) {
	t.Parallel()

	a := &types.Attribution{
		Calls:     []types.CallUsage{{Usage: types.TokenUsage{OutputTokens: 1, APICallCount: 1}}},
		Subagents: []types.SubagentRecord{{ToolUseID: "gone", Usage: &types.TokenUsage{OutputTokens: 5, APICallCount: 1}}},
	}
	got := findContributor(t, Attribute(a, nil), KindSubagent, LabelUnknownSubagent, "")
	if got.Source != SourceTaskRecord || got.Usage.OutputTokens != 5 {
		t.Errorf("unknown-type orphan = %+v", got)
	}
}

func TestAttributeSubagentRecordPricedAtBaseTier(t *testing.T) {
	t.Parallel()

	big := types.TokenUsage{InputTokens: 300_000, OutputTokens: 1000, APICallCount: 1}
	base := ComputeCostShares(&big, WeightsForCall("gpt-5.5", 0)).Units
	tiered := ComputeCostShares(&big, WeightsForCall("gpt-5.5", 300_000)).Units
	if tiered <= base {
		t.Fatalf("test premise: tiered %v should exceed base %v", tiered, base)
	}

	// The same 300K input as one CALL is tiered...
	call := Attribute(&types.Attribution{Calls: []types.CallUsage{{Model: "gpt-5.5", Usage: big}}}, nil)
	assertCloseRel(t, call.PricedUnits, tiered)

	// ...but as a subagent RECORD (a session aggregate) it is priced at base.
	agentRef := types.ToolUseRef{ID: "a1", Tool: "Agent", SubagentType: "Explore"}
	rec := Attribute(&types.Attribution{
		Calls:     []types.CallUsage{{UsageUnknown: true, Emitted: []types.ToolUseRef{agentRef}}},
		Subagents: []types.SubagentRecord{{ToolUseID: "a1", SubagentType: "Explore", Model: "gpt-5.5", Usage: &big}},
	}, nil)
	assertCloseRel(t, rec.PricedUnits, base)
	if len(rec.Unpriced) != 0 {
		t.Errorf("unpriced = %v: a UsageUnknown call on an unrecorded model has nothing to report", rec.Unpriced)
	}
}

func TestAttributePreWindowResultCountedOnce(t *testing.T) {
	t.Parallel()

	old := ref("old", "Bash", "cat")
	a := &types.Attribution{Calls: []types.CallUsage{
		{Usage: types.TokenUsage{InputTokens: 10, APICallCount: 1}, Consumed: []types.ToolResultRef{{ToolUse: old, Bytes: 5}}},
		{Usage: types.TokenUsage{InputTokens: 20, APICallCount: 1}, Consumed: []types.ToolResultRef{{ToolUse: old, Bytes: 5}}},
	}}
	bash := findContributor(t, Attribute(a, nil), KindTool, "Bash", "")
	if len(bash.Details) != 1 || bash.Details[0].Calls != 1 || bash.Details[0].Tokens != 30 {
		t.Errorf("details = %+v, want cat: 1 call, 30 tokens", bash.Details)
	}
}

func TestAttributeDuplicateRecordsForOneToolUse(t *testing.T) {
	t.Parallel()

	agentRef := types.ToolUseRef{ID: "a1", Tool: "Agent", SubagentType: "Explore", Model: "haiku"}
	a := &types.Attribution{
		Calls: []types.CallUsage{{Model: "claude-opus-4-1", Usage: types.TokenUsage{OutputTokens: 2, APICallCount: 1}, Emitted: []types.ToolUseRef{agentRef}}},
		Subagents: []types.SubagentRecord{
			{ToolUseID: "a1", SubagentType: "Explore", Model: modelSonnet, Usage: &types.TokenUsage{OutputTokens: 10, APICallCount: 1}},
			{ToolUseID: "a1", SubagentType: "Explore", Model: "claude-opus-4-1", Usage: &types.TokenUsage{OutputTokens: 20, APICallCount: 2}},
		},
	}
	got := Attribute(a, nil)
	sub := findContributor(t, got, KindSubagent, "Explore", "")
	if sub.Usage.OutputTokens != 32 || sub.Usage.APICallCount != 4 {
		t.Errorf("usage = %+v, want both records absorbed (2+10+20 output, 1+1+2 calls)", sub.Usage)
	}
	if sub.Model != modelSonnet {
		t.Errorf("model = %q, want the first record's", sub.Model)
	}
	if n := len(got.Contributors); n != 1 {
		t.Errorf("%d contributors, want 1", n)
	}
}

func TestMergeContributorsSourcePrecedence(t *testing.T) {
	t.Parallel()

	orphan := Attributed{Contributors: []Contributor{{Kind: KindSubagent, Label: "Explore", Source: SourceTaskRecord, Usage: types.TokenUsage{OutputTokens: 1}}}}
	live := Attributed{Contributors: []Contributor{{Kind: KindSubagent, Label: "Explore", Source: SourceTranscript, Usage: types.TokenUsage{OutputTokens: 2}}}}
	for name, in := range map[string][]Attributed{"orphan first": {orphan, live}, "live first": {live, orphan}} {
		got := findContributor(t, MergeContributors(in), KindSubagent, "Explore", "")
		if got.Source != SourceTranscript || got.Usage.OutputTokens != 3 {
			t.Errorf("%s: %+v, want transcript source and 3 output", name, got)
		}
	}
	only := findContributor(t, MergeContributors([]Attributed{orphan, orphan}), KindSubagent, "Explore", "")
	if only.Source != SourceTaskRecord {
		t.Errorf("task_record-only merge = %q", only.Source)
	}
}

func TestSplitWithSubsetCorruptFallback(t *testing.T) {
	t.Parallel()

	wholes, subsets := splitWithSubset([]int{1, 1}, 2, 3, 5)
	if !reflect.DeepEqual(wholes, []int{2, 1}) || !reflect.DeepEqual(subsets, []int{3, 2}) {
		t.Errorf("wholes %v subsets %v, want [2 1] / [3 2]", wholes, subsets)
	}
	wholes, subsets = splitWithSubset([]int{1, 1, 1}, 3, 10, 4)
	if !reflect.DeepEqual(wholes, []int{4, 3, 3}) || !reflect.DeepEqual(subsets, []int{2, 1, 1}) {
		t.Errorf("well-formed: wholes %v subsets %v, want [4 3 3] / [2 1 1]", wholes, subsets)
	}
}

func TestLargestRemainder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		counts        []int
		total, budget int
		want          []int
	}{
		{"thirds to 100", []int{1, 1, 1}, 3, 100, []int{34, 33, 33}},
		{"quarters to 100", []int{1, 1, 1, 1}, 4, 100, []int{25, 25, 25, 25}},
		{"non-positive total", []int{1, 2}, 0, 100, []int{0, 0}},
		{"zero budget", []int{1, 2}, 3, 0, []int{0, 0}},
		{"ten across three", []int{1, 1, 1}, 3, 10, []int{4, 3, 3}},
		{"bytes", []int{12000, 300}, 12300, 1230, []int{1200, 30}},
		{"remainder ties by lower index", []int{1, 1, 1, 1, 1}, 5, 7, []int{2, 2, 1, 1, 1}},
		{"empty", nil, 0, 5, []int{}},
	}
	for _, tc := range cases {
		got := LargestRemainder(tc.counts, tc.total, tc.budget)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// randomAttribution builds a random call graph: every call emits 0–3 refs,
// consumes 0–3 results (mostly refs emitted by earlier calls, sometimes an
// unresolvable one), with random zero Bytes, random skills/subagents, some
// UsageUnknown calls, and subagent records both matched and orphaned.
func randomAttribution(r *rand.Rand) *types.Attribution {
	models := []string{"claude-opus-4-1", "gpt-5.4", "gemini-2.5-pro", "mystery", ""}
	tools := []string{"Bash", "Read", "Grep", "Edit"}
	a := &types.Attribution{}
	var pool []types.ToolUseRef
	var subagentIDs []string
	nCalls := 1 + r.IntN(8)
	for i := range nCalls {
		call := types.CallUsage{Model: models[r.IntN(len(models))]}
		if r.IntN(6) == 0 {
			call.UsageUnknown = true
		} else {
			out := r.IntN(2000)
			cc := r.IntN(3000)
			call.Usage = types.TokenUsage{
				InputTokens:           r.IntN(5000),
				CacheCreationTokens:   cc,
				CacheReadTokens:       r.IntN(50000),
				OutputTokens:          out,
				APICallCount:          1,
				ThinkingTokens:        r.IntN(out + 1),
				CacheCreation1hTokens: r.IntN(cc + 1),
			}
		}
		if r.IntN(3) == 0 {
			call.ActiveSkill = "skill-" + strconv.Itoa(r.IntN(2))
		}
		for range r.IntN(4) {
			id := "t" + strconv.Itoa(i) + "-" + strconv.Itoa(len(pool))
			rf := types.ToolUseRef{ID: id, Tool: tools[r.IntN(len(tools))], Detail: "d" + strconv.Itoa(r.IntN(5))}
			switch r.IntN(8) {
			case 0:
				rf.Tool, rf.SkillName = "Skill", "sk"+strconv.Itoa(r.IntN(2))
			case 1:
				rf.Tool, rf.SubagentType, rf.Model = "Agent", "Explore", "haiku"
				subagentIDs = append(subagentIDs, id)
			}
			call.Emitted = append(call.Emitted, rf)
		}
		for range r.IntN(4) {
			var res types.ToolResultRef
			if len(pool) > 0 && r.IntN(5) > 0 {
				res.ToolUse = pool[r.IntN(len(pool))]
			} else {
				res.ToolUse = types.ToolUseRef{ID: "old" + strconv.Itoa(r.IntN(3))}
			}
			if r.IntN(3) > 0 {
				res.Bytes = r.IntN(20000)
			}
			call.Consumed = append(call.Consumed, res)
		}
		pool = append(pool, call.Emitted...)
		a.Calls = append(a.Calls, call)
	}
	for _, id := range subagentIDs {
		if r.IntN(4) == 0 {
			continue // spawned but no record yet
		}
		a.Subagents = append(a.Subagents, randomRecord(r, id, models))
	}
	for range r.IntN(3) {
		a.Subagents = append(a.Subagents, randomRecord(r, "orphan"+strconv.Itoa(r.IntN(3)), models))
	}
	return a
}

func randomRecord(r *rand.Rand, id string, models []string) types.SubagentRecord {
	rec := types.SubagentRecord{ToolUseID: id, SubagentType: "Explore", Model: models[r.IntN(len(models))]}
	if r.IntN(5) > 0 {
		rec.Usage = &types.TokenUsage{
			InputTokens: r.IntN(5000), CacheCreationTokens: r.IntN(1000), CacheReadTokens: r.IntN(90000),
			OutputTokens: r.IntN(3000), APICallCount: 1 + r.IntN(20), Model: models[r.IntN(len(models))],
		}
		if r.IntN(3) == 0 {
			// A nested chain must be flattened away, never carried into a row.
			rec.Usage.SubagentTokens = &types.TokenUsage{InputTokens: 1 + r.IntN(100), OutputTokens: 1 + r.IntN(100), APICallCount: 1}
		}
	}
	return rec
}

// sameCounts compares the seven numeric fields of two usages, ignoring Model
// and any nested SubagentTokens.
func sameCounts(a, b *types.TokenUsage) bool {
	return a.InputTokens == b.InputTokens && a.CacheCreationTokens == b.CacheCreationTokens &&
		a.CacheReadTokens == b.CacheReadTokens && a.OutputTokens == b.OutputTokens &&
		a.APICallCount == b.APICallCount && a.ThinkingTokens == b.ThinkingTokens &&
		a.CacheCreation1hTokens == b.CacheCreation1hTokens
}

func TestAttributeConservesEveryClass(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(20260828, 1))
	for iter := range 200 {
		a := randomAttribution(r)
		var want *types.TokenUsage
		for i := range a.Calls {
			want = types.AddTokenUsage(want, &a.Calls[i].Usage)
		}
		for _, s := range a.Subagents {
			want = types.AddTokenUsage(want, s.Usage)
		}

		got := Attribute(a, nil)
		var sum *types.TokenUsage
		for _, c := range got.Contributors {
			u := c.Usage
			sum = types.AddTokenUsage(sum, &u)
			var detailTokens int
			for _, d := range c.Details {
				detailTokens += d.Tokens
			}
			if detailTokens > volume(&c.Usage) {
				t.Errorf("iter %d: %s/%q Σ details %d > volume %d", iter, c.Kind, c.Label, detailTokens, volume(&c.Usage))
			}
			if len(c.Details) > maxDetails {
				t.Errorf("iter %d: %d details", iter, len(c.Details))
			}
			if c.Usage.ThinkingTokens > c.Usage.OutputTokens || c.Usage.CacheCreation1hTokens > c.Usage.CacheCreationTokens {
				t.Errorf("iter %d: %s/%q subset exceeds its class: %+v", iter, c.Kind, c.Label, c.Usage)
			}
			if c.Usage.SubagentTokens != nil || c.Usage.Model != "" {
				t.Errorf("iter %d: %s/%q usage not flat: model %q nested %+v", iter, c.Kind, c.Label, c.Usage.Model, c.Usage.SubagentTokens)
			}
		}
		if sum == nil {
			sum = &types.TokenUsage{}
		}
		if !sameCounts(sum, want) {
			t.Errorf("iter %d: Σ contributors %+v != Σ calls+subagents %+v", iter, *sum, *want)
		}
		var shares float64
		for _, c := range got.Contributors {
			shares += c.CostShare
		}
		if got.PricedUnits > 0 {
			assertClose(t, shares, 1)
		} else if shares != 0 {
			t.Errorf("iter %d: shares %v with nothing priced", iter, shares)
		}
	}
}
