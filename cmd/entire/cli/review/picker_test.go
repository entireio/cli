package review_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	testReviewSkill   = "/pr-review-toolkit:review-pr"
	testAgentName     = "claude-code"
	testCodexAgent    = "codex"
	testExternalAgent = "my-external"
	testExternalSkill = "/external-skill"
)

// TestMergePickerResults pins the data-loss regression where a
// manually-configured external-agent entry would be silently deleted the
// first time the user ran `entire review --edit`.
func TestMergePickerResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing map[string]settings.ReviewConfig
		offered  map[string]struct{}
		selected map[string]settings.ReviewConfig
		want     map[string]settings.ReviewConfig
	}{
		{
			name: "preserves uncurated/external entries the picker did not surface",
			existing: map[string]settings.ReviewConfig{
				testAgentName:     {Skills: []string{"/old-pick"}},
				testExternalAgent: {Skills: []string{testExternalSkill}},
			},
			offered:  map[string]struct{}{testAgentName: {}},
			selected: map[string]settings.ReviewConfig{testAgentName: {Skills: []string{"/new-pick"}}},
			want: map[string]settings.ReviewConfig{
				testAgentName:     {Skills: []string{"/new-pick"}},
				testExternalAgent: {Skills: []string{testExternalSkill}},
			},
		},
		{
			name: "offered agent with no picks is removed (user unconfiguring)",
			existing: map[string]settings.ReviewConfig{
				testAgentName:  {Skills: []string{"/old-pick"}},
				testCodexAgent: {Skills: []string{"/codex-pick"}},
			},
			offered:  map[string]struct{}{testAgentName: {}, testCodexAgent: {}},
			selected: map[string]settings.ReviewConfig{testAgentName: {Skills: []string{"/new-pick"}}},
			want: map[string]settings.ReviewConfig{
				testAgentName: {Skills: []string{"/new-pick"}},
			},
		},
		{
			name:     "empty existing: merge is identity on selected",
			existing: map[string]settings.ReviewConfig{},
			offered:  map[string]struct{}{testAgentName: {}},
			selected: map[string]settings.ReviewConfig{testAgentName: {Skills: []string{"/a"}}},
			want:     map[string]settings.ReviewConfig{testAgentName: {Skills: []string{"/a"}}},
		},
		{
			name: "deselected curated agent leaves only external entry",
			existing: map[string]settings.ReviewConfig{
				testAgentName:     {Skills: []string{"/old-pick"}},
				testExternalAgent: {Skills: []string{testExternalSkill}},
			},
			offered:  map[string]struct{}{testAgentName: {}},
			selected: map[string]settings.ReviewConfig{},
			want: map[string]settings.ReviewConfig{
				testExternalAgent: {Skills: []string{testExternalSkill}},
			},
		},
		{
			name:     "prompt-only entry is preserved",
			existing: map[string]settings.ReviewConfig{},
			offered:  map[string]struct{}{testAgentName: {}},
			selected: map[string]settings.ReviewConfig{
				testAgentName: {Prompt: "Focus on security regressions."},
			},
			want: map[string]settings.ReviewConfig{
				testAgentName: {Prompt: "Focus on security regressions."},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := review.MergePickerResults(tc.existing, tc.offered, tc.selected)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MergePickerResults =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

// TestSelectReviewAgent_OverrideResolvesSpecificAgent pins that --agent flag
// resolves a non-default configured agent when the map has multiple entries.
func TestSelectReviewAgent_OverrideResolvesSpecificAgent(t *testing.T) {
	t.Parallel()
	reviewMap := map[string]settings.ReviewConfig{
		testAgentName:  {Skills: []string{"/a"}},
		testCodexAgent: {Skills: []string{"/b"}},
	}

	name, cfg, err := review.SelectReviewAgent(reviewMap, testCodexAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != testCodexAgent || len(cfg.Skills) != 1 || cfg.Skills[0] != "/b" {
		t.Errorf("override=%s returned name=%q cfg=%+v", testCodexAgent, name, cfg)
	}

	// Default (no override) must remain the alphabetically-first agent.
	name, _, err = review.SelectReviewAgent(reviewMap, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != testAgentName {
		t.Errorf("default pick = %q, want %q", name, testAgentName)
	}

	// Unknown override must surface a helpful error listing configured agents.
	_, _, err = review.SelectReviewAgent(reviewMap, "gemini")
	if err == nil {
		t.Fatal("expected error for unconfigured --agent value")
	}
	if !strings.Contains(err.Error(), testAgentName) || !strings.Contains(err.Error(), testCodexAgent) {
		t.Errorf("error should list configured agents; got: %v", err)
	}
}

// TestSplitSavedPicks pins the partition logic used by the picker to
// pre-select previously-saved skills.
func TestSplitSavedPicks(t *testing.T) {
	t.Parallel()
	builtins := []skilldiscovery.CuratedSkill{
		{Name: "/review"},
		{Name: "/test-auditor"},
	}
	discovered := []agent.DiscoveredSkill{
		{Name: "/pr-review-toolkit:review-pr"},
		{Name: "/my-plugin:lint"},
	}

	tests := []struct {
		name           string
		saved          []string
		wantBuiltin    []string
		wantDiscovered []string
	}{
		{
			name:           "all matches — both buckets populated",
			saved:          []string{"/review", "/pr-review-toolkit:review-pr", "/test-auditor"},
			wantBuiltin:    []string{"/review", "/test-auditor"},
			wantDiscovered: []string{"/pr-review-toolkit:review-pr"},
		},
		{
			name:           "only builtins saved",
			saved:          []string{"/review"},
			wantBuiltin:    []string{"/review"},
			wantDiscovered: nil,
		},
		{
			name:           "only discovered saved",
			saved:          []string{"/my-plugin:lint"},
			wantBuiltin:    nil,
			wantDiscovered: []string{"/my-plugin:lint"},
		},
		{
			name:           "unknown saved skill drops from both (uninstalled/external)",
			saved:          []string{"/ghost"},
			wantBuiltin:    nil,
			wantDiscovered: nil,
		},
		{
			name:           "empty saved returns empty",
			saved:          nil,
			wantBuiltin:    nil,
			wantDiscovered: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBuiltin, gotDiscovered := review.SplitSavedPicks(tc.saved, builtins, discovered)
			if !reflect.DeepEqual(gotBuiltin, tc.wantBuiltin) {
				t.Errorf("builtin = %v, want %v", gotBuiltin, tc.wantBuiltin)
			}
			if !reflect.DeepEqual(gotDiscovered, tc.wantDiscovered) {
				t.Errorf("discovered = %v, want %v", gotDiscovered, tc.wantDiscovered)
			}
		})
	}
}

// TestBuildReviewPickerFields_StructureWithDiscovery pins the field count and
// layout when both built-ins and discovered skills are present.
func TestBuildReviewPickerFields_StructureWithDiscovery(t *testing.T) {
	t.Parallel()
	fields := review.BuildReviewPickerFields(
		"claude-code",
		[]skilldiscovery.CuratedSkill{{Name: "/review", Desc: "x"}},
		[]agent.DiscoveredSkill{{Name: "/pr-review-toolkit:review-pr", Description: "y"}},
		[]skilldiscovery.InstallHint{{Message: "install more"}},
		"",
		nil, nil, nil,
	)
	if len(fields) != 4 {
		t.Fatalf("picker fields = %d, want 4 (built-in, discovered, hint, prompt)", len(fields))
	}
}

// TestBuildReviewPickerFields_EmptyBuiltinsRendersNote pins that the
// built-in section renders as a Note when no built-ins are available.
func TestBuildReviewPickerFields_EmptyBuiltinsRendersNote(t *testing.T) {
	t.Parallel()
	fields := review.BuildReviewPickerFields(
		"gemini-cli",
		nil,
		nil,
		[]skilldiscovery.InstallHint{{Message: "install gemini-code-review"}},
		"",
		nil, nil, nil,
	)
	if len(fields) != 4 {
		t.Fatalf("fields = %d, want 4 even with empty built-ins and discovered", len(fields))
	}
	for i, f := range fields {
		if f == nil {
			t.Errorf("fields[%d] is nil — every slot must be populated", i)
		}
	}
}

// TestBuildReviewPickerFields_HintSectionOmittedWhenEmpty pins that the
// hints section is omitted when there are no active hints.
func TestBuildReviewPickerFields_HintSectionOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	fields := review.BuildReviewPickerFields(
		"claude-code",
		[]skilldiscovery.CuratedSkill{{Name: "/review", Desc: "x"}},
		nil,
		nil,
		"",
		nil, nil, nil,
	)
	if len(fields) != 3 {
		t.Errorf("fields count = %d, want 3 (hint section omitted when empty)", len(fields))
	}
}

func TestBuildReviewPickerFields_SingleBuiltinDefaultsSelectedAndRenders(t *testing.T) {
	t.Parallel()

	var builtinPicks []string
	fields := review.BuildReviewPickerFields(
		"codex",
		[]skilldiscovery.CuratedSkill{{Name: "/review", Desc: "Review current changes"}},
		nil,
		nil,
		"",
		&builtinPicks, nil, nil,
	)

	if len(fields) == 0 {
		t.Fatal("expected picker fields")
	}
	got, ok := fields[0].GetValue().([]string)
	if !ok {
		t.Fatalf("built-in field value has type %T, want []string", fields[0].GetValue())
	}
	if !reflect.DeepEqual(got, []string{"/review"}) {
		t.Fatalf("built-in defaults = %v, want [/review]", got)
	}

	field := fields[0].WithWidth(80)
	field.Focus()
	if got := field.View(); !strings.Contains(got, "/review") {
		t.Fatalf("single built-in option did not render:\n%s", got)
	}
}

// TestSaveReviewConfig_PersistsSettings verifies SaveReviewConfig writes and
// the settings can be read back.
func TestSaveReviewConfig_PersistsSettings(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)

	err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		testAgentName: {Skills: []string{testReviewSkill, "/test-auditor"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	cfg := s.Review[testAgentName]
	if len(cfg.Skills) != 2 {
		t.Errorf("expected 2 skills saved, got %v", cfg.Skills)
	}
	if cfg.Skills[0] != testReviewSkill {
		t.Errorf("first skill = %q", cfg.Skills[0])
	}
}

// TestSaveReviewConfig_ReturnsErrorOnMalformedSettings ensures SaveReviewConfig
// does not overwrite existing settings when settings.json is malformed.
func TestSaveReviewConfig_ReturnsErrorOnMalformedSettings(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	malformed := []byte(`{"enabled": true, "strategy": "manual-commit", "review": {`)
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(entireDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	err = review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		testAgentName: {Skills: []string{testReviewSkill}},
	})
	if err == nil {
		t.Fatal("expected SaveReviewConfig to error on malformed settings")
	}

	after, err := os.ReadFile(filepath.Join(entireDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("settings.json was overwritten on load error:\nbefore=%q\nafter=%q", before, after)
	}
}
