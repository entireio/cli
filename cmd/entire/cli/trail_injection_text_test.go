package cli

import (
	"strings"
	"testing"
)

// The first-turn injection is now a thin pointer at `entire agent-help` that
// names the auto-detected repo and carries the no-ask rule — it must NOT
// enumerate the command surface (flags/subcommands), which is what went stale
// when params were added.
func TestEntireTrailContextInjection_PointsAtAgentHelpWithRepo(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"})

	for _, want := range []string{"entire agent-help", "gh/acme/app", "never ask"} {
		if !strings.Contains(got, want) {
			t.Fatalf("injection missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"--repo", "view, create, update, or watch"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("injection should not enumerate the command surface (%q):\n%s", unwanted, got)
		}
	}
}

// When the repo can't be determined, the pointer still points at agent-help and
// keeps the no-ask rule, without emitting a malformed repo line.
func TestEntireTrailContextInjection_NoRepo(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{})

	if !strings.Contains(got, "entire agent-help") {
		t.Fatalf("missing agent-help pointer:\n%s", got)
	}
	if !strings.Contains(got, "never ask") {
		t.Errorf("missing no-ask rule:\n%s", got)
	}
	if strings.Contains(got, "//") {
		t.Errorf("malformed empty repo line:\n%s", got)
	}
}

// A partially-populated scope (e.g. forge+owner but no repo) must not emit a
// half-formed repo line — it falls back to the no-repo phrasing.
func TestEntireTrailContextInjection_PartialScopeOmitsRepo(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme"})

	if strings.Contains(got, "gh/acme") {
		t.Errorf("partial scope must not emit a repo line:\n%s", got)
	}
	if !strings.Contains(got, "entire agent-help") || !strings.Contains(got, "never ask") {
		t.Errorf("partial scope must still point at agent-help with the no-ask rule:\n%s", got)
	}
}
