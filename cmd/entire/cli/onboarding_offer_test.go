package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
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
		authed:      func(context.Context) bool { return f.loggedIn },
		probeMirror: func(context.Context, string, string) (bool, error) { return f.mirrored, nil },
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
func (f *fakeConnectState) offers(t *testing.T, ran *[]string) map[string]func(context.Context) error {
	t.Helper()
	return map[string]func(context.Context) error{
		onboarding.KeyAuth: func(context.Context) error {
			*ran = append(*ran, onboarding.KeyAuth)
			f.loggedIn = true
			return nil
		},
		onboarding.KeyMirror: func(context.Context) error {
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
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

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
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) { return setupModeSkip, nil }

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
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) { return setupModeStepByStep, nil }
	r.confirmRung = func(res onboarding.Result) (bool, error) {
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
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) {
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
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) {
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
	r.offerFns[onboarding.KeyMirror] = func(context.Context) error {
		return errors.New("cluster unreachable")
	}
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

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

// A mirror create returns before the server-side clone finishes, so the
// closing re-check can still read "not mirrored". A rung whose offer just
// succeeded must not render as missing with a retry hint.
func TestOnboardingLadder_SucceededOfferStillSyncingRendersInProgress(t *testing.T) {
	t.Parallel()
	f := &fakeConnectState{loggedIn: true}
	var ran []string
	r := newTestOffersRunner(f, &ran, t)
	//nolint:unparam // the offer-map signature requires an error return
	r.offerFns[onboarding.KeyMirror] = func(context.Context) error {
		ran = append(ran, "mirror")
		return nil // create succeeded — but f.mirrored stays false (clone lag)
	}
	r.promptMode = func([]onboarding.Result) (onboardingSetupMode, error) { return setupModeAll, nil }

	var out bytes.Buffer
	r.run(context.Background(), &out)

	if !strings.Contains(out.String(), "created — sync in progress") {
		t.Errorf("succeeded-but-syncing rung should render as in progress, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "✗ Repo mirrored") {
		t.Errorf("succeeded offer must not render as missing, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "run later: entire repo mirror create") {
		t.Errorf("succeeded offer must not print a retry hint, got:\n%s", out.String())
	}
}
