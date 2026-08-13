package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	configureAccessPollInterval = 2 * time.Second
	configureAccessWaitTimeout  = 5 * time.Minute
	configureCommitMessage      = "chore: configure entire"
	configureBranchBase         = "configure-entire"
	configureSaveDirect         = "direct"
	configureSaveNewBranch      = "new-branch"
)

// configureAccessReporter is the small part of the authenticated API used by
// onboarding. Keeping it narrow makes the state machine independently testable.
type configureAccessReporter interface {
	ReportEnable(ctx context.Context, remoteURL string) (*api.EnableRepoResponse, error)
}

// configureFlowDeps contains side effects used by the interactive configure
// flow. Tests can replace individual operations without a keyring, browser,
// GitHub CLI, or live control plane.
type configureFlowDeps struct {
	resolveAuth  func(context.Context) (statusTarget, error)
	fetchProfile profileFetcher
	runLogin     func(context.Context, io.Writer, io.Writer) error
	accessClient func(context.Context) (configureAccessReporter, error)
	coreClient   func() (*coreapi.Client, error)
	openURL      browserOpenFunc
	githubAdmin  func(context.Context, string, string) (bool, error)
	now          func() time.Time
}

func newConfigureFlowDeps(insecure bool) configureFlowDeps {
	return configureFlowDeps{
		resolveAuth: func(ctx context.Context) (statusTarget, error) {
			return resolveAuthStatusTarget(ctx, auth.Contexts, auth.RefreshedLoginToken)
		},
		fetchProfile: defaultFetchProfile,
		runLogin: func(ctx context.Context, outW, errW io.Writer) error {
			cmd := newLoginCmd()
			cmd.SetContext(ctx)
			cmd.SetOut(outW)
			cmd.SetErr(errW)
			return cmd.RunE(cmd, nil)
		},
		accessClient: func(ctx context.Context) (configureAccessReporter, error) {
			return NewAuthenticatedAPIClient(ctx, insecure)
		},
		coreClient:  coreapi.New,
		openURL:     openBrowser,
		githubAdmin: configureGitHubAdmin,
		now:         time.Now,
	}
}

// runConfigureOnboarding is a seam for command tests. Bare interactive
// `entire configure` is the new end-to-end entry point; flag-based configure
// remains the non-interactive settings editor.
var runConfigureOnboarding = func(cmd *cobra.Command, opts EnableOptions) error {
	return runConfigureOnboardingFlow(cmd, opts, newConfigureFlowDeps(false))
}

