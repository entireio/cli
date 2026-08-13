package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

const testConfigureUSHost = "us.entire.io"

func TestConfigureCmdBareInteractiveRunsOnboarding(t *testing.T) {
	setupTestRepo(t)
	t.Setenv(interactive.EnvTestTTY, "1")

	previous := runConfigureOnboarding
	t.Cleanup(func() { runConfigureOnboarding = previous })
	called := false
	runConfigureOnboarding = func(_ *cobra.Command, opts EnableOptions) error {
		called = true
		if !opts.Telemetry {
			t.Error("default configure options should keep telemetry enabled")
		}
		return nil
	}

	cmd := newSetupCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare configure: %v", err)
	}
	if !called {
		t.Fatal("bare interactive configure did not run onboarding")
	}
}

func TestEnsureConfigureLoginUsesExistingSession(t *testing.T) {
	profile := &authProfile{Handle: "dipree", Jurisdiction: "eu"}
	loginCalled := false
	deps := configureFlowDeps{
		resolveAuth: func(context.Context) (statusTarget, error) {
			return statusTarget{coreURL: "https://auth.example", token: "token"}, nil
		},
		fetchProfile: func(context.Context, string, string) (*authProfile, error) { return profile, nil },
		runLogin: func(context.Context, io.Writer, io.Writer) error {
			loginCalled = true
			return nil
		},
	}
	got, err := ensureConfigureLogin(context.Background(), io.Discard, io.Discard, deps)
	if err != nil {
		t.Fatalf("ensure login: %v", err)
	}
	if got != profile {
		t.Fatalf("profile = %#v, want existing profile", got)
	}
	if loginCalled {
		t.Fatal("login flow ran despite a valid session")
	}
}

func TestEnsureConfigureLoginCreatesMissingSession(t *testing.T) {
	var resolves int
	deps := configureFlowDeps{
		resolveAuth: func(context.Context) (statusTarget, error) {
			resolves++
			if resolves == 1 {
				return statusTarget{}, nil
			}
			return statusTarget{coreURL: "https://auth.example", token: "new-token"}, nil
		},
		fetchProfile: func(_ context.Context, _, token string) (*authProfile, error) {
			if token != "new-token" {
				return nil, errors.New("wrong token")
			}
			return &authProfile{Handle: "dipree", Jurisdiction: "eu"}, nil
		},
		runLogin: func(context.Context, io.Writer, io.Writer) error { return nil },
	}
	var out bytes.Buffer
	profile, err := ensureConfigureLogin(context.Background(), &out, io.Discard, deps)
	if err != nil {
		t.Fatalf("ensure login: %v", err)
	}
	if profile.Handle != "dipree" {
		t.Fatalf("handle = %q", profile.Handle)
	}
	if !strings.Contains(out.String(), "Logged in as dipree") {
		t.Fatalf("output = %q", out.String())
	}
}

type configureAccessReporterStub struct{}

func (configureAccessReporterStub) ReportEnable(context.Context, string) (*api.EnableRepoResponse, error) {
	return &api.EnableRepoResponse{Connected: false}, nil
}

