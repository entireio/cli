package review

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func newPostReviewTestCmd(out, errOut *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "review"}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd
}

func nonEmptyManifest() LocalReviewManifest {
	return LocalReviewManifest{
		Sources: []ManifestSource{{
			SessionID: "s1",
			Agent:     "claude-code",
			Label:     "Claude Code",
			Output:    "H1. Some finding\nDetails about the finding.",
		}},
	}
}

func TestFindingsCount_CountsHeadingsAcrossSources(t *testing.T) {
	t.Parallel()
	m := LocalReviewManifest{
		Sources: []ManifestSource{
			{Output: "H1. one\nbody\nH2. two\nmore"},
			{Output: "M1. three"},
		},
	}
	if got := findingsCount(m); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestFindingsCount_EmptyManifestZero(t *testing.T) {
	t.Parallel()
	if got := findingsCount(LocalReviewManifest{}); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestRunPostReviewFixPrompt_NoFindingsReturnsNilWithoutPrinting(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	called := false
	launch := func(_ context.Context, _ *cobra.Command, _ LocalReviewManifest, _, _ string, _ bool, _ func(error) error) error {
		called = true
		return nil
	}
	if err := runPostReviewFixPromptWithDeps(context.Background(), cmd, s, LocalReviewManifest{}, "", nil, false, launch, strings.NewReader(""), false); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("launch should not be called when there are no findings")
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty manifest, got: %q", out.String())
	}
}

func TestRunPostReviewFixPrompt_NoFixerPrintsSetupHint(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	// No agent has the Fixer/Both role.
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleReviewer},
	}}
	if err := runPostReviewFixPromptWithDeps(context.Background(), cmd, s, nonEmptyManifest(), "", nil, false, nil, strings.NewReader(""), false); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "no Fixer is configured") {
		t.Errorf("expected setup hint, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "entire review setup") {
		t.Errorf("expected setup pointer, got: %q", out.String())
	}
}

func TestRunPostReviewFixPrompt_NoFixerExplicitOmittedPrintsHintNotSetup(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleReviewer},
	}}
	if err := runPostReviewFixPromptWithDeps(context.Background(), cmd, s, nonEmptyManifest(), "", nil, true /* userExplicitlyOmittedFixer */, nil, strings.NewReader(""), false); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "--fixer <agent>") {
		t.Errorf("expected --fixer hint, got: %q", out.String())
	}
	if strings.Contains(out.String(), "entire review setup") {
		t.Errorf("setup nag should NOT appear when user explicitly omitted --fixer, got: %q", out.String())
	}
}

func TestRunPostReviewFixPrompt_AlwaysModeSkipsPromptAndLaunches(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{
		FixAfterReview: settings.FixAfterReviewAlways,
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Role: settings.RoleBoth},
		},
	}
	captured := false
	var capturedAll bool
	launch := func(_ context.Context, _ *cobra.Command, _ LocalReviewManifest, _, _ string, all bool, _ func(error) error) error {
		captured = true
		capturedAll = all
		return nil
	}
	if err := runPostReviewFixPromptWithDeps(context.Background(), cmd, s, nonEmptyManifest(), "", nil, false, launch, strings.NewReader(""), false); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !captured {
		t.Fatal("launch should have been called in always mode")
	}
	if !capturedAll {
		t.Errorf("always mode should launch with all=true")
	}
	if strings.Contains(out.String(), "Apply") {
		t.Errorf("always mode should not show the prompt, got: %q", out.String())
	}
}

func TestRunPostReviewFixPrompt_NonTTYPrintsFooter(t *testing.T) {
	t.Parallel()
	// canPrompt=false simulates the non-interactive call site (CI, piped
	// stdin). The helper should print the footer instead of reading keys.
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	launch := func(_ context.Context, _ *cobra.Command, _ LocalReviewManifest, _, _ string, _ bool, _ func(error) error) error {
		return nil
	}
	if err := runPostReviewFixPromptWithDeps(context.Background(), cmd, s, nonEmptyManifest(), "", nil, false, launch, strings.NewReader(""), false); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "Findings preserved on disk") {
		t.Errorf("expected footer, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "Run: entire review fix") {
		t.Errorf("expected Run: pointer, got: %q", out.String())
	}
}