func runConfigureOnboardingFlow(cmd *cobra.Command, opts EnableOptions, deps configureFlowDeps) error {
	cmd.SilenceUsage = true
	ctx := cmd.Context()
	outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		fmt.Fprintln(errW, "Not a git repository. Please run 'entire configure' from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}
	owner, repo, cleanRemote, err := configureRepoIdentity(ctx)
	if err != nil {
		return err
	}

	profile, err := ensureConfigureLogin(ctx, outW, errW, deps)
	if err != nil {
		return err
	}

	reporter, err := deps.accessClient(ctx)
	if err != nil {
		return fmt.Errorf("connect to Entire: %w", err)
	}
	access, err := reporter.ReportEnable(ctx, cleanRemote)
	if err != nil {
		return fmt.Errorf("check repository access: %w", err)
	}
	if !access.Connected {
		if err := ensureConfigureRepoAccess(ctx, outW, errW, reporter, access, cleanRemote, owner, repo, deps); err != nil {
			return err
		}
	}

	client, err := deps.coreClient()
	if err != nil {
		return fmt.Errorf("connect to Entire control plane: %w", err)
	}
	placements, err := resolvePullablePlacements(ctx, client, owner, repo)
	if err != nil {
		return renderCoreError(err)
	}
	if len(placements) == 0 {
		placements, err = configureCreateMirrors(ctx, outW, errW, client, owner, repo, profile.Jurisdiction)
		if err != nil {
			return err
		}
	}
	if len(placements) == 0 {
		return errors.New("no usable mirror was selected")
	}

	manageRegionsHint := ""
	if admin, adminErr := deps.githubAdmin(ctx, owner, repo); adminErr == nil && admin {
		manageRegionsHint = fmt.Sprintf("Manage regions: entire repo mirror create · or %s/gh/%s/%s/settings",
			configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
	}
	chosen, selectedAgents, err := promptConfigureUpstreamAndAgents(
		ctx, errW, repoRoot, placements, profile.Jurisdiction, manageRegionsHint,
	)
	if err != nil {
		return err
	}
	if err := configureUseMirror(ctx, repoRoot, owner, repo, chosen); err != nil {
		return err
	}

	before, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}

	// The combined form above owns both independent choices. Keeping them in a
	// single group leaves the upstream visible while agents are focused and lets
	// the user move back to it before submitting. runEnableInteractive applies
	// the complete agent selection, writes settings, and installs hooks.

	opts.Yes = true
	opts.UseProjectSettings = true
	opts.UseLocalSettings = false
	opts.SuppressDoneMessage = true
	opts.SuppressAdditionalSetup = true
	if err := runEnableInteractive(ctx, outW, selectedAgents, opts); err != nil {
		return err
	}
	// A local enabled:false override must not win over the newly written project
	// configuration. setEnabledFlag updates the project and synchronizes an
	// existing local override without replacing its other local-only fields.
	if err := setEnabledFlag(ctx, true, true); err != nil {
		return err
	}

	after, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	generated := newConfigureChanges(before, after)
	fmt.Fprintf(outW, "✓ Wrote %s\n", configDisplayProject)

	if err := configureSaveAndPush(cmd, repoRoot, owner, repo, before, generated, deps.now); err != nil {
		return err
	}
	fmt.Fprintln(outW, "✓ Configuration complete")
	fmt.Fprintln(outW)
	fmt.Fprintln(outW, "  You're set. Start a session with any selected agent —")
	fmt.Fprintf(outW, "  checkpoints will show up on %s/gh/%s/%s\n", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
	return nil
}

func configureWebBaseURL() string {
	return strings.TrimRight(api.BaseURL(), "/")
}

func configureRepoIdentity(ctx context.Context) (owner, repo, cleanRemote string, err error) {
	raw, err := gitremote.GetRemoteURL(ctx, defaultMirrorRemote)
	if err != nil {
		return "", "", "", fmt.Errorf("read origin remote: %w", err)
	}
	info, err := gitremote.ParseURL(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("parse origin remote: %w", err)
	}
	if info.Forge != mirrorUseForge {
		return "", "", "", errors.New("interactive configure currently requires a GitHub origin remote")
	}
	clean, err := cleanRemoteURLForReport(raw)
	if err != nil {
		return "", "", "", err
	}
	return strings.ToLower(info.Owner), strings.ToLower(info.Repo), clean, nil
}

func ensureConfigureLogin(ctx context.Context, outW, errW io.Writer, deps configureFlowDeps) (*authProfile, error) {
	target, err := deps.resolveAuth(ctx)
	if err != nil {
		return nil, err
	}
	if target.token != "" {
		if profile, profileErr := deps.fetchProfile(ctx, target.coreURL, target.token); profileErr == nil {
			return profile, nil
		} else if !isKeychainTokenRejected(profileErr) {
			return nil, fmt.Errorf("validate login: %w", profileErr)
		}
	}

	if err := deps.runLogin(ctx, outW, errW); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	target, err = deps.resolveAuth(ctx)
	if err != nil {
		return nil, err
	}
	if target.token == "" {
		return nil, errors.New("login completed without an active session")
	}
	profile, err := deps.fetchProfile(ctx, target.coreURL, target.token)
	if err != nil {
		return nil, fmt.Errorf("validate login: %w", err)
	}
	fmt.Fprintf(outW, "✓ Logged in as %s\n", profile.Handle)
	return profile, nil
}

func ensureConfigureRepoAccess(ctx context.Context, outW, errW io.Writer, reporter configureAccessReporter, initial *api.EnableRepoResponse, cleanRemote, owner, repo string, deps configureFlowDeps) error {
	fmt.Fprintf(errW, "✗ Entire has no access to %s/%s\n\n", owner, repo)
	installURL := strings.TrimSpace(initial.InstallURL)
	if installURL == "" {
		installURL = fmt.Sprintf("%s/install?repo=%s%%2F%s", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
	}

	admin, adminErr := deps.githubAdmin(ctx, owner, repo)
	if adminErr != nil || !admin {
		fmt.Fprintln(errW, "  An admin needs to install the GitHub app first.")
		fmt.Fprintln(errW, "  Send them this link to continue:")
		fmt.Fprintf(errW, "\n  %s\n", installURL)
		return NewSilentError(errors.New("GitHub app installation required"))
	}

	fmt.Fprintln(outW, "  Install the GitHub app to grant access:")
	fmt.Fprintf(outW, "  %s\n\n", installURL)
	if err := deps.openURL(ctx, installURL); err != nil {
		fmt.Fprintln(outW, "  Open the link above in your browser.")
	}
	stop := startSpinner(errW, "Waiting for installation")
	stopped := false
	defer func() {
		if !stopped {
			stop(false)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, configureAccessWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(configureAccessPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for GitHub app installation: %w", waitCtx.Err())
		case <-ticker.C:
			access, err := reporter.ReportEnable(waitCtx, cleanRemote)
			if err == nil && access.Connected {
				stop(true)
				stopped = true
				fmt.Fprintln(outW, "✓ Access granted")
				return nil
			}
		}
	}
}

func configureGitHubAdmin(ctx context.Context, owner, repo string) (bool, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", "repos/"+owner+"/"+repo, "--jq", ".permissions.admin").Output()
	if err != nil {
		return false, fmt.Errorf("check GitHub admin permission: %w", err)
	}
	admin, err := strconv.ParseBool(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("parse GitHub admin permission: %w", err)
	}
	return admin, nil
}

func configureCreateMirrors(ctx context.Context, outW, errW io.Writer, client *coreapi.Client, owner, repo, jurisdiction string) ([]coreapi.ResolvedPlacement, error) {
	regions, err := availableRegions(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	if len(regions) == 0 {
		return nil, errors.New("no regions available to mirror into")
	}
	selected, err := pickConfigureRegions(ctx, outW, regions, jurisdiction)
	if err != nil {
		return nil, err
	}
	results := createMirrors(ctx, errW, mirrorTargets([]coreapi.AvailableMirror{{Owner: owner, Repo: repo}}, selected), false, 30*time.Minute)
	if err := reportMirrorResults(outW, errW, results); err != nil {
		return nil, err
	}
	placements := make([]coreapi.ResolvedPlacement, 0, len(results))
	for i, result := range results {
		if result.err != nil || result.cloneURL == "" {
			continue
		}
		region := selected[i]
		placements = append(placements, coreapi.ResolvedPlacement{
			ClusterHost:  region.host,
			Cell:         coreapi.NewOptString(region.slug),
			Jurisdiction: coreapi.NewOptString(region.jurisdiction),
		})
	}
	return placements, nil
}

func pickConfigureRegions(ctx context.Context, outW io.Writer, regions []regionChoice, jurisdiction string) ([]regionChoice, error) {
	opts, defaults := clusterChoices(regions, jurisdiction)
	byHost := make(map[string]regionChoice, len(regions))
	for _, region := range regions {
		byHost[region.host] = region
	}
	selected := append([]string(nil), defaults...)
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Which regions should this repository live in?").
			Description("Space to select, enter to confirm — your data stays in the selected regions.").
			Options(opts...).
			Height(multiSelectHeight(len(opts))).
			Validate(func(values []string) error {
				if len(values) == 0 {
					return errors.New("select at least one region")
				}
				return nil
			}).
			Value(&selected),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(outW, "Configure", err); cancelErr != nil {
			return nil, cancelErr
		}
		return nil, NewSilentError(errors.New("configure cancelled"))
	}
	chosen := make([]regionChoice, 0, len(selected))
	for _, host := range selected {
		if region, ok := byHost[host]; ok {
			chosen = append(chosen, region)
		}
	}
	return chosen, nil
}

// configureFocusChangedMsg forces huh's dynamic titles to re-evaluate after a
// field transition. huh evaluates TitleFunc before it calls Blur/Focus while
// handling NextField, so without this follow-up frame the help bar changes but
// the old section keeps the active-colored question mark until another keypress.
type configureFocusChangedMsg struct{}

func configureRefreshFocus() tea.Cmd {
	return func() tea.Msg { return configureFocusChangedMsg{} }
}

type configureUpstreamField struct {
	*huh.Select[string]

	value       *string
	committed   string
	highlighted string
	refresh     func()
	focused     bool
}

func (field *configureUpstreamField) Focus() tea.Cmd {
	field.focused = true
	return tea.Batch(field.Select.Focus(), configureRefreshFocus())
}

func (field *configureUpstreamField) Blur() tea.Cmd {
	field.focused = false
	return field.Select.Blur()
}

func (field *configureUpstreamField) KeyBinds() []key.Binding {
	return configureRadioKeyBinds(field.Select)
}

func (field *configureUpstreamField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case configureRadioSelectKey(keyMsg.String()):
			field.commitHighlighted()
			return field, nil
		case configureNextKey(keyMsg.String()):
			// Enter continues with the intentionally selected value. Merely moving
			// the cursor does not change the radio selection.
			return field, huh.NextField
		}
	}
	model, cmd := field.Select.Update(msg)
	if updated, ok := model.(*huh.Select[string]); ok {
		field.Select = updated
	}
	_, movedCursor := msg.(tea.KeyPressMsg)
	field.preserveCommittedSelection(movedCursor)
	return field, cmd
}

func (field *configureUpstreamField) preserveCommittedSelection(movedCursor bool) {
	if field.value == nil {
		return
	}
	if movedCursor {
		field.highlighted = *field.value
	}
	*field.value = field.committed
}

func (field *configureUpstreamField) commitHighlighted() {
	if field.value == nil {
		return
	}
	field.committed = field.highlighted
	*field.value = field.committed
	if field.refresh != nil {
		field.refresh()
	}
}

type configureAgentField struct {
	*huh.MultiSelect[string]

	focused bool
}

func (field *configureAgentField) Focus() tea.Cmd {
	field.focused = true
	return tea.Batch(field.MultiSelect.Focus(), configureRefreshFocus())
}

func (field *configureAgentField) Blur() tea.Cmd {
	field.focused = false
	return field.MultiSelect.Blur()
}

func (field *configureAgentField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	model, cmd := field.MultiSelect.Update(msg)
	if updated, ok := model.(*huh.MultiSelect[string]); ok {
		field.MultiSelect = updated
	}
	return field, cmd
}

type configureSaveField struct {
	*huh.Select[string]

	value       *string
	committed   string
	highlighted string
	refresh     func()
	focused     bool
}

func (field *configureSaveField) Focus() tea.Cmd {
	field.focused = true
	return tea.Batch(field.Select.Focus(), configureRefreshFocus())
}

func (field *configureSaveField) Blur() tea.Cmd {
	field.focused = false
	return field.Select.Blur()
}

func (field *configureSaveField) KeyBinds() []key.Binding {
	return configureRadioKeyBinds(field.Select)
}

func (field *configureSaveField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case configureRadioSelectKey(keyMsg.String()):
			field.commitHighlighted()
			return field, nil
		case configureNextKey(keyMsg.String()):
			return field, huh.NextField
		}
	}
	model, cmd := field.Select.Update(msg)
	if updated, ok := model.(*huh.Select[string]); ok {
		field.Select = updated
	}
	_, movedCursor := msg.(tea.KeyPressMsg)
	field.preserveCommittedSelection(movedCursor)
	return field, cmd
}

