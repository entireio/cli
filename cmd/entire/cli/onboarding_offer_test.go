package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// Fixture identifiers shared across the onboarding test files.
const (
	testOwnerAcme = "acme"
	testCtxProd   = "prod"
	testRepoAPI   = "api"
)

// fakeConnectState simulates ground truth that flips as offers run: login
// starts missing, and the mirror probe reports unmirrored until created.
type fakeConnectState struct {
	loggedIn bool
	mirrored bool
}

func (f *fakeConnectState) deps() onboardingRungDeps {
	deps := onboardingRungDeps{
		installedAgents: func(context.Context) []string { return []string{"Claude Code"} },
		envToken:        func() string { return "" },
		resolveOrigin: func(context.Context) (string, string, string, error) {
			return "gh", testOwnerAcme, testRepoAPI, nil
		},
		authed: func(context.Context) bool { return f.loggedIn },
		probeMirror: func(context.Context, string, string) (mirrorProbeResult, error) {
			return mirrorProbeResult{Mirrored: f.mirrored}, nil
		},
		discoverImports: func(context.Context) ([]agentImportStatus, error) {
			return nil, nil // no history: import rung not applicable
		},
	}
	deps.listContexts = func() ([]*contexts.Context, string, error) {
		if !f.loggedIn {
			return nil, "", nil
		}
		return []*contexts.Context{{Name: testCtxProd, Handle: "peyton"}}, testCtxProd, nil
	}
	deps.tokenForContext = func(*contexts.Context) (string, error) { return "jwt", nil }
	return deps
}

//nolint:unparam // the offer-map signature requires an error return
func (f *fakeConnectState) offers(t *testing.T, ran *[]string) map[string]func(context.Context, bool) error {
	t.Helper()
	return map[string]func(context.Context, bool) error{
		onboarding.KeyAuth: func(context.Context, bool) error {
			*ran = append(*ran, onboarding.KeyAuth)
			f.loggedIn = true
			return nil
		},
		onboarding.KeyMirror: func(context.Context, bool) error {
			*ran = append(*ran, onboarding.KeyMirror)
			f.mirrored = true
			return nil
		},
	}
}

func newTestOffersRunner(f *fakeConnectState, ran *[]string, t *testing.T) onboardingOfferRunner {
	return onboardingOfferRunner{
		deps:      f.deps(),
		offerFns:  f.offers(t, ran),
		canPrompt: func() bool { return true },
		styles:    func(io.Writer) statusStyles { return plainStyles() },
	}
}

func TestOnboardingLadder_ModeAll_RunsOffersAndUnblocks(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	// Auth first, which unblocks mirror in the same pass.
	if len(ran) != 2 || ran[0] != onboarding.KeyAuth || ran[1] != onboarding.KeyMirror {
		t.Errorf("offers ran = %v, want [auth mirror]", ran)
	}
	if !strings.Contains(out.String(), "Connected") {
		t.Errorf("closing checklist should be fully connected, got:\n%s", out.String())
	}
}

func TestOnboardingLadder_ModeSkip_PrintsHintsWithoutOffers(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) { return setupModeSkip, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none on skip", ran)
	}
	if !strings.Contains(out.String(), "Setup 1/3 complete") {
		t.Errorf("skip should print the checklist, got:\n%s", out.String())
	}
}

func TestOnboardingLadder_StepByStep_HonorsDecline(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		return setupModeStepByStep, nil
	}
	r.confirmRung = func(_ context.Context, res onboarding.Result) (bool, error) {
		return res.Rung.Key != onboarding.KeyAuth, nil // decline login, accept the rest
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	// Auth declined; mirror stays blocked so its offer never fires.
	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none (auth declined blocks mirror)", ran)
	}
	if !strings.Contains(out.String(), "entire auth login") {
		t.Errorf("closing checklist should carry the login hint, got:\n%s", out.String())
	}
}

func TestOnboardingLadder_NonInteractive_NeverPromptsOrOffers(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.canPrompt = func() bool { return false }
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		t.Error("promptMode must not be called non-interactively")
		return setupModeSkip, nil
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none non-interactively", ran)
	}
	if !strings.Contains(out.String(), "Setup 1/3 complete") {
		t.Errorf("non-interactive should still print the checklist, got:\n%s", out.String())
	}
}

func TestOnboardingLadder_FullyConnected_NoPromptCollapsedLine(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true, mirrored: true}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		t.Error("promptMode must not be called when nothing is missing")
		return setupModeSkip, nil
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none when connected", ran)
	}
	if !strings.Contains(out.String(), "Connected") {
		t.Errorf("want collapsed connected line, got:\n%s", out.String())
	}
}

func TestOnboardingLadder_OfferFailureIsBestEffort(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true} // only mirror left to do
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.offerFns[onboarding.KeyMirror] = func(context.Context, bool) error {
		return errors.New("cluster unreachable")
	}
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if !strings.Contains(out.String(), "cluster unreachable") {
		t.Errorf("offer failure should surface as a notice, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "entire repo mirror create github.com/acme/api") {
		t.Errorf("closing checklist should carry the retry hint, got:\n%s", out.String())
	}
}