func TestEnsureConfigureRepoAccessNonAdminPrintsShareableLink(t *testing.T) {
	opened := false
	deps := configureFlowDeps{
		githubAdmin: func(context.Context, string, string) (bool, error) { return false, nil },
		openURL: func(context.Context, string) error {
			opened = true
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := ensureConfigureRepoAccess(context.Background(), &out, &errOut, configureAccessReporterStub{},
		&api.EnableRepoResponse{InstallURL: "https://entire.io/install?repo=acme%2Fwidget"},
		"https://github.com/acme/widget.git", "acme", "widget", deps)
	if err == nil {
		t.Fatal("non-admin access branch should stop configure")
	}
	if opened {
		t.Fatal("non-admin branch must not open the installation URL")
	}
	for _, want := range []string{"Entire has no access to acme/widget", "An admin needs to install", "https://entire.io/install?repo=acme%2Fwidget"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestEnsureConfigureRepoAccessUnknownAdminStillAllowsInstall(t *testing.T) {
	opened := false
	deps := configureFlowDeps{
		githubAdmin: func(context.Context, string, string) (bool, error) {
			return false, errConfigureGHUnavailable
		},
		openURL: func(context.Context, string) error {
			opened = true
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	err := ensureConfigureRepoAccess(ctx, &out, &errOut, configureAccessReporterStub{},
		&api.EnableRepoResponse{InstallURL: "https://entire.io/install?repo=acme%2Fwidget"},
		"https://github.com/acme/widget.git", "acme", "widget", deps)
	if err == nil {
		t.Fatal("expected the installation wait to end with the test deadline")
	}
	if !opened {
		t.Fatal("an unknown admin state should still open the installation URL")
	}
	if !strings.Contains(errOut.String(), "gh CLI is not installed") {
		t.Fatalf("missing actionable gh guidance:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "An admin needs to install") {
		t.Fatalf("unknown admin state was misreported as non-admin:\n%s", errOut.String())
	}
}

func TestConfigureOnboardingSuppressesTelemetryPrompt(t *testing.T) {
	opts := configureOnboardingEnableOptions(EnableOptions{Telemetry: true})
	if !opts.Yes {
		t.Fatal("onboarding must auto-answer setup choices it already presented")
	}
	if !opts.SuppressAdditionalSetup {
		t.Fatal("onboarding must suppress telemetry and unrelated setup prompts")
	}
}

func TestConfigureFieldFocusChangesActiveQuestion(t *testing.T) {
	selected := testConfigureUSHost
	upstream := &configureUpstreamField{Select: huh.NewSelect[string]().Options(huh.NewOption("US", selected)).Value(&selected)}
	if cmd := upstream.Focus(); cmd == nil {
		t.Fatal("focus must schedule a title refresh")
	}
	if !upstream.focused {
		t.Fatal("upstream did not become focused")
	}
	active := configureQuestionTitle("Upstream", upstream.focused)
	upstream.Blur()
	inactive := configureQuestionTitle("Upstream", upstream.focused)
	if active == inactive {
		t.Fatal("active and inactive section titles are visually identical")
	}
}

func TestConfigureUpstreamFieldEnterAdvancesToAgents(t *testing.T) {
	selected := testConfigureUSHost
	field := &configureUpstreamField{
		Select: huh.NewSelect[string]().
			Options(huh.NewOption("US", selected)).
			Value(&selected),
	}
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not emit a transition to the agent field")
	}
}

func TestConfigureUpstreamArrowsMoveCursorWithoutSelecting(t *testing.T) {
	const euHost = "eu.entire.io"
	selected := euHost
	field := &configureUpstreamField{
		value:       &selected,
		committed:   selected,
		highlighted: selected,
	}
	field.Select = huh.NewSelect[string]().
		Options(
			huh.NewOption("US", testConfigureUSHost),
			huh.NewOption("EU", euHost),
		).
		Value(&selected)
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd != nil {
		t.Fatal("down arrow emitted a section transition")
	}
	if selected != euHost {
		t.Fatalf("down arrow changed radio selection to %q", selected)
	}
	if field.highlighted != testConfigureUSHost {
		t.Fatalf("down arrow cursor = %q, want US", field.highlighted)
	}

	field.Update(tea.KeyPressMsg{Code: ' '})
	if selected != testConfigureUSHost {
		t.Fatalf("space did not intentionally select highlighted radio: %q", selected)
	}
}

func TestConfigureAgentFieldHasActiveCursorAndEnterAdvances(t *testing.T) {
	selected := []string{"claude-code"}
	field := &configureAgentField{MultiSelect: huh.NewMultiSelect[string]().
		Options(huh.NewOption("Claude Code", "claude-code")).
		Value(&selected)}
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.WithPosition(huh.FieldPosition{Field: 1, FirstField: 0, LastField: 2, LastGroup: 0})
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not advance from agents to save")
	}
}

func TestConfigureSaveFieldEnterSubmits(t *testing.T) {
	choice := configureSaveNewBranch
	field := &configureSaveField{Select: huh.NewSelect[string]().
		Options(huh.NewOption("Push to a new branch", choice)).
		Value(&choice)}
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.WithPosition(huh.FieldPosition{Field: 2, FirstField: 0, LastField: 2, LastGroup: 0})
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not submit the save field")
	}
}

func TestConfigureUpstreamOptionsUsePrototypeLabels(t *testing.T) {
	placements := []coreapi.ResolvedPlacement{
		{ClusterHost: "aws-ap-southeast-2.entire.io", Jurisdiction: coreapi.NewOptString("au")},
		{ClusterHost: "aws-eu-central-1.entire.io", Jurisdiction: coreapi.NewOptString("eu")},
		{ClusterHost: "aws-us-east-2.entire.io", Jurisdiction: coreapi.NewOptString("us")},
	}
	options := configureUpstreamOptions(placements, placements[1].ClusterHost, placements[0].ClusterHost)
	got := make([]string, len(options))
	for i, option := range options {
		got[i] = ansi.Strip(option.Key)
	}
	want := []string{"○ AU — current", "● EU", "○ US"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("option labels = %v, want %v", got, want)
	}
}

func TestConfigurePlacementOrderPrefersCurrentThenHome(t *testing.T) {
	placements := []coreapi.ResolvedPlacement{
		{ClusterHost: "us.entire.io", Jurisdiction: coreapi.NewOptString("us")},
		{ClusterHost: "eu.entire.io", Jurisdiction: coreapi.NewOptString("eu")},
		{ClusterHost: "au.entire.io", Jurisdiction: coreapi.NewOptString("au")},
	}
	got := configurePlacementOrder(placements, "eu.entire.io", "au")
	if got[0].ClusterHost != "eu.entire.io" {
		t.Fatalf("first placement = %s, want current EU upstream", got[0].ClusterHost)
	}
	got = configurePlacementOrder(placements, "", "au")
	if got[0].ClusterHost != "au.entire.io" {
		t.Fatalf("first placement = %s, want home AU upstream", got[0].ClusterHost)
	}
}

func TestConfigureFormThemeHasNoFocusedRail(t *testing.T) {
	styles := configureFormTheme().Theme(true)
	if got := styles.Focused.Base.Render("upstream"); got != "upstream" {
		t.Fatalf("focused field renders %q; configure theme should not add a rail", got)
	}
	if got := ansi.Strip(styles.Focused.SelectSelector.String()); got != "> " {
		t.Fatalf("radio cursor = %q, want active-row indicator only", got)
	}
	if got := ansi.Strip(styles.Focused.MultiSelectSelector.String()); got != "> " {
		t.Fatalf("agent cursor = %q, want the same active-row indicator", got)
	}
	if got := ansi.Strip(styles.Focused.SelectedPrefix.String()); got != "◼ " {
		t.Fatalf("selected agent marker = %q, want prototype checkbox", got)
	}
}

func TestConfigureSaveIsSkippedWithoutGeneratedChanges(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := configureSaveAndPush(cmd, t.TempDir(), "owner", "repo", nil, nil, "main", false, configureSaveDirect, time.Now); err != nil {
		t.Fatalf("configureSaveAndPush() error = %v", err)
	}
	if strings.Contains(out.String(), "Save configuration") {
		t.Fatalf("save prompt appeared without changes: %q", out.String())
	}
	if !strings.Contains(out.String(), "No new configuration changes") {
		t.Fatalf("missing unchanged message: %q", out.String())
	}
}

func TestConfigureSelectionChangesDistinguishLocalAndPushChanges(t *testing.T) {
	tests := []struct {
		name                      string
		selectedHost, currentHost string
		selectedAgents, installed []string
		wantMirror, wantAgents    bool
	}{
		{
			name:         "unchanged",
			selectedHost: "eu.entire.io", currentHost: "eu.entire.io",
			selectedAgents: []string{"claude-code"}, installed: []string{"claude-code"},
		},
		{
			name:         "mirror only",
			selectedHost: "au.entire.io", currentHost: "eu.entire.io",
			selectedAgents: []string{"claude-code"}, installed: []string{"claude-code"},
			wantMirror: true,
		},
		{
			name:         "agents require push",
			selectedHost: "eu.entire.io", currentHost: "eu.entire.io",
			selectedAgents: []string{"claude-code", "codex"}, installed: []string{"claude-code"},
			wantAgents: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mirror, agents := configureSelectionChanges(tt.selectedHost, tt.currentHost, tt.selectedAgents, tt.installed)
			if mirror != tt.wantMirror || agents != tt.wantAgents {
				t.Fatalf("changes = (mirror %v, agents %v), want (%v, %v)", mirror, agents, tt.wantMirror, tt.wantAgents)
			}
		})
	}
}

func TestConfigureSaveOptionsAreDynamic(t *testing.T) {
	push := configureSaveOptions("main", false, true, configureSaveDirect)
	got := []string{ansi.Strip(push[0].Key), ansi.Strip(push[1].Key), ansi.Strip(push[2].Key)}
	want := []string{
		"● Save — push to main",
		"○ Save — push to a new branch, review before it lands",
		"○ Cancel",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("push save options = %v, want %v", got, want)
	}

	local := configureSaveOptions("main", false, false, configureSaveLocal)
	got = []string{ansi.Strip(local[0].Key), ansi.Strip(local[1].Key)}
	want = []string{"● Save", "○ Cancel"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local save options = %v, want %v", got, want)
	}

	protected := configureSaveOptions("main", true, true, configureSaveNewBranch)
	if len(protected) != 2 {
		t.Fatalf("protected save options = %d, want new branch and cancel", len(protected))
	}
	if got := ansi.Strip(protected[0].Key); got != "● Save — push to a new branch, review before it lands" {
		t.Fatalf("selected new-branch option = %q", got)
	}
	description := ansi.Strip(configureSaveDescription("main", true, true))
	if !strings.Contains(description, "○ Save — push to main — protected branch") {
		t.Fatalf("protected destination is not shown as disabled: %q", description)
	}
}

func TestNewConfigureChangesExcludesPreexistingWork(t *testing.T) {
	before := map[string]string{"README.md": " M", "already-staged": "M "}
	after := map[string]string{
		"README.md":             " M",
		"already-staged":        "M ",
		".entire/settings.json": "??",
		".claude/settings.json": " M",
	}
	got := newConfigureChanges(before, after)
	want := []string{".claude/settings.json", ".entire/settings.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new changes = %v, want %v", got, want)
	}
}
