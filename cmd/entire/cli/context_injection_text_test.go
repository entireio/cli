package cli

import (
	"strings"
	"testing"
)

var injectionTestScope = trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"}

// The first-turn injection is a thin pointer at `entire agent-help` that names
// the auto-detected repo. The base text must NOT enumerate the command surface
// (flags/subcommands), which is what went stale when params were added; the
// trails paragraph names only the selector flags for working outside a checkout.
func TestEntireContextInjection_PointsAtAgentHelpWithRepo(t *testing.T) {
	t.Parallel()

	for _, trails := range []bool{false, true} {
		got := entireContextInjection(injectionTestScope, true, trails)

		for _, want := range []string{"entire agent-help"} {
			if !strings.Contains(got, want) {
				t.Fatalf("injection (trails=%v) missing %q:\n%s", trails, want, got)
			}
		}
		if !strings.HasPrefix(got, "Entire is enabled for this repo (`gh/acme/app`, auto-detected from the git origin remote). ") {
			t.Errorf("injection (trails=%v) should open by naming the repo:\n%s", trails, got)
		}
		if strings.Contains(got, "view, create, update, or watch") {
			t.Errorf("injection (trails=%v) should not enumerate subcommands:\n%s", trails, got)
		}
	}
	base := entireContextInjection(injectionTestScope, true, false)
	for _, unwanted := range []string{"--repo", "--project", "--branch"} {
		if strings.Contains(base, unwanted) {
			t.Errorf("base injection should not enumerate flags (%q):\n%s", unwanted, base)
		}
	}
}

// The injection carries invariants that hold on EVERY turn, not per-task command
// recommendations. An earlier revision urged `entire why <file>:<line>` and
// `entire checkpoint search` "before large edits"; a census of 963 agent
// transcripts found zero invocations of either against 25 calls to the
// agent-help pointer, so it only ever cost first-turn tokens while framing a
// sometimes-appropriate query as an always-do step. Which command suits a task
// is agent-help's job, where it is pulled on demand and grouped by audience.
func TestEntireContextInjection_OmitsPerTaskCommandRecommendations(t *testing.T) {
	t.Parallel()

	got := entireContextInjection(injectionTestScope, true, true)

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
		"entire agent-help", // the pointer that measurably converts
		"only create checkpoints manually when the user asks", // commits auto-capture
		"always ask the user before running it",               // setup/destructive commands
	} {
		if !strings.Contains(got, want) {
			t.Errorf("injection lost a standing invariant (%q):\n%s", want, got)
		}
	}
}

// When the repo can't be determined, the pointer still points at agent-help and
// opens without naming a repo, rather than emitting a malformed one.
func TestEntireContextInjection_NoRepo(t *testing.T) {
	t.Parallel()

	got := entireContextInjection(trailEnablementScope{}, true, false)

	if !strings.Contains(got, "entire agent-help") {
		t.Fatalf("missing agent-help pointer:\n%s", got)
	}
	if !strings.HasPrefix(got, "Entire is enabled for this repo. ") {
		t.Errorf("missing generic opening line:\n%s", got)
	}
	if strings.Contains(got, "//") {
		t.Errorf("malformed empty repo line:\n%s", got)
	}
}

// A partially-populated scope (e.g. forge+owner but no repo) must not emit a
// half-formed repo line — it falls back to the no-repo phrasing.
func TestEntireContextInjection_PartialScopeOmitsRepo(t *testing.T) {
	t.Parallel()

	got := entireContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme"}, true, false)

	if strings.Contains(got, "gh/acme") {
		t.Errorf("partial scope must not emit a repo line:\n%s", got)
	}
	if !strings.Contains(got, "entire agent-help") || !strings.HasPrefix(got, "Entire is enabled for this repo. ") {
		t.Errorf("partial scope must still point at agent-help with the generic opening line:\n%s", got)
	}
}

// The trails paragraph appears only when trails are enabled, sits between the
// agent-help pointer and the commit rule, asks for full trail/change URLs, and
// sends trail feedback to findings rather than comments.
func TestEntireContextInjection_TrailsParagraphOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	const trailLine = "A Trail is a project-level work item"
	without := entireContextInjection(injectionTestScope, true, false)
	if strings.Contains(without, trailLine) || strings.Contains(without, "entire trail") {
		t.Errorf("injection without trails must not describe trails:\n%s", without)
	}

	with := entireContextInjection(injectionTestScope, true, true)
	pointer := strings.Index(with, "entire agent-help")
	trail := strings.Index(with, trailLine)
	commits := strings.Index(with, "Commits capture")
	if pointer < 0 || trail < 0 || commits < 0 || pointer >= trail || trail >= commits {
		t.Errorf("want agent-help pointer, then trails, then commits:\n%s", with)
	}
	for _, want := range []string{"`entire trail update`", "`--project <forge>/<owner>`", "include its full URL", "findings (`entire trail finding`), never as trail comments"} {
		if !strings.Contains(with, want) {
			t.Errorf("trails paragraph missing %q:\n%s", want, with)
		}
	}
}

// When trails are decided on a later turn than the base text, only the trails
// paragraph is sent; the base text is not repeated.
func TestEntireContextInjection_TrailsOnly(t *testing.T) {
	t.Parallel()

	got := entireContextInjection(injectionTestScope, false, true)
	if !strings.HasPrefix(got, "A Trail is a project-level work item") {
		t.Errorf("trails-only injection should start with the trails paragraph:\n%s", got)
	}
	for _, unwanted := range []string{"entire agent-help", "Commits capture", "gh/acme/app"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("trails-only injection must not repeat the base text (%q):\n%s", unwanted, got)
		}
	}
}