func (field *configureSaveField) preserveCommittedSelection(movedCursor bool) {
	if field.value == nil {
		return
	}
	if movedCursor {
		field.highlighted = *field.value
	}
	*field.value = field.committed
}

func (field *configureSaveField) commitHighlighted() {
	if field.value == nil {
		return
	}
	field.committed = field.highlighted
	*field.value = field.committed
	if field.refresh != nil {
		field.refresh()
	}
}

func configureRadioKeyBinds(selectField *huh.Select[string]) []key.Binding {
	bindings := selectField.KeyBinds()
	for i := range bindings {
		help := bindings[i].Help()
		if help.Key == "enter" && help.Desc == "select" {
			bindings[i].SetHelp("enter", "continue")
		}
	}
	return append(bindings, key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "select")))
}

func configureRadioSelectKey(key string) bool {
	return key == "space" || key == " "
}

func configureNextKey(key string) bool {
	return key == "enter" || key == "tab"
}

// promptConfigureUpstreamAndAgents presents upstream and agents as one
// persistent form. Arrow keys stay within the active section; Enter confirms a
// section and advances, while shift+tab revisits the previous section.
func promptConfigureUpstreamAndAgents(ctx context.Context, errW io.Writer, repoRoot string, placements []coreapi.ResolvedPlacement, jurisdiction, manageRegionsHint string) (coreapi.ResolvedPlacement, []agent.Agent, error) {
	currentHost := configureCurrentUpstream(ctx, repoRoot)
	placements = configurePlacementOrder(placements, currentHost, jurisdiction)
	if len(placements) == 0 {
		return coreapi.ResolvedPlacement{}, nil, errors.New("no upstreams available")
	}

	// configurePlacementOrder puts the configured upstream first, so it is also
	// the initial selection. If this repo has no Entire upstream yet, it falls
	// back to the jurisdiction-preferred placement.
	selectedHost := placements[0].ClusterHost
	placementByHost := make(map[string]coreapi.ResolvedPlacement, len(placements))
	for _, placement := range placements {
		placementByHost[placement.ClusterHost] = placement
	}
	upstreamOptions := func() []huh.Option[string] {
		return configureUpstreamOptions(placements, selectedHost, currentHost)
	}

	external.DiscoverAndRegisterAlways(ctx)
	preselected := configureAgentPreselection(ctx)
	agentOptions := configureAgentOptions(hookAgentOptions(preselected), preselected)
	if len(agentOptions) == 0 {
		return coreapi.ResolvedPlacement{}, nil, errors.New("no agents with hook support available")
	}
	selectedAgentNames := make([]string, 0, len(preselected))
	for _, option := range agentOptions {
		if _, ok := preselected[types.AgentName(option.Value)]; ok {
			selectedAgentNames = append(selectedAgentNames, option.Value)
		}
	}

	// huh's footer already documents navigation. Keep field descriptions for
	// contextual information only, rather than repeating keyboard controls.
	upstreamDescription := manageRegionsHint
	upstreamControl := &configureUpstreamField{
		value:       &selectedHost,
		committed:   selectedHost,
		highlighted: selectedHost,
	}
	upstreamControl.Select = huh.NewSelect[string]().
		TitleFunc(func() string {
			return configureQuestionTitle("Which upstream do you want to use?", upstreamControl.focused)
		}, &upstreamControl.focused).
		Description(upstreamDescription).
		Options(upstreamOptions()...).
		Height(configureFieldHeight(len(placements), upstreamDescription)).
		Value(&selectedHost)
	upstreamControl.refresh = func() {
		upstreamControl.Select.Options(upstreamOptions()...)
	}

	agentControl := &configureAgentField{}
	agentControl.MultiSelect = huh.NewMultiSelect[string]().
		TitleFunc(func() string {
			return configureQuestionTitle("Select the agents you want to use", agentControl.focused)
		}, &agentControl.focused).
		Options(agentOptions...).
		// MultiSelect subtracts its title from an implicit viewport height, so
		// include exactly one title row to show every option without padding.
		Height(len(agentOptions) + 1).
		Validate(func(values []string) error {
			if len(values) == 0 {
				return errors.New("select at least one agent")
			}
			return nil
		}).
		Value(&selectedAgentNames)

	group := huh.NewGroup(upstreamControl, agentControl)
	form := newConfigureForm(group)
	// Separate the form from the shell prompt or preceding onboarding status.
	fmt.Fprintln(errW)
	if err := form.RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Configure", err); cancelErr != nil {
			return coreapi.ResolvedPlacement{}, nil, cancelErr
		}
		return coreapi.ResolvedPlacement{}, nil, NewSilentError(errors.New("configure cancelled"))
	}

	chosen, ok := placementByHost[selectedHost]
	if !ok {
		return coreapi.ResolvedPlacement{}, nil, errors.New("selected upstream is no longer available")
	}
	selectedAgents := make([]agent.Agent, 0, len(selectedAgentNames))
	for _, name := range selectedAgentNames {
		ag, err := agent.Get(types.AgentName(name))
		if err != nil {
			return coreapi.ResolvedPlacement{}, nil, fmt.Errorf("load selected agent %q: %w", name, err)
		}
		selectedAgents = append(selectedAgents, ag)
	}
	return chosen, selectedAgents, nil
}

