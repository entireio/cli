package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
)

func plainStyles() statusStyles {
	return statusStyles{colorEnabled: false, width: 80}
}

func resultFor(key, title string, check onboarding.Check) onboarding.Result {
	return onboarding.Result{Rung: onboarding.Rung{Key: key, Title: title}, Check: check}
}

// When login is the only actionable rung (a deliberate logout, or a local repo
// that can't be mirrored), the checklist reads as a reconnect: it points at
// the login command and drops the "Setup X/Y … finish setup → entire enable"
// onboarding framing, which wrongly presented a logout as unfinished setup.
func TestRenderOnboardingChecklist_LoginOnlyGapIsReconnect(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no GitHub origin"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "✗ Logged in") {
		t.Errorf("reconnect checklist should still show the login row, got:\n%s", out)
	}
	if !strings.Contains(out, "→ Run `entire auth login` to connect") {
		t.Errorf("reconnect checklist should point at the login command, got:\n%s", out)
	}
	if strings.Contains(out, "Setup ") || strings.Contains(out, "finish setup") {
		t.Errorf("login-only gap must not use the 'finish setup' framing, got:\n%s", out)
	}
}

// A logged-out repo that CAN mirror (GitHub origin) has a blocked mirror rung
// alongside the missing login, so more than login remains — it keeps the full
// "finish setup → entire enable" framing rather than the reconnect shortcut.
func TestRenderOnboardingChecklist_LogoutWithMirrorableRepoKeepsFinishSetup(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateBlocked, Detail: "needs login", Hint: "entire auth login"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "Setup 1/3 complete") {
		t.Errorf("mirrorable logged-out repo should keep the setup counter, got:\n%s", out)
	}
	if !strings.Contains(out, "→ Run `entire enable` to finish setup (2 steps left)") {
		t.Errorf("mirrorable logged-out repo should keep the enable pointer, got:\n%s", out)
	}
}

func TestRenderOnboardingChecklist_MidSetup(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code, Cursor"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateDone, Detail: "peyton"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{
			State: onboarding.StateMissing, Detail: "commits won't appear in the web UI",
			Hint: "entire repo mirror create github.com/acme/api",
		}),
		resultFor(onboarding.KeyImport, "History", onboarding.Check{
			State: onboarding.StateMissing, Detail: "12 claude-code sessions found, not imported",
			Hint: "entire import claude-code",
		}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	for _, want := range []string{
		"Setup 2/4 complete",
		"✓ Agent hooks",
		"Claude Code, Cursor",
		"✓ Logged in",
		"peyton",
		"✗ Repo mirrored",
		"commits won't appear in the web UI",
		"✗ History",
		"12 claude-code sessions found, not imported",
		"Run `entire enable` to finish setup (2 steps left)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q in:\n%s", want, out)
		}
	}
}

// Not parallel: pins the web base via ENTIRE_WEB_BASE_URL.
func TestRenderOnboardingChecklist_CollapsesWhenComplete(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.example")
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateDone, Detail: "peyton"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateDone, Detail: "github.com/acme/api"}),
		resultFor(onboarding.KeyImport, "History", onboarding.Check{State: onboarding.StateDone, Detail: "7 sessions imported"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "Connected: peyton · https://entire.example/gh/acme/api") {
		t.Errorf("complete checklist should collapse to identity + repo overview link, got:\n%s", out)
	}
	if strings.Contains(out, "Setup") || strings.Contains(out, "✗") {
		t.Errorf("complete checklist should not render rows or setup counter, got:\n%s", out)
	}
}

func TestRenderOnboardingChecklist_CollapseWithoutMirror(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateDone, Detail: "peyton"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no GitHub origin"}),
		resultFor(onboarding.KeyImport, "History", onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no prior history found"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "Connected: peyton") {
		t.Errorf("want connected line without mirror slug, got:\n%s", out)
	}
	if strings.Contains(out, "mirrored to") {
		t.Errorf("connected line must not mention mirror when not applicable, got:\n%s", out)
	}
}

func TestRenderOnboardingChecklist_BlockedAndUnknownRows(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateBlocked, Detail: "needs login", Hint: "entire auth login"}),
		resultFor(onboarding.KeyImport, "History", onboarding.Check{State: onboarding.StateUnknown}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "✗ Repo mirrored") || !strings.Contains(out, "needs login") {
		t.Errorf("blocked row should render with its detail, got:\n%s", out)
	}
	if !strings.Contains(out, "? History") {
		t.Errorf("unknown row should render with ? marker, got:\n%s", out)
	}
	// Unknown doesn't count as a remaining step; missing + blocked do.
	if !strings.Contains(out, "(2 steps left)") {
		t.Errorf("want 2 steps left (missing auth + blocked mirror), got:\n%s", out)
	}
}

func TestRenderOnboardingChecklist_SkipsNotApplicableRows(t *testing.T) {
	t.Parallel()
	// More than login is missing (auth + mirror), so this keeps the setup
	// counter — the login-only case renders as a reconnect instead. The
	// not-applicable import rung must still be skipped and excluded.
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateMissing, Detail: "commits won't appear in the web UI"}),
		resultFor(onboarding.KeyImport, "History", onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no prior history found"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if strings.Contains(out, "History") {
		t.Errorf("not-applicable rung should not render a row, got:\n%s", out)
	}
	if !strings.Contains(out, "Setup 1/3 complete") {
		t.Errorf("counter should exclude not-applicable rungs, got:\n%s", out)
	}
}

// An unknown rung that carries a hint (e.g. offline mirror probe) must
// surface it — otherwise an offline user gets a bare "?" row with nothing
// actionable and no footer, since unknown doesn't count as a missing step.
func TestRenderOnboardingChecklist_UnknownRowCarriesHint(t *testing.T) {
	t.Parallel()
	results := []onboarding.Result{
		resultFor(onboarding.KeyHooks, "Agent hooks", onboarding.Check{State: onboarding.StateDone, Detail: "Claude Code"}),
		resultFor(onboarding.KeyAuth, "Logged in", onboarding.Check{State: onboarding.StateDone, Detail: "peyton"}),
		resultFor(onboarding.KeyMirror, "Repo mirrored", onboarding.Check{State: onboarding.StateUnknown, Hint: "entire repo mirror list"}),
	}

	out := renderOnboardingChecklist(results, plainStyles())

	if !strings.Contains(out, "couldn't check — retry with `entire repo mirror list`") {
		t.Errorf("unknown row should carry its hint, got:\n%s", out)
	}
	if strings.Contains(out, "steps left") {
		t.Errorf("unknown rungs are not missing steps; no enable footer expected, got:\n%s", out)
	}
}