// A blocked rung whose fix is another rung's command (mirror needs login)
// must not repeat that command; hints are deduped in ladder order.
func TestOnboardingLadder_RunLaterHintsAreDeduped(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.canPrompt = func() bool { return false }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if got := strings.Count(out.String(), "entire auth login"); got != 1 {
		t.Errorf("login hint appears %d times, want exactly 1:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "run later: entire auth login") {
		t.Errorf("want 'run later:' hint phrasing, got:\n%s", out.String())
	}
}

// The import rung carries an inline offer: "set up everything" must carry the
// ladder through history import, not dead-end at 3/4 with a hint.
func TestOnboardingLadder_ModeAll_RunsImportOffer(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true, mirrored: true}
	imported := false
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.deps.discoverImports = func(context.Context) ([]agentImportStatus, error) {
		status := agentImportStatus{Agent: "claude-code", Sessions: 1}
		if !imported {
			status.UnimportedTurns = 3
		}
		return []agentImportStatus{status}, nil
	}
	//nolint:unparam // the offer-map signature requires an error return
	r.offerFns[onboarding.KeyImport] = func(context.Context, bool) error {
		ran = append(ran, onboarding.KeyImport)
		imported = true
		return nil
	}
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 1 || ran[0] != onboarding.KeyImport {
		t.Errorf("offers ran = %v, want [import]", ran)
	}
	if !strings.Contains(out.String(), "Connected") {
		t.Errorf("ladder should be fully connected after import, got:\n%s", out.String())
	}
}

// Production wiring must include the import offer alongside auth and mirror.
func TestNewOnboardingOfferRunner_WiresImportOffer(t *testing.T) {
	t.Parallel()
	r := newOnboardingOfferRunner(io.Discard, nil)
	for _, key := range []string{onboarding.KeyAuth, onboarding.KeyMirror, onboarding.KeyImport} {
		if r.offerFns[key] == nil {
			t.Errorf("offerFns[%q] = nil, want an offer wired", key)
		}
	}
}

// A suspended placement returns nil error from createAndAwaitMirror; the
// offer must not report it as success or write-through the probe cache —
// otherwise the checklist shows ✓ for an unusable mirror and the cache
// suppresses re-offering for the TTL.
func TestFinalizeMirrorOffer_SuspendedIsAnErrorAndNotCached(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	outcome := mirrorCreateOutcome{created: &coreapi.CreatedMirror{Suspended: true}}

	err := finalizeMirrorOffer(&out, outcome)

	if err == nil {
		t.Error("suspended placement must be an error so the rung keeps its retry hint")
	}
	if !strings.Contains(out.String(), "suspended") {
		t.Errorf("user should be told the mirror is suspended, got:\n%s", out.String())
	}
}

func TestFinalizeMirrorOffer_SuccessCachesAndReportsCloning(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	outcome := mirrorCreateOutcome{created: &coreapi.CreatedMirror{}}

	if err := finalizeMirrorOffer(&out, outcome); err != nil {
		t.Fatalf("finalizeMirrorOffer error = %v", err)
	}
	if !strings.Contains(out.String(), "clone continues in the background") {
		t.Errorf("successful placement should mention the background clone, got:\n%s", out.String())
	}
}

// Cancelling the consent prompt (Ctrl-C / form error) must behave like skip:
// no offers run, the checklist still prints. A regression here would run
// login and mirror creation without consent.
func TestOnboardingLadder_ConsentCancelBehavesLikeSkip(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		return setupModeAll, errors.New("user aborted")
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none after a cancelled prompt", ran)
	}
	if !strings.Contains(out.String(), "Setup 1/3 complete") {
		t.Errorf("checklist should still print after cancel, got:\n%s", out.String())
	}
}

// Step-by-step accept must actually run the confirmed rung's offer.
func TestOnboardingLadder_StepByStep_AcceptRunsOffer(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true} // only mirror missing
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		return setupModeStepByStep, nil
	}
	r.confirmRung = func(context.Context, onboarding.Result) (bool, error) { return true, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 1 || ran[0] != onboarding.KeyMirror {
		t.Errorf("offers ran = %v, want [mirror] on accept", ran)
	}
}

// A blocked rung is only offerable when the rung blocking it can itself be
// offered this pass. If auth is Unknown (e.g. unreadable context store),
// promising "mirrors this repo" in the consent prompt would be a lie — the
// mirror rung stays blocked and nothing would run.
func TestOnboardingLadder_BlockedRungNotOfferableWithoutItsBlocker(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.deps.listContexts = func() ([]*contexts.Context, string, error) {
		return nil, "", errors.New("contexts.json: corrupt") // auth → Unknown
	}
	r.promptMode = func(_ context.Context, missing []onboarding.Result) (onboardingSetupMode, error) {
		t.Errorf("promptMode called with %d rungs; nothing is actionable (auth unknown, mirror blocked)", len(missing))
		return setupModeSkip, nil
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none", ran)
	}
}

