package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	testConfigureUSHost = "us.entire.io"
	testGitRevParse     = "rev-parse"
	testConfigureSHA    = "abc123"
)

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

func TestConfigureCmdYesRunsOnboardingWithoutTTY(t *testing.T) {
	setupTestRepo(t)
	t.Setenv(interactive.EnvTestTTY, "0")

	previous := runConfigureOnboarding
	t.Cleanup(func() { runConfigureOnboarding = previous })
	called := false
	runConfigureOnboarding = func(_ *cobra.Command, opts EnableOptions) error {
		called = true
		if !opts.Yes {
			t.Error("--yes was not passed to onboarding")
		}
		return nil
	}

	cmd := newSetupCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --yes: %v", err)
	}
	if !called {
		t.Fatal("configure --yes did not run non-interactive onboarding")
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

func TestEnsureConfigureLoginRejectsInsecureURLBeforeSendingToken(t *testing.T) {
	fetchCalled := false
	loginCalled := false
	deps := configureFlowDeps{
		resolveAuth: func(context.Context) (statusTarget, error) {
			return statusTarget{coreURL: "http://auth.example", token: "secret"}, nil
		},
		fetchProfile: func(context.Context, string, string) (*authProfile, error) {
			fetchCalled = true
			return nil, errors.New("profile fetch should not run")
		},
		runLogin: func(context.Context, io.Writer, io.Writer) error {
			loginCalled = true
			return nil
		},
	}
	_, err := ensureConfigureLogin(context.Background(), io.Discard, io.Discard, deps)
	if !errors.Is(err, api.ErrInsecureHTTP) {
		t.Fatalf("error = %v, want ErrInsecureHTTP", err)
	}
	if fetchCalled {
		t.Fatal("profile request sent bearer to insecure URL")
	}
	if loginCalled {
		t.Fatal("insecure existing session incorrectly started login")
	}
}

func TestEnsureConfigureLoginRejectsInsecureURLAfterLogin(t *testing.T) {
	var resolves int
	fetchCalled := false
	deps := configureFlowDeps{
		resolveAuth: func(context.Context) (statusTarget, error) {
			resolves++
			if resolves == 1 {
				return statusTarget{}, nil
			}
			return statusTarget{coreURL: "http://auth.example", token: "secret"}, nil
		},
		fetchProfile: func(context.Context, string, string) (*authProfile, error) {
			fetchCalled = true
			return nil, errors.New("profile fetch should not run")
		},
		runLogin: func(context.Context, io.Writer, io.Writer) error { return nil },
	}
	_, err := ensureConfigureLogin(context.Background(), io.Discard, io.Discard, deps)
	if !errors.Is(err, api.ErrInsecureHTTP) {
		t.Fatalf("error = %v, want ErrInsecureHTTP", err)
	}
	if fetchCalled {
		t.Fatal("post-login profile request sent bearer to insecure URL")
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
		"https://github.com/acme/widget.git", "acme", "widget", false, deps)
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
		"https://github.com/acme/widget.git", "acme", "widget", false, deps)
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

func TestConfigureSuccessfulMirrorPlacementsKeepsPartialSuccess(t *testing.T) {
	selected := []regionChoice{
		{host: "au.entire.io", slug: "au", jurisdiction: "au"},
		{host: "eu.entire.io", slug: "eu", jurisdiction: "eu"},
	}
	results := []mirrorResult{
		{cloneURL: "entire://au.entire.io/gh/acme/widget", status: mirrorStatusReady},
		{status: mirrorStatusError, err: errors.New("region unavailable")},
	}
	placements := configureSuccessfulMirrorPlacements(selected, results)
	if len(placements) != 1 {
		t.Fatalf("successful placements = %d, want 1", len(placements))
	}
	if placements[0].ClusterHost != "au.entire.io" {
		t.Fatalf("successful placement host = %q", placements[0].ClusterHost)
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
	want := []string{"○ Australia — current", "● European Union", "○ United States"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("option labels = %v, want %v", got, want)
	}
}

func TestConfigurePlacementLabelUsesProductionRegionNames(t *testing.T) {
	tests := []struct {
		jurisdiction string
		want         string
	}{
		{jurisdiction: "au", want: "Australia"},
		{jurisdiction: "eu", want: "European Union"},
		{jurisdiction: "in", want: "India"},
		{jurisdiction: "us", want: "United States"},
	}
	for _, tt := range tests {
		placement := coreapi.ResolvedPlacement{Jurisdiction: coreapi.NewOptString(tt.jurisdiction)}
		if got := configurePlacementLabel(placement); got != tt.want {
			t.Errorf("configurePlacementLabel(%q) = %q, want %q", tt.jurisdiction, got, tt.want)
		}
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

func TestConfigureSaveFieldIsAlwaysVisible(t *testing.T) {
	choice := configureSaveLocal
	field := &configureSaveField{}
	field.Select = huh.NewSelect[string]().
		Title("Save configuration").
		Description(configureSaveDescription("main", false, false, false)).
		Options(configureSaveOptions("main", false, false, false, choice)...).
		Value(&choice)
	got := ansi.Strip(field.View())
	if !strings.Contains(got, "Save configuration") || !strings.Contains(got, "Save — no changes") {
		t.Fatalf("save field does not show its disabled no-change state:\n%s", got)
	}
}

func TestConfigureSaveIsSkippedWithoutGeneratedChanges(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := configureSaveAndPush(cmd, t.TempDir(), "owner", "repo", nil, nil, "main", false, configureSaveDirect, mirrorCloneProviderGitHub, time.Now); err != nil {
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

func TestConfigureBranchHasNoUnpushedCommits(t *testing.T) {
	previous := gitRunner
	t.Cleanup(func() { gitRunner = previous })

	t.Run("up to date", func(t *testing.T) {
		gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
			switch args[0] {
			case testGitRevParse:
				return "origin/main", nil
			case "rev-list":
				if got := args[len(args)-1]; got != "origin/main..HEAD" {
					t.Fatalf("rev-list range = %q", got)
				}
				return "0", nil
			default:
				return "", fmt.Errorf("unexpected git args: %v", args)
			}
		}
		if !configureBranchHasNoUnpushedCommits(context.Background(), ".") {
			t.Fatal("up-to-date branch reported unsafe")
		}
	})

	t.Run("ahead", func(t *testing.T) {
		gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
			if args[0] == testGitRevParse {
				return "origin/main", nil
			}
			return "2", nil
		}
		if configureBranchHasNoUnpushedCommits(context.Background(), ".") {
			t.Fatal("branch with unpushed commits reported safe")
		}
	})

	t.Run("no upstream", func(t *testing.T) {
		gitRunner = func(context.Context, string, ...string) (string, error) {
			return "", errors.New("no upstream")
		}
		if configureBranchHasNoUnpushedCommits(context.Background(), ".") {
			t.Fatal("branch without upstream reported safe")
		}
	})
}

func TestConfigureRulesBlockDirectPush(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "copilot review is advisory", raw: `[{"type":"copilot_code_review"}]`},
		{name: "required reviews block", raw: `[{"type":"pull_request"}]`, want: true},
		{name: "required checks block", raw: `[{"type":"required_status_checks"}]`, want: true},
		{name: "no rules", raw: `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := configureRulesBlockDirectPush([]byte(tt.raw))
			if err != nil {
				t.Fatalf("configureRulesBlockDirectPush() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("configureRulesBlockDirectPush() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigureSaveOptionsAreDynamic(t *testing.T) {
	unchanged := configureSaveOptions("main", false, false, false, configureSaveCancel)
	if len(unchanged) != 1 || unchanged[0].Value != configureSaveCancel {
		t.Fatalf("unchanged selectable options = %v, want only Cancel", unchanged)
	}
	if got := ansi.Strip(configureSaveDescription("main", false, false, false)); got != "  Save — no changes" {
		t.Fatalf("unchanged disabled save = %q", got)
	}

	push := configureSaveOptions("main", false, true, true, configureSaveDirect)
	got := []string{ansi.Strip(push[0].Key), ansi.Strip(push[1].Key), ansi.Strip(push[2].Key)}
	want := []string{
		"Save — push to main",
		"Save — push to a new branch, review before it lands",
		"Cancel",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("push save options = %v, want %v", got, want)
	}

	local := configureSaveOptions("main", false, false, true, configureSaveLocal)
	got = []string{ansi.Strip(local[0].Key), ansi.Strip(local[1].Key)}
	want = []string{"Save", "Cancel"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local save options = %v, want %v", got, want)
	}

	protected := configureSaveOptions("main", true, true, true, configureSaveNewBranch)
	if len(protected) != 2 {
		t.Fatalf("protected save options = %d, want new branch and cancel", len(protected))
	}
	if got := ansi.Strip(protected[0].Key); got != "Save — push to a new branch, review before it lands" {
		t.Fatalf("selected new-branch option = %q", got)
	}
	description := ansi.Strip(configureSaveDescription("main", true, true, true))
	if !strings.Contains(description, "Save — push to main — protected branch") {
		t.Fatalf("protected destination is not shown as disabled: %q", description)
	}
}

func TestConfigureSaveAndPushUsesForgeRemote(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	previous := gitRunner
	t.Cleanup(func() { gitRunner = previous })
	var pushArgs []string
	gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case "add", gitCmdCommit:
			return "", nil
		case testGitRevParse:
			return testConfigureSHA, nil
		case "push":
			pushArgs = append([]string(nil), args...)
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git args: %v", args)
		}
	}
	generated := []string{".entire/settings.json"}
	if err := configureSaveAndPush(cmd, dir, "acme", "widget", nil, generated, "feature", false, configureSaveDirect, mirrorCloneProviderGitHub, time.Now); err != nil {
		t.Fatalf("configureSaveAndPush() error = %v", err)
	}
	want := []string{"push", "-u", mirrorCloneProviderGitHub, "feature"}
	if !reflect.DeepEqual(pushArgs, want) {
		t.Fatalf("push args = %v, want %v", pushArgs, want)
	}
}

func TestConfigureSaveNewBranchReturnsToOriginalBranch(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	previous := gitRunner
	t.Cleanup(func() { gitRunner = previous })
	var calls [][]string
	gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "show-ref":
			cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 1")
			return "", cmd.Run()
		case "switch", "add", gitCmdCommit, "push":
			return "", nil
		case testGitRevParse:
			return testConfigureSHA, nil
		default:
			return "", fmt.Errorf("unexpected git args: %v", args)
		}
	}
	generated := []string{".entire/settings.json"}
	if err := configureSaveAndPush(cmd, dir, "acme", "widget", nil, generated, "feature", false, configureSaveNewBranch, mirrorCloneProviderGitHub, time.Now); err != nil {
		t.Fatalf("configureSaveAndPush() error = %v", err)
	}
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, []string{"switch", "feature"}) {
		t.Fatalf("last git call = %v, want return to original branch", got)
	}
}

func TestConfigureUseMirrorPreservesOriginUnderAvailableForgeRemote(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	const oldOrigin = "https://github.com/acme/widget.git"
	if _, err := gitRunner(ctx, ".", "remote", "add", defaultMirrorRemote, oldOrigin); err != nil {
		t.Fatalf("add origin: %v", err)
	}
	if _, err := gitRunner(ctx, ".", "remote", "add", defaultMirrorUpstreamRemote, "https://github.com/upstream/widget.git"); err != nil {
		t.Fatalf("add upstream: %v", err)
	}

	var out, errOut bytes.Buffer
	chosen := coreapi.ResolvedPlacement{ClusterHost: testConfigureUSHost}
	forgeRemote, err := configureUseMirror(ctx, &out, &errOut, ".", "acme", "widget", chosen)
	if err != nil {
		t.Fatalf("configureUseMirror() error = %v", err)
	}
	if forgeRemote != mirrorCloneProviderGitHub {
		t.Fatalf("forge remote = %q, want preserved %q", forgeRemote, mirrorCloneProviderGitHub)
	}
	if !strings.Contains(out.String(), "was: "+oldOrigin) {
		t.Fatalf("former origin URL was not reported:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("preservation unexpectedly warned:\n%s", errOut.String())
	}
	githubURL, err := gitremote.GetRemoteURLInDir(ctx, ".", forgeRemote)
	if err != nil {
		t.Fatalf("read preserved forge remote: %v", err)
	}
	if githubURL != oldOrigin {
		t.Fatalf("preserved forge URL = %q, want %q", githubURL, oldOrigin)
	}
}

func TestNewConfigureChangesExcludesWorkAppearingWhileFormWasOpen(t *testing.T) {
	// beforeApply is intentionally captured after the form returns. A file that
	// appeared while the form was open must be treated as pre-existing work, not
	// as onboarding output eligible for automatic commit.
	beforeApply := map[string]string{"notes-from-another-process.md": "??"}
	after := map[string]string{
		"notes-from-another-process.md": "??",
		".entire/settings.json":         "??",
	}
	got := newConfigureChanges(beforeApply, after)
	want := []string{".entire/settings.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new changes = %v, want %v", got, want)
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
