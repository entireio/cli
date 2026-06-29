package review

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// helperTestAgent is an agent name not in the registry, so labelForSimpleAgent
// falls back to the raw name and the helper assertions stay registry-independent.
const helperTestAgent = "agent-x"

// errStubReviewerStart is returned by the stub's Start, which these tests never
// call — reviewerFor only checks the returned interface for non-nil identity.
var errStubReviewerStart = errors.New("pureHelperReviewer.Start not implemented")

// pureHelperReviewer is a minimal AgentReviewer used only to make reviewerFor
// return a non-nil value for "launchable" agents in these helper tests.
type pureHelperReviewer struct{ name string }

func (p pureHelperReviewer) Name() string { return p.name }
func (p pureHelperReviewer) Start(context.Context, reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return nil, errStubReviewerStart
}

// reviewerForSet returns a reviewerFor that is non-nil for the named agents.
func reviewerForSet(launchable ...string) func(string) reviewtypes.AgentReviewer {
	set := make(map[string]struct{}, len(launchable))
	for _, n := range launchable {
		set[n] = struct{}{}
	}
	return func(name string) reviewtypes.AgentReviewer {
		if _, ok := set[name]; ok {
			return pureHelperReviewer{name: name}
		}
		return nil
	}
}

func TestFinalJudgeDisplayName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", "final judge"},
		{"   ", "final judge"},
		{"claude-code", "judge: claude-code"},
		{"  codex  ", "judge: codex"},
	}
	for _, tt := range tests {
		if got := finalJudgeDisplayName(tt.in); got != tt.want {
			t.Errorf("finalJudgeDisplayName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExampleAgentList(t *testing.T) {
	t.Parallel()

	// No installed agents → fixed fallback.
	const fallback = "claude-code,codex"
	if got := exampleAgentList(nil); got != fallback {
		t.Errorf("empty catalog = %q, want fallback", got)
	}
	if got := exampleAgentList([]reviewAgentCatalogEntry{{Name: "a"}, {Name: "b"}}); got != fallback {
		t.Errorf("no installed = %q, want fallback", got)
	}

	// Only installed entries are listed, capped at two.
	catalog := []reviewAgentCatalogEntry{
		{Name: "claude-code", Installed: true},
		{Name: "codex", Installed: false},
		{Name: "gemini", Installed: true},
		{Name: "pi", Installed: true},
	}
	if got := exampleAgentList(catalog); got != "claude-code,gemini" {
		t.Errorf("installed list = %q, want first two installed", got)
	}
}

func TestNonLaunchableEligibleNames(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{} // zero agents → labels are bare names
	eligible := []AgentChoice{{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"}}
	// bravo is launchable; alpha and charlie are not.
	got := nonLaunchableEligibleNames(profile, eligible, reviewerForSet("bravo"))
	want := []string{"alpha", "charlie"} // sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nonLaunchableEligibleNames = %v, want %v", got, want)
	}

	// All launchable → none reported.
	if got := nonLaunchableEligibleNames(profile, eligible, reviewerForSet("alpha", "bravo", "charlie")); len(got) != 0 {
		t.Errorf("all-launchable = %v, want empty", got)
	}
}

func TestLaunchableInstalledAgentNames(t *testing.T) {
	t.Parallel()
	installed := []types.AgentName{"bravo", "alpha", "charlie"}
	got := launchableInstalledAgentNames(installed, reviewerForSet("alpha", "bravo"))
	want := []string{"alpha", "bravo"} // sorted, charlie excluded (nil reviewer)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launchableInstalledAgentNames = %v, want %v", got, want)
	}

	// Nil reviewerFor keeps everyone (the guard only drops on a non-nil func).
	got = launchableInstalledAgentNames([]types.AgentName{"b", "a"}, nil)
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nil reviewerFor = %v, want %v", got, want)
	}
}

func TestSlotLabel(t *testing.T) {
	t.Parallel()
	if got := slotLabel(crewSlot{agent: helperTestAgent}); got != helperTestAgent {
		t.Errorf("slotLabel no model = %q, want %q", got, helperTestAgent)
	}
	if got := slotLabel(crewSlot{agent: helperTestAgent, model: "opus"}); got != helperTestAgent+" · opus" {
		t.Errorf("slotLabel with model = %q, want %q", got, helperTestAgent+" · opus")
	}
	if got := slotLabel(crewSlot{agent: helperTestAgent, model: "   "}); got != helperTestAgent {
		t.Errorf("slotLabel blank model = %q, want %q", got, helperTestAgent)
	}
}

func TestDefaultAgentPick(t *testing.T) {
	t.Parallel()
	choices := []AgentChoice{{Name: "a"}, {Name: "b"}}
	if got := defaultAgentPick(choices, "b"); got != "b" {
		t.Errorf("saved match = %q, want b", got)
	}
	if got := defaultAgentPick(choices, "missing"); got != "a" {
		t.Errorf("saved miss falls back to first = %q, want a", got)
	}
	if got := defaultAgentPick(nil, "anything"); got != "" {
		t.Errorf("empty choices = %q, want empty", got)
	}
}

func TestFilterOutBuiltinCollisions(t *testing.T) {
	t.Parallel()
	discovered := []agent.DiscoveredSkill{{Name: "/review"}, {Name: "/custom"}, {Name: "/audit"}}
	builtins := map[string]struct{}{"/review": {}, "/audit": {}}
	got := filterOutBuiltinCollisions(discovered, builtins)
	if len(got) != 1 || got[0].Name != "/custom" {
		t.Errorf("filterOutBuiltinCollisions = %v, want only /custom", got)
	}

	// No builtins → input returned unchanged.
	if got := filterOutBuiltinCollisions(discovered, nil); !reflect.DeepEqual(got, discovered) {
		t.Errorf("no builtins = %v, want unchanged", got)
	}
}

func TestDedupeStrings(t *testing.T) {
	t.Parallel()
	got := dedupeStrings([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"} // first-seen order preserved
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeStrings = %v, want %v", got, want)
	}
	if got := dedupeStrings(nil); got != nil {
		t.Errorf("dedupeStrings(nil) = %v, want nil", got)
	}
}