func configureUpstreamOptions(placements []coreapi.ResolvedPlacement, selectedHost, currentHost string) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(placements))
	for _, placement := range placements {
		selected := placement.ClusterHost == selectedHost
		label := configureRadioLabel(configurePlacementLabel(placement), selected)
		if strings.EqualFold(placement.ClusterHost, currentHost) {
			tag := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("— current")
			label += " " + tag
		}
		options = append(options, huh.NewOption(label, placement.ClusterHost))
	}
	return options
}

func configureFieldHeight(optionCount int, description string) int {
	// One title row, the explicit description rows, and exactly one row per
	// option. huh otherwise gives dynamic Select fields spare viewport rows,
	// which accumulate as increasingly large gaps between form sections.
	height := optionCount + 1
	if description != "" {
		height += lipgloss.Height(description)
	}
	return height
}

func promptConfigureSave(ctx context.Context, errW io.Writer, branch string, protected bool) (string, error) {
	choice := configureSaveDirect
	if protected {
		choice = configureSaveNewBranch
	}
	options := func() []huh.Option[string] {
		return configureSaveOptions(branch, protected, choice)
	}
	control := &configureSaveField{
		value:       &choice,
		committed:   choice,
		highlighted: choice,
	}
	control.Select = huh.NewSelect[string]().
		TitleFunc(func() string {
			return configureQuestionTitle("Save configuration", control.focused)
		}, &control.focused).
		Description(configureSaveDescription(branch, protected)).
		Options(options()...).
		Height(configureFieldHeight(len(options()), configureSaveDescription(branch, protected))).
		Value(&choice)
	control.refresh = func() {
		control.Select.Options(options()...)
	}

	fmt.Fprintln(errW)
	if err := newConfigureForm(huh.NewGroup(control)).RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Configure", err); cancelErr != nil {
			return "", cancelErr
		}
		return "", NewSilentError(errors.New("configure cancelled"))
	}
	return choice, nil
}