// --yes ("accept all defaults") auto-runs the import offer — import is
// local-only — while login and mirror stay strictly opt-in.
func TestOnboardingLadder_AutoRunImportsWithoutPrompting(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	imported := false
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.canPrompt = func() bool { return false }
	r.autoRun = map[string]bool{onboarding.KeyImport: true}
	r.deps.discoverImports = func(context.Context) ([]agentImportStatus, error) {
		status := agentImportStatus{Agent: "claude-code", Sessions: 2}
		if !imported {
			status.UnimportedTurns = 2
		}
		return []agentImportStatus{status}, nil
	}
	//nolint:unparam // the offer-map signature requires an error return
	r.offerFns[onboarding.KeyImport] = func(_ context.Context, granular bool) error {
		if granular {
			t.Error("auto-run offers must not be granular (no prompting available)")
		}
		ran = append(ran, onboarding.KeyImport)
		imported = true
		return nil
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 1 || ran[0] != onboarding.KeyImport {
		t.Errorf("offers ran = %v, want [import] only — login/mirror are never implicit", ran)
	}
}

// Without autoRun (non-interactive without --yes), nothing runs at all.
func TestOnboardingLadder_NoAutoRunWithoutYes(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.canPrompt = func() bool { return false }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none non-interactively without --yes", ran)
	}
}

// The import offer presents its own picker in step-by-step mode, so the
// generic yes/no confirm is skipped for it (no double-asking) and the offer
// receives granular=true.
func TestOnboardingLadder_StepByStep_SelfPromptingImportSkipsConfirm(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true, mirrored: true}
	imported := false
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	r.selfPrompting = map[string]bool{onboarding.KeyImport: true}
	r.deps.discoverImports = func(context.Context) ([]agentImportStatus, error) {
		status := agentImportStatus{Agent: "claude-code", Sessions: 1}
		if !imported {
			status.UnimportedTurns = 1
		}
		return []agentImportStatus{status}, nil
	}
	//nolint:unparam // the offer-map signature requires an error return
	r.offerFns[onboarding.KeyImport] = func(_ context.Context, granular bool) error {
		if !granular {
			t.Error("step-by-step import offer must be granular (its picker is the confirm)")
		}
		ran = append(ran, onboarding.KeyImport)
		imported = true
		return nil
	}
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		return setupModeStepByStep, nil
	}
	r.confirmRung = func(_ context.Context, res onboarding.Result) (bool, error) {
		t.Errorf("generic confirm called for self-prompting rung %q", res.Rung.Key)
		return false, nil
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if len(ran) != 1 || ran[0] != onboarding.KeyImport {
		t.Errorf("offers ran = %v, want [import]", ran)
	}
}

// A failed rung confirm (Ctrl-C, broken terminal) means the user is bailing
// out: the walk must stop instead of surfacing every remaining prompt.
func TestOnboardingLadder_ConfirmErrorStopsTheWalk(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{}
	var ran []string
	confirms := 0
	r := newTestOffersRunner(f, &ran, t)
	r.promptMode = func(context.Context, []onboarding.Result) (onboardingSetupMode, error) {
		return setupModeStepByStep, nil
	}
	r.confirmRung = func(context.Context, onboarding.Result) (bool, error) {
		confirms++
		return false, errors.New("user aborted")
	}

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if confirms != 1 {
		t.Errorf("confirm prompts shown = %d, want 1 (abort must stop the walk)", confirms)
	}
	if len(ran) != 0 {
		t.Errorf("offers ran = %v, want none after abort", ran)
	}
}

// The option→runner mapping: --yes auto-imports only on the very first
// enable; any later --yes must not import history the user may have declined.
func TestNewEnableOnboardingRunner_AutoRunGatedOnFirstRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    enableOnboardingOpts
		autoRun bool
		prompts bool
	}{
		{"first-run --yes", enableOnboardingOpts{assumeYes: true, firstRun: true}, true, false},
		{"re-enable --yes", enableOnboardingOpts{assumeYes: true}, false, false},
		{"first-run interactive", enableOnboardingOpts{firstRun: true}, false, true},
		{"--agent without --yes", enableOnboardingOpts{neverPrompt: true, firstRun: true}, false, false},
	}
	for _, tc := range cases {
		r := newEnableOnboardingRunner(io.Discard, tc.opts)
		if got := r.autoRun[onboarding.KeyImport]; got != tc.autoRun {
			t.Errorf("%s: autoRun[import] = %v, want %v", tc.name, got, tc.autoRun)
		}
		if tc.prompts && r.canPrompt == nil {
			t.Errorf("%s: canPrompt unexpectedly overridden", tc.name)
		}
		if !tc.prompts && !tc.autoRun {
			if r.canPrompt() {
				t.Errorf("%s: canPrompt() = true, want prompting suppressed", tc.name)
			}
		}
	}
}
