package review

import (
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func adapterFor(names ...string) func(string) reviewtypes.AgentReviewer {
	return func(name string) reviewtypes.AgentReviewer {
		for _, n := range names {
			if n == name {
				return pureHelperReviewer{name: name}
			}
		}
		return nil
	}
}

// An isolated reviewer registers Entire's hooks itself, so it doesn't need
// this checkout's; a marker-fallback agent (no adapter) applies no config, so
// it still does.
func TestReviewerHooksReady(t *testing.T) {
	t.Parallel()
	isolated := &settings.ReviewAgentConfig{}
	ready := reviewerHooksReady([]types.AgentName{"claude-code"}, adapterFor("claude-code", "codex"))
	for _, tc := range []struct {
		name   string
		worker string
		cfg    settings.ReviewConfig
		want   bool
	}{
		{"installed", "claude-code", settings.ReviewConfig{}, true},
		{"not installed", "codex", settings.ReviewConfig{Prompt: "p"}, false},
		{"isolated with adapter", "codex", settings.ReviewConfig{Config: isolated}, true},
		{"isolated worker alias", "strict", settings.ReviewConfig{Agent: "codex", Config: isolated}, true},
		{"isolated without adapter", "cursor", settings.ReviewConfig{Config: isolated}, false},
	} {
		if got := ready(tc.worker, tc.cfg); got != tc.want {
			t.Errorf("%s: ready = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestProfileGatesExemptIsolatedReviewers(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{Agents: map[string]settings.ReviewConfig{
		"claude-code": {Prompt: "p"},
		"codex":       {Config: &settings.ReviewAgentConfig{}},
		"cursor":      {Prompt: "p"},
	}}
	installed := []types.AgentName{"claude-code"}
	reviewerFor := adapterFor("claude-code", "codex")

	missing := missingInstalledProfileAgents(profile.Agents, reviewerHooksReady(installed, reviewerFor))
	if want := []string{reviewWorkerLabel("cursor", profile.Agents["cursor"])}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	var names []string
	for _, c := range ComputeEligibleConfiguredForProfile(profile, installed, reviewerFor) {
		names = append(names, c.Name)
	}
	if want := []string{"claude-code", "codex"}; !reflect.DeepEqual(names, want) {
		t.Errorf("eligible = %v, want %v", names, want)
	}
}