func configureSaveOptions(branch string, protected bool, selected string) []huh.Option[string] {
	newBranch := huh.NewOption(
		configureRadioLabel("Push to a new branch — review before it lands", selected == configureSaveNewBranch),
		configureSaveNewBranch,
	)
	if protected {
		// huh has no disabled-option primitive. Keep the protected destination
		// visible in the description, but omit it from the selectable options so
		// keyboard navigation can never focus or choose it.
		return []huh.Option[string]{newBranch}
	}
	return []huh.Option[string]{
		huh.NewOption(configureRadioLabel("Push to "+branch, selected == configureSaveDirect), configureSaveDirect),
		newBranch,
	}
}

func configureSaveDescription(branch string, protected bool) string {
	if !protected {
		return ""
	}
	// The huh help bar already documents navigation and submission. The only
	// description needed here is the visible but non-selectable destination.
	return lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("  ○ Push to " + branch + " — protected branch")
}

func configureRadioLabel(label string, selected bool) string {
	marker := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("○")
	if selected {
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Success)).Render("●")
	}
	return marker + " " + label
}

func newConfigureForm(groups ...*huh.Group) *huh.Form {
	// Do not start with NewAccessibleForm here: huh fields retain the first
	// theme assigned to them, so applying configureFormTheme afterward would
	// leave the standard rail and `>` cursor in place. Install this theme first.
	form := huh.NewForm(groups...).WithTheme(configureFormTheme())
	if IsAccessibleMode() {
		form = form.WithAccessible(true)
	}
	return form
}

func configureFormTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		theme := uiform.Theme().Theme(isDark)
		success := lipgloss.Color(palette.Success)
		muted := lipgloss.Color(palette.Muted)

		// Radio and checkbox lists use the same active-row indicator. Selection
		// remains green and independent from the orange `>` cursor.
		theme.Focused.Base = lipgloss.NewStyle()
		theme.Blurred.Base = lipgloss.NewStyle()
		theme.Focused.SelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Warning)).SetString("> ")
		theme.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
		theme.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Warning)).SetString("> ")
		theme.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
		theme.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(success).SetString("◼ ")
		theme.Blurred.SelectedPrefix = lipgloss.NewStyle().Foreground(success).SetString("◼ ")
		theme.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(muted).SetString("◻ ")
		theme.Blurred.UnselectedPrefix = lipgloss.NewStyle().Foreground(muted).SetString("◻ ")
		theme.Focused.SelectedOption = theme.Focused.SelectedOption.UnsetForeground()
		theme.Blurred.SelectedOption = theme.Blurred.SelectedOption.UnsetForeground()
		theme.Focused.UnselectedOption = theme.Focused.UnselectedOption.Foreground(muted)
		theme.Blurred.UnselectedOption = theme.Blurred.UnselectedOption.Foreground(muted)
		return theme
	})
}

func configureQuestionTitle(question string, focused bool) string {
	color := palette.Muted
	if focused {
		color = palette.Warning
	}
	marker := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("?")
	return marker + " " + question
}

