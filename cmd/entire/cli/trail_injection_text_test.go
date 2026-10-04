package cli

import (
	"strings"
	"testing"
)

// The first-turn injection teaches the current-branch trail workflow and points
// at trail-specific agent help for the live command surface.
func TestEntireTrailContextInjection_TeachesTrailWorkflowWithRepo(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"})

	for _, want := range []string{
		"A trail ties together the context for a branch",
		"entire trail show",
		"entire trail create",
		"entire trail update",
		"entire trail finding",
		"entire trail watch",
		"entire agent-help trail",
		"entire agent-help trail <subcommand>",
		"gh/acme/app",
		"omit `--repo` unless targeting a different repo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trail injection missing %q:\n%s", want, got)
		}
	}
}

// The trail injection must not spend model-context tokens teaching unrelated
// Entire features or repeating the old general-purpose agent guidance.
func TestEntireTrailContextInjection_OnlyTalksAboutTrails(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"})

	for _, unwanted := range []string{
		"checkpoint",
		"entire why",
		"entire search",
		"enable, disable, clean, auth",
		"entire agent-help <command>",
		"what entire does",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("trail injection includes unrelated guidance %q:\n%s", unwanted, got)
		}
	}
}

// When the repo cannot be determined, trail guidance remains useful without
// emitting a malformed or partial repo identifier.
func TestEntireTrailContextInjection_UnknownRepo(t *testing.T) {
	t.Parallel()

	for _, scope := range []trailEnablementScope{
		{},
		{Forge: "gh", Owner: "acme"},
	} {
		got := entireTrailContextInjection(scope)
		if !strings.Contains(got, "entire agent-help trail") {
			t.Errorf("missing trail help pointer:\n%s", got)
		}
		if !strings.Contains(got, "Trail commands auto-detect the repo from the git origin remote") {
			t.Errorf("missing generic trail repo guidance:\n%s", got)
		}
		if strings.Contains(got, "gh/acme") || strings.Contains(got, "//") {
			t.Errorf("partial scope emitted a malformed repo identifier:\n%s", got)
		}
	}
}
