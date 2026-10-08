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
	// This branch originally required the injection to name `entire search`, to
	// pin the canonical spelling. Main has since dropped per-task command
	// recommendations from the injection entirely (see
	// TestEntireTrailContextInjection_OmitsPerTaskCommandRecommendations), so
	// there is no search mention left to spell either way. The spelling is still
	// pinned everywhere it does appear — see the surfaces asserted in
	// root_test.go and setup_search_skill.
	if strings.Contains(got, "checkpoint search") {
		t.Errorf("injection must not use the old `checkpoint search` spelling:\n%s", got)
	}
}

// The injection carries invariants that hold on EVERY turn, not per-task command
// recommendations. An earlier revision urged `entire why <file>:<line>` and
// `entire checkpoint search` "before large edits"; a census of 963 agent
// transcripts found zero invocations of either against 25 calls to the
// agent-help pointer, so it only ever cost first-turn tokens while framing a
// sometimes-appropriate query as an always-do step. Which command suits a task
// is agent-help's job, where it is pulled on demand and grouped by audience.
func TestEntireTrailContextInjection_OmitsPerTaskCommandRecommendations(t *testing.T) {
	t.Parallel()

	got := entireTrailContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"})

	for _, unwanted := range []string{
		"entire why",
		"checkpoint search",
		"Before large edits",
		"recover the intent",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("injection must not recommend per-task commands (%q):\n%s", unwanted, got)
		}
	}

	// The invariants that DO belong stay: they change what an agent does on every
	// turn, and no drill-down is required to act on them.
	for _, want := range []string{
		"entire agent-help",  // the pointer that measurably converts
		"never create check", // commits auto-capture; don't hand-roll
		"Leave setup and destructive commands",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("injection lost a standing invariant (%q):\n%s", want, got)
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