func configureAgentOptions(options []huh.Option[string], selected map[types.AgentName]struct{}) []huh.Option[string] {
	styled := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		name := types.AgentName(option.Value)
		dot := lipgloss.NewStyle().Foreground(lipgloss.Color(configureAgentColor(name))).Render("●")
		item := huh.NewOption(dot+" "+option.Key, option.Value)
		if _, ok := selected[name]; ok {
			item = item.Selected(true)
		}
		styled = append(styled, item)
	}
	return styled
}

func configureAgentColor(name types.AgentName) string {
	switch name {
	case agent.AgentNameClaudeCode:
		return "#e8864a"
	case agent.AgentNameCodex:
		return "#34a37f"
	case agent.AgentNameCopilotCLI:
		return "#d162c4"
	case agent.AgentNameGemini:
		return "#5b93e8"
	case agent.AgentNameOpenCode:
		return "#4cc3c9"
	case agent.AgentNamePi:
		return "#e6c04a"
	default:
		return palette.Muted
	}
}

func configureCurrentUpstream(ctx context.Context, repoRoot string) string {
	raw, err := gitremote.GetRemoteURLInDir(ctx, repoRoot, defaultMirrorRemote)
	if err != nil {
		return ""
	}
	info, err := gitremote.ParseURL(raw)
	if err != nil || info.Protocol != gitremote.ProtocolEntire {
		return ""
	}
	return info.Host
}

func configurePlacementOrder(placements []coreapi.ResolvedPlacement, currentHost, jurisdiction string) []coreapi.ResolvedPlacement {
	ordered := append([]coreapi.ResolvedPlacement(nil), placements...)
	sort.SliceStable(ordered, func(i, j int) bool {
		iCurrent := strings.EqualFold(ordered[i].ClusterHost, currentHost)
		jCurrent := strings.EqualFold(ordered[j].ClusterHost, currentHost)
		if iCurrent != jCurrent {
			return iCurrent
		}
		iHome := strings.EqualFold(ordered[i].Jurisdiction.Or(""), jurisdiction)
		jHome := strings.EqualFold(ordered[j].Jurisdiction.Or(""), jurisdiction)
		if currentHost == "" && iHome != jHome {
			return iHome
		}
		return configurePlacementLabel(ordered[i]) < configurePlacementLabel(ordered[j])
	})
	return ordered
}

func configurePlacementLabel(placement coreapi.ResolvedPlacement) string {
	if jurisdiction := strings.TrimSpace(placement.Jurisdiction.Or("")); jurisdiction != "" {
		return strings.ToUpper(jurisdiction)
	}
	if cell := strings.TrimSpace(placement.Cell.Or("")); cell != "" {
		return cell
	}
	return placement.ClusterHost
}

func configureAgentPreselection(ctx context.Context) map[types.AgentName]struct{} {
	selected := make(map[types.AgentName]struct{})
	if installed := GetAgentsWithHooksInstalled(ctx); len(installed) > 0 {
		for _, name := range installed {
			selected[name] = struct{}{}
		}
		return selected
	}
	for _, detected := range agent.DetectAll(ctx) {
		if isBuiltInAgent(detected) {
			selected[detected.Name()] = struct{}{}
		}
	}
	return selected
}

func configureUseMirror(ctx context.Context, repoRoot, owner, repo string, chosen coreapi.ResolvedPlacement) error {
	remotes, err := listGitRemotes(ctx, repoRoot)
	if err != nil {
		return err
	}
	current := ""
	if remotes[defaultMirrorRemote] {
		current, err = gitremote.GetRemoteURLInDir(ctx, repoRoot, defaultMirrorRemote)
		if err != nil {
			return fmt.Errorf("read origin remote URL: %w", err)
		}
	}
	mirrorURL := mirrorCloneURL(chosen.ClusterHost, owner, repo)
	preserve := defaultMirrorUpstreamRemote
	// An existing forge alias already preserves direct GitHub access. Avoid
	// manufacturing another alias, and never preserve an old Entire placement
	// under the misleading name "upstream".
	if info, parseErr := gitremote.ParseURL(current); parseErr == nil && info.Protocol == gitremote.ProtocolEntire {
		preserve = ""
	}
	plan := planMirrorRemote(defaultMirrorRemote, mirrorURL, current, preserve, remotes)
	return applyMirrorRemotePlan(ctx, repoRoot, plan)
}

// configureGitChanges returns porcelain status keyed by path. Its values retain
// XY so callers can detect whether anything was already staged before the flow.
func configureGitChanges(ctx context.Context, repoRoot string) (map[string]string, error) {
	out, err := gitRunner(ctx, repoRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("read git status: %w", err)
	}
	changes := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if arrow := strings.LastIndex(path, " -> "); arrow >= 0 {
			path = path[arrow+4:]
		}
		if path != "" {
			changes[path] = line[:2]
		}
	}
	return changes, nil
}

