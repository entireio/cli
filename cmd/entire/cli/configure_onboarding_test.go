package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	testConfigureUSHost = "us.entire.io"
	testGitRevParse     = "rev-parse"
	testConfigureSHA    = "abc123"
	testGitLSRemote     = "ls-remote"
	testGitShowRef      = "show-ref"
	testGitPush         = "push"
	testGitAdd          = "add"
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

func TestConfigureAgentOptionsUsePlainLabels(t *testing.T) {
	selected := map[types.AgentName]struct{}{agent.AgentNameClaudeCode: {}}
	options := configureAgentOptions([]huh.Option[string]{
		huh.NewOption("Claude Code", string(agent.AgentNameClaudeCode)),
		huh.NewOption("Codex", string(agent.AgentNameCodex)),
	}, selected)
	got := []string{ansi.Strip(options[0].Key), ansi.Strip(options[1].Key)}
	if want := []string{"Claude Code", "Codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent labels = %v, want plain labels %v", got, want)
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

func TestConfigureSaveNoChangesStateIsAlwaysVisible(t *testing.T) {
	choice := configureSaveLocal
	field := uiform.NewActionSelect(
		"Save configuration",
		configureSaveDescription("main", false, false, false),
		configureSaveOptions("main", false, false, false, choice),
		&choice,
	)
	form := uiform.New(huh.NewGroup(field))
	form.Init()
	got := ansi.Strip(form.View())
	if !strings.Contains(got, "Save — no changes") {
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

	const headSHA = "0123456789abcdef"
	gitWithRemoteHead := func(localSHA string) func(context.Context, string, ...string) (string, error) {
		return func(_ context.Context, _ string, args ...string) (string, error) {
			switch args[0] {
			case "config":
				if strings.HasSuffix(args[len(args)-1], ".remote") {
					return mirrorCloneProviderGitHub, nil
				}
				return "refs/heads/main", nil
			case testGitLSRemote:
				return headSHA + "\trefs/heads/main", nil
			case testGitRevParse:
				return localSHA, nil
			default:
				return "", fmt.Errorf("unexpected git args: %v", args)
			}
		}
	}

	t.Run("matches live remote despite stale tracking ref", func(t *testing.T) {
		gitRunner = gitWithRemoteHead(headSHA)
		if !configureBranchHasNoUnpushedCommits(context.Background(), ".", "main") {
			t.Fatal("branch matching the live forge head reported unsafe")
		}
	})

	t.Run("ahead of live remote", func(t *testing.T) {
		gitRunner = gitWithRemoteHead("fedcba9876543210")
		if configureBranchHasNoUnpushedCommits(context.Background(), ".", "main") {
			t.Fatal("branch with unpushed commits reported safe")
		}
	})

	t.Run("no upstream", func(t *testing.T) {
		gitRunner = func(context.Context, string, ...string) (string, error) {
			return "", errors.New("no upstream")
		}
		if configureBranchHasNoUnpushedCommits(context.Background(), ".", "main") {
			t.Fatal("branch without upstream reported safe")
		}
	})
}

func TestConfigureNonInteractiveAgentSelectionFallsBackToDefault(t *testing.T) {
	defaultAgent := agent.Default()
	if defaultAgent == nil {
		t.Fatal("test requires the default agent to be registered")
	}
	defaultName := string(defaultAgent.Name())
	options := []huh.Option[string]{huh.NewOption("Default", defaultName)}
	got, err := configureNonInteractiveAgentSelection(nil, options)
	if err != nil {
		t.Fatalf("configureNonInteractiveAgentSelection() error = %v", err)
	}
	if !reflect.DeepEqual(got, []string{defaultName}) {
		t.Fatalf("selection = %v, want default agent %q", got, defaultName)
	}
}

func TestConfigureNonInteractiveAgentSelectionPreservesDetectedAgents(t *testing.T) {
	selected := []string{"codex"}
	got, err := configureNonInteractiveAgentSelection(selected, nil)
	if err != nil {
		t.Fatalf("configureNonInteractiveAgentSelection() error = %v", err)
	}
	if !reflect.DeepEqual(got, selected) {
		t.Fatalf("selection = %v, want existing selection %v", got, selected)
	}
}

func TestConfigureNoChangeSaveChoices(t *testing.T) {
	if got := defaultConfigureSaveChoice(false, false, false); got != configureSaveCancel {
		t.Fatalf("default no-change form choice = %q, want Cancel", got)
	}
	if got := configureNonInteractiveSaveChoice(false, false, false); got != "" {
		t.Fatalf("non-interactive no-change action = %q, want successful no-op", got)
	}
}

func TestConfigureEffectiveProtectionFailsClosedNonInteractively(t *testing.T) {
	tests := []struct {
		name                             string
		protected, known, nonInteractive bool
		want                             bool
	}{
		{name: "known unprotected", known: true, nonInteractive: true},
		{name: "known protected", protected: true, known: true, nonInteractive: true, want: true},
		{name: "unknown interactive remains advisory"},
		{name: "unknown non-interactive fails closed", nonInteractive: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configureEffectiveProtection(tt.protected, tt.known, tt.nonInteractive); got != tt.want {
				t.Fatalf("configureEffectiveProtection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigureBranchProtectedChecksRulesetsAndClassicProtection(t *testing.T) {
	previous := configureGitHubAPI
	t.Cleanup(func() { configureGitHubAPI = previous })

	tests := []struct {
		name        string
		rules       string
		classicErr  error
		want        bool
		wantClassic bool
	}{
		{name: "classic protection blocks", rules: `[]`, want: true, wantClassic: true},
		{name: "no classic or ruleset protection", rules: `[{"type":"copilot_code_review"}]`, classicErr: errors.New("HTTP 404: branch not protected"), wantClassic: true},
		{name: "blocking ruleset short circuits", rules: `[{"type":"pull_request"}]`, classicErr: errors.New("classic endpoint should not be called"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classicCalled := false
			configureGitHubAPI = func(_ context.Context, endpoint string) ([]byte, error) {
				if strings.Contains(endpoint, "/rules/branches/") {
					return []byte(tt.rules), nil
				}
				classicCalled = true
				if tt.classicErr != nil {
					return nil, tt.classicErr
				}
				return []byte(`{"required_pull_request_reviews":{}}`), nil
			}
			got, err := configureBranchProtected(context.Background(), "acme", "widget", "main")
			if err != nil {
				t.Fatalf("configureBranchProtected() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("configureBranchProtected() = %v, want %v", got, tt.want)
			}
			if classicCalled != tt.wantClassic {
				t.Fatalf("classic endpoint called = %v, want %v", classicCalled, tt.wantClassic)
			}
		})
	}
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
		{name: "required signatures block", raw: `[{"type":"required_signatures"}]`, want: true},
		{name: "merge queue blocks", raw: `[{"type":"merge_queue"}]`, want: true},
		{name: "locked branch blocks", raw: `[{"type":"lock_branch"}]`, want: true},
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
		case testGitAdd, gitCmdCommit:
			return "", nil
		case testGitRevParse:
			return testConfigureSHA, nil
		case testGitPush:
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
	want := []string{testGitPush, "-u", mirrorCloneProviderGitHub, "feature"}
	if !reflect.DeepEqual(pushArgs, want) {
		t.Fatalf("push args = %v, want %v", pushArgs, want)
	}
}

func TestAvailableConfigureBranchSkipsRemoteCollision(t *testing.T) {
	previous := gitRunner
	t.Cleanup(func() { gitRunner = previous })
	var remoteChecks []string
	gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case testGitShowRef:
			cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 1")
			return "", cmd.Run()
		case testGitLSRemote:
			branch := strings.TrimPrefix(args[len(args)-1], "refs/heads/")
			remoteChecks = append(remoteChecks, branch)
			if branch == configureBranchBase {
				return "deadbeef\trefs/heads/" + branch, nil
			}
			cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 2")
			return "", cmd.Run()
		default:
			return "", fmt.Errorf("unexpected git args: %v", args)
		}
	}
	now := func() time.Time { return time.Date(2026, 8, 12, 14, 30, 45, 0, time.UTC) }
	got, err := availableConfigureBranch(context.Background(), ".", mirrorCloneProviderGitHub, now)
	if err != nil {
		t.Fatalf("availableConfigureBranch() error = %v", err)
	}
	want := configureBranchBase + "-20260812-143045"
	if got != want {
		t.Fatalf("branch = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(remoteChecks, []string{configureBranchBase, want}) {
		t.Fatalf("remote checks = %v", remoteChecks)
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
		case testGitShowRef:
			cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 1")
			return "", cmd.Run()
		case testGitLSRemote:
			cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 2")
			return "", cmd.Run()
		case "switch", testGitAdd, gitCmdCommit, testGitPush, "restore":
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
	wantRestore := []string{"restore", "--source", configureBranchBase, "--worktree", "--", ".entire/settings.json"}
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, wantRestore) {
		t.Fatalf("last git call = %v, want configuration restored on original branch as %v", got, wantRestore)
	}
}

func TestConfigureSaveNewBranchPushFailureRestoresConfiguration(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	previous := gitRunner
	t.Cleanup(func() { gitRunner = previous })
	var calls [][]string
	gitRunner = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case testGitShowRef:
			exit := exec.CommandContext(context.Background(), "sh", "-c", "exit 1")
			return "", exit.Run()
		case testGitLSRemote:
			exit := exec.CommandContext(context.Background(), "sh", "-c", "exit 2")
			return "", exit.Run()
		case "switch", testGitAdd, gitCmdCommit, "restore":
			return "", nil
		case testGitRevParse:
			return testConfigureSHA, nil
		case testGitPush:
			return "", errors.New("push rejected")
		default:
			return "", fmt.Errorf("unexpected git args: %v", args)
		}
	}
	generated := []string{".entire/settings.json"}
	err := configureSaveAndPush(cmd, t.TempDir(), "acme", "widget", nil, generated, "feature", false, configureSaveNewBranch, mirrorCloneProviderGitHub, time.Now)
	if err == nil || !strings.Contains(err.Error(), "push rejected") {
		t.Fatalf("configureSaveAndPush() error = %v, want push failure", err)
	}
	wantRestore := []string{"restore", "--source", configureBranchBase, "--worktree", "--", generated[0]}
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, wantRestore) {
		t.Fatalf("last git call after push failure = %v, want %v", got, wantRestore)
	}
}

func TestConfigureSaveNewBranchKeepsConfigurationOnOriginalBranch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	remoteDir := filepath.Join(root, "forge.git")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(dir string, args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repoDir, "init", "--quiet")
	runGit(repoDir, "config", "user.name", "Configure Test")
	runGit(repoDir, "config", "user.email", "configure@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	runGit(repoDir, testGitAdd, "README.md")
	runGit(repoDir, "commit", "--quiet", "-m", "base")
	originalBranch := runGit(repoDir, "branch", "--show-current")
	runGit(root, "init", "--bare", "--quiet", remoteDir)
	runGit(repoDir, "remote", testGitAdd, mirrorCloneProviderGitHub, remoteDir)

	generatedPath := filepath.Join(repoDir, ".entire", "settings.json")
	if err := os.MkdirAll(filepath.Dir(generatedPath), 0o755); err != nil {
		t.Fatalf("mkdir generated config: %v", err)
	}
	if err := os.WriteFile(generatedPath, []byte("{\"enabled\":true}\n"), 0o644); err != nil {
		t.Fatalf("write generated config: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	generated := []string{".entire/settings.json"}
	if err := configureSaveAndPush(cmd, repoDir, "acme", "widget", nil, generated, originalBranch, false, configureSaveNewBranch, mirrorCloneProviderGitHub, time.Now); err != nil {
		t.Fatalf("configureSaveAndPush() error = %v", err)
	}
	if got := runGit(repoDir, "branch", "--show-current"); got != originalBranch {
		t.Fatalf("current branch = %q, want %q", got, originalBranch)
	}
	contents, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("configuration disappeared after returning to original branch: %v", err)
	}
	if got, want := string(contents), "{\"enabled\":true}\n"; got != want {
		t.Fatalf("configuration contents = %q, want %q", got, want)
	}
	if got := runGit(repoDir, "status", "--porcelain", "--", generated[0]); got != "?? .entire/settings.json" {
		t.Fatalf("configuration status on original branch = %q, want uncommitted generated file", got)
	}
}

func TestConfigureUseMirrorPreservesOriginUnderAvailableForgeRemote(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	const oldOrigin = "https://github.com/acme/widget.git"
	if _, err := gitRunner(ctx, ".", "remote", testGitAdd, defaultMirrorRemote, oldOrigin); err != nil {
		t.Fatalf("add origin: %v", err)
	}
	if _, err := gitRunner(ctx, ".", "remote", testGitAdd, defaultMirrorUpstreamRemote, "https://github.com/upstream/widget.git"); err != nil {
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

func TestConfigureUseMirrorRetargetsCurrentBranchFromMirrorToForge(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	branch, err := gitRunner(ctx, ".", "branch", "--show-current")
	if err != nil || branch == "" {
		t.Fatalf("current branch = %q, error = %v", branch, err)
	}
	mirrorURL := mirrorCloneURL(testConfigureUSHost, "acme", "widget")
	if _, err := gitRunner(ctx, ".", "remote", testGitAdd, defaultMirrorRemote, mirrorURL); err != nil {
		t.Fatalf("add mirror origin: %v", err)
	}
	if _, err := gitRunner(ctx, ".", "remote", testGitAdd, mirrorCloneProviderGitHub, "https://github.com/acme/widget.git"); err != nil {
		t.Fatalf("add forge remote: %v", err)
	}
	if _, err := gitRunner(ctx, ".", "config", "--local", "branch."+branch+".remote", defaultMirrorRemote); err != nil {
		t.Fatalf("set tracking remote: %v", err)
	}
	if _, err := gitRunner(ctx, ".", "config", "--local", "branch."+branch+".merge", "refs/heads/"+branch); err != nil {
		t.Fatalf("set tracking merge ref: %v", err)
	}

	chosen := coreapi.ResolvedPlacement{ClusterHost: testConfigureUSHost}
	if _, err := configureUseMirror(ctx, io.Discard, io.Discard, ".", "acme", "widget", chosen); err != nil {
		t.Fatalf("configureUseMirror() error = %v", err)
	}
	trackingRemote, err := gitRunner(ctx, ".", "config", "--get", "branch."+branch+".remote")
	if err != nil {
		t.Fatalf("read tracking remote: %v", err)
	}
	if trackingRemote != mirrorCloneProviderGitHub {
		t.Fatalf("tracking remote = %q, want forge remote %q", trackingRemote, mirrorCloneProviderGitHub)
	}
}

func TestConfigureUseMirrorCreatesForgeRemoteWithGHProtocolPreference(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	mirrorURL := mirrorCloneURL(testConfigureUSHost, "acme", "widget")
	if _, err := gitRunner(ctx, ".", "remote", testGitAdd, defaultMirrorRemote, mirrorURL); err != nil {
		t.Fatalf("add mirror origin: %v", err)
	}

	previousProtocol := configureGitHubProtocol
	t.Cleanup(func() { configureGitHubProtocol = previousProtocol })
	configureGitHubProtocol = func(context.Context) string { return configureGitProtocolSSH }

	var out, errOut bytes.Buffer
	chosen := coreapi.ResolvedPlacement{ClusterHost: testConfigureUSHost}
	forgeRemote, err := configureUseMirror(ctx, &out, &errOut, ".", "acme", "widget", chosen)
	if err != nil {
		t.Fatalf("configureUseMirror() error = %v", err)
	}
	forgeURL, err := gitremote.GetRemoteURLInDir(ctx, ".", forgeRemote)
	if err != nil {
		t.Fatalf("read forge remote: %v", err)
	}
	if want := "git@github.com:acme/widget.git"; forgeURL != want {
		t.Fatalf("forge URL = %q, want SSH preference %q", forgeURL, want)
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