// captureLaunch returns a postReviewFixLauncher that records whether it
// was called and the `all` argument it received. It's shared across the
// interactive-branch tests below.
type captureLaunch struct {
	called bool
	all    bool
}

func (c *captureLaunch) fn(_ context.Context, _ *cobra.Command, _ LocalReviewManifest, _, _ string, all bool, _ func(error) error) error {
	c.called = true
	c.all = all
	return nil
}

func TestRunPostReviewFixPrompt_InteractiveY_LaunchesAll(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	c := &captureLaunch{}
	if err := runPostReviewFixPromptWithDeps(
		context.Background(), cmd, s, nonEmptyManifest(), "", nil, false,
		c.fn, strings.NewReader("Y"), true,
	); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !c.called {
		t.Fatal("expected launcher to be called on [Y]")
	}
	if !c.all {
		t.Errorf("[Y] should launch with all=true, got all=%v", c.all)
	}
}

func TestRunPostReviewFixPrompt_InteractiveS_LaunchesSelect(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	c := &captureLaunch{}
	if err := runPostReviewFixPromptWithDeps(
		context.Background(), cmd, s, nonEmptyManifest(), "", nil, false,
		c.fn, strings.NewReader("s"), true,
	); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !c.called {
		t.Fatal("expected launcher to be called on [s]")
	}
	if c.all {
		t.Errorf("[s] should launch with all=false (delegates to selector), got all=%v", c.all)
	}
}

func TestRunPostReviewFixPrompt_InteractiveN_PrintsFooterNoLaunch(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	c := &captureLaunch{}
	if err := runPostReviewFixPromptWithDeps(
		context.Background(), cmd, s, nonEmptyManifest(), "", nil, false,
		c.fn, strings.NewReader("n"), true,
	); err != nil {
		t.Fatalf("err: %v", err)
	}
	if c.called {
		t.Errorf("[n] should not call the launcher, but it was called (all=%v)", c.all)
	}
	if !strings.Contains(out.String(), "Findings preserved on disk") {
		t.Errorf("[n] should print footer; got: %q", out.String())
	}
}

func TestRunPostReviewFixPrompt_InteractiveA_PersistsAlwaysAndLaunchesAll(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir(). The [A] branch
	// persists to clone-local preferences (via SaveClonePreferences),
	// which needs a real git common dir — hence testutil.InitRepo.
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)

	var out, errOut bytes.Buffer
	cmd := newPostReviewTestCmd(&out, &errOut)
	s := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleBoth},
	}}
	c := &captureLaunch{}
	if err := runPostReviewFixPromptWithDeps(
		context.Background(), cmd, s, nonEmptyManifest(), "", nil, false,
		c.fn, strings.NewReader("A"), true,
	); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !c.called {
		t.Fatal("expected launcher to be called on [A]")
	}
	if !c.all {
		t.Errorf("[A] should launch with all=true, got all=%v", c.all)
	}
	if s.FixAfterReview != settings.FixAfterReviewAlways {
		t.Errorf("[A] should set FixAfterReview = Always, got %q", s.FixAfterReview)
	}
	if strings.Contains(errOut.String(), "could not persist") {
		t.Errorf("unexpected persistence warning: %q", errOut.String())
	}
	// FixAfterReview must land in clone-local prefs (gitignored), NOT
	// .entire/settings.json. Writing to the committable file would trip
	// maybePromptReviewSettingsMigration on the next `entire review`.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("LoadClonePreferences: %v", err)
	}
	if prefs == nil || prefs.FixAfterReview != settings.FixAfterReviewAlways {
		t.Errorf("clone-local prefs should hold FixAfterReview=Always, got: %+v", prefs)
	}
	if _, projectRaw, exists, err := settings.LoadProjectRaw(context.Background()); err != nil {
		t.Fatalf("LoadProjectRaw: %v", err)
	} else if exists {
		if _, has := projectRaw["fix_after_review"]; has {
			t.Errorf("project settings.json contains fix_after_review key; would trigger legacy migration nudge")
		}
	}
}

func TestPrintFindingsFooter_Contents(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printFindingsFooter(&out, LocalReviewManifest{})
	got := out.String()
	for _, want := range []string{
		"Skipped",
		"Run: entire review fix",
		"--all",
		"entire review findings",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("footer missing %q:\n%s", want, got)
		}
	}
}