func newConfigureChanges(before, after map[string]string) []string {
	paths := make([]string, 0, len(after))
	for path := range after {
		if _, existed := before[path]; !existed {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func configureSaveAndPush(cmd *cobra.Command, repoRoot, owner, repo string, before map[string]string, generated []string, now func() time.Time) error {
	outW := cmd.OutOrStdout()
	if len(generated) == 0 {
		fmt.Fprintln(outW, "  No new configuration changes to commit.")
		return nil
	}
	if len(before) != 0 {
		fmt.Fprintln(outW, "  Configuration was saved but not committed because the worktree already had changes.")
		fmt.Fprintln(outW, "  Review and commit the configuration normally when ready.")
		return nil
	}

	branch, err := gitRunner(cmd.Context(), repoRoot, "branch", "--show-current")
	if err != nil || branch == "" {
		return errors.New("cannot save configuration from a detached HEAD")
	}
	protected, protectionErr := configureBranchProtected(cmd.Context(), owner, repo, branch)
	if protectionErr != nil {
		// Protection discovery is advisory; a rejected direct push still fails
		// safely at git push.
		protected = false
	}
	choice, err := promptConfigureSave(cmd.Context(), cmd.ErrOrStderr(), branch, protected)
	if err != nil {
		return err
	}
	if protected && choice == configureSaveDirect {
		return fmt.Errorf("%s is protected; push to a new branch", branch)
	}
	if choice != configureSaveDirect && choice != configureSaveNewBranch {
		return fmt.Errorf("unknown save choice %q", choice)
	}

	pushBranch := branch
	if choice == configureSaveNewBranch {
		var err error
		pushBranch, err = availableConfigureBranch(cmd.Context(), repoRoot, now)
		if err != nil {
			return err
		}
		if _, err := gitRunner(cmd.Context(), repoRoot, "switch", "-c", pushBranch); err != nil {
			return fmt.Errorf("create configuration branch: %w", err)
		}
	}

	args := []string{"add", "--"}
	args = append(args, generated...)
	if _, err := gitRunner(cmd.Context(), repoRoot, args...); err != nil {
		return fmt.Errorf("stage configuration: %w", err)
	}
	if _, err := gitRunner(cmd.Context(), repoRoot, "commit", "-m", configureCommitMessage); err != nil {
		return fmt.Errorf("commit configuration: %w", err)
	}
	sha, err := gitRunner(cmd.Context(), repoRoot, "rev-parse", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("read configuration commit: %w", err)
	}
	fmt.Fprintf(outW, "✓ Committed %s · %s\n", configureCommitMessage, sha)
	if _, err := gitRunner(cmd.Context(), repoRoot, "push", "-u", defaultMirrorRemote, pushBranch); err != nil {
		return fmt.Errorf("push configuration: %w", err)
	}
	fmt.Fprintf(outW, "✓ Pushed to %s/%s\n", defaultMirrorRemote, pushBranch)
	if choice == configureSaveNewBranch {
		fmt.Fprintln(outW, "  Open a trail to merge it:")
		fmt.Fprintf(outW, "  %s/gh/%s/%s/trails/new?branch=%s\n", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(pushBranch))
	}
	return nil
}

func configureBranchProtected(ctx context.Context, owner, repo, branch string) (bool, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", "repos/"+owner+"/"+repo+"/branches/"+url.PathEscape(branch), "--jq", ".protected").Output()
	if err != nil {
		return false, fmt.Errorf("check GitHub branch protection: %w", err)
	}
	protected, err := strconv.ParseBool(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("parse GitHub branch protection: %w", err)
	}
	return protected, nil
}

func availableConfigureBranch(ctx context.Context, repoRoot string, now func() time.Time) (string, error) {
	candidates := []string{configureBranchBase, configureBranchBase + "-" + now().Format("20060102-150405")}
	for _, candidate := range candidates {
		_, err := gitRunner(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+candidate)
		if err == nil {
			continue // branch already exists
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return candidate, nil // show-ref uses 1 for a missing ref
		}
		return "", fmt.Errorf("check configuration branch %q: %w", candidate, err)
	}
	return "", errors.New("could not choose an unused configuration branch name")
}
