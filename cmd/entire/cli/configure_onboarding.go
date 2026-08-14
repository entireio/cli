package cli

import (
	"context"
	"encoding/json"
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

var (
	errConfigureGHUnavailable     = errors.New("gh CLI is not installed")
	errConfigureGHUnauthenticated = errors.New("gh CLI is not authenticated")
)

const (
	configureAccessPollInterval = 2 * time.Second
	configureAccessWaitTimeout  = 5 * time.Minute
	configureCommitMessage      = "chore: configure entire"
	configureBranchBase         = "configure-entire"
	configureSaveDirect         = "direct"
	configureSaveNewBranch      = "new-branch"
	configureSaveLocal          = "local"
	configureSaveCancel         = "cancel"
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
		if err := ensureConfigureRepoAccess(ctx, outW, errW, reporter, access, cleanRemote, owner, repo, opts.Yes, deps); err != nil {
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
		placements, err = configureCreateMirrors(ctx, outW, errW, client, owner, repo, profile.Jurisdiction, opts.Yes)
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
	initialChanges, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	branch, branchErr := gitRunner(ctx, repoRoot, "branch", "--show-current")
	if branchErr != nil {
		// Best-effort: local-only saves do not need a branch.
		branch = ""
	}
	protected := false
	protectionKnown := branch == ""
	if branch != "" {
		if detected, protectionErr := configureBranchProtected(ctx, owner, repo, branch); protectionErr == nil {
			protected = detected
			protectionKnown = true
		}
	}
	// Non-interactive onboarding has no confirmation step. If GitHub protection
	// cannot be checked (common when gh is unavailable in CI), fail closed and
	// use the review branch rather than pushing directly to the checked-out
	// branch. Interactive users can still explicitly choose a direct push when
	// protection is known not to block it.
	protected = configureEffectiveProtection(protected, protectionKnown, opts.Yes)
	branchPushSafe := false
	if branch != "" && len(initialChanges) == 0 {
		branchPushSafe = configureBranchHasNoUnpushedCommits(ctx, repoRoot)
	}
	chosen, selectedAgents, saveChoice, agentsChanged, err := promptConfigureUpstreamAndAgents(
		ctx, errW, repoRoot, placements, profile.Jurisdiction, manageRegionsHint,
		branch, protected, branchPushSafe, opts.Yes,
	)
	if err != nil {
		return err
	}
	if saveChoice == configureSaveCancel {
		fmt.Fprintln(outW, "Configuration cancelled.")
		return nil
	}
	if saveChoice == "" {
		fmt.Fprintln(outW, "No configuration changes.")
		return nil
	}

	// Snapshot immediately before applying our changes. The form may have been
	// open for an arbitrary amount of time, so the pre-form status is not a safe
	// commit baseline: editor autosaves or another process may have changed files
	// while the user was choosing options. Anything already dirty now is excluded
	// from generated and disables automatic commit/push below.
	beforeApply, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	if (saveChoice == configureSaveDirect || saveChoice == configureSaveNewBranch) &&
		(len(beforeApply) != 0 || !configureBranchHasNoUnpushedCommits(ctx, repoRoot)) {
		saveChoice = configureSaveLocal
	}

	forgeRemote, err := configureUseMirror(ctx, outW, errW, repoRoot, owner, repo, chosen)
	if err != nil {
		return err
	}

	if agentsChanged {
		opts = configureOnboardingEnableOptions(opts)
		if err := runEnableInteractive(ctx, outW, selectedAgents, opts); err != nil {
			return err
		}
		// A local enabled:false override must not win over the newly written project
		// configuration. setEnabledFlag updates the project and synchronizes an
		// existing local override without replacing its other local-only fields.
		if err := setEnabledFlag(ctx, true, true); err != nil {
			return err
		}
	}

	after, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	generated := newConfigureChanges(beforeApply, after)
	if agentsChanged {
		fmt.Fprintf(outW, "✓ Wrote %s\n", configDisplayProject)
	}

	if saveChoice == configureSaveDirect || saveChoice == configureSaveNewBranch {
		if err := configureSaveAndPush(cmd, repoRoot, owner, repo, beforeApply, generated, branch, protected, saveChoice, forgeRemote, deps.now); err != nil {
			return err
		}
	} else if agentsChanged && len(generated) > 0 {
		fmt.Fprintln(outW, "  Configuration was saved locally. Review and commit it normally when ready.")
	}
	fmt.Fprintln(outW, "✓ Configuration complete")
	fmt.Fprintln(outW)
	fmt.Fprintln(outW, "  You're set. Start a session with any selected agent —")
	fmt.Fprintf(outW, "  checkpoints will show up on %s/gh/%s/%s\n", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
	return nil
}

func configureOnboardingEnableOptions(opts EnableOptions) EnableOptions {
	opts.Yes = true
	opts.UseProjectSettings = true
	opts.UseLocalSettings = false
	opts.SuppressDoneMessage = true
	opts.SuppressAdditionalSetup = true
	// Logged-in onboarding does not ask a separate telemetry question. The
	// documented default is applied without prompting; existing settings and
	// ENTIRE_TELEMETRY_OPTOUT remain authoritative.
	return opts
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
		if err := api.RequireSecureURL(target.coreURL); err != nil {
			return nil, fmt.Errorf("context login server URL check: %w", err)
		}
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
	if err := api.RequireSecureURL(target.coreURL); err != nil {
		return nil, fmt.Errorf("context login server URL check: %w", err)
	}
	profile, err := deps.fetchProfile(ctx, target.coreURL, target.token)
	if err != nil {
		return nil, fmt.Errorf("validate login: %w", err)
	}
	fmt.Fprintf(outW, "✓ Logged in as %s\n", profile.Handle)
	return profile, nil
}

func ensureConfigureRepoAccess(ctx context.Context, outW, errW io.Writer, reporter configureAccessReporter, initial *api.EnableRepoResponse, cleanRemote, owner, repo string, nonInteractive bool, deps configureFlowDeps) error {
	fmt.Fprintf(errW, "✗ Entire has no access to %s/%s\n\n", owner, repo)
	installURL := strings.TrimSpace(initial.InstallURL)
	if installURL == "" {
		installURL = fmt.Sprintf("%s/install?repo=%s%%2F%s", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
	}

	admin, adminErr := deps.githubAdmin(ctx, owner, repo)
	if adminErr == nil && !admin {
		fmt.Fprintln(errW, "  An admin needs to install the GitHub app first.")
		fmt.Fprintln(errW, "  Send them this link to continue:")
		fmt.Fprintf(errW, "\n  %s\n", installURL)
		return NewSilentError(errors.New("GitHub app installation required"))
	}
	if adminErr != nil {
		switch {
		case errors.Is(adminErr, errConfigureGHUnavailable):
			fmt.Fprintln(errW, "  Could not verify GitHub admin access because the gh CLI is not installed.")
			fmt.Fprintln(errW, "  Install it from https://cli.github.com/ and run `gh auth login` for future checks.")
		case errors.Is(adminErr, errConfigureGHUnauthenticated):
			fmt.Fprintln(errW, "  Could not verify GitHub admin access because the gh CLI is not authenticated.")
			fmt.Fprintln(errW, "  Run `gh auth login` for future checks.")
		default:
			fmt.Fprintf(errW, "  Could not verify GitHub admin access: %v\n", adminErr)
		}
		fmt.Fprintln(errW, "  If you administer this repository, you can still continue with the installation link.")
		fmt.Fprintln(errW)
	}

	fmt.Fprintln(outW, "  Install the GitHub app to grant access:")
	fmt.Fprintf(outW, "  %s\n\n", installURL)
	if nonInteractive {
		return NewSilentError(errors.New("GitHub app installation required; install it with the URL above, then rerun 'entire configure --yes'"))
	}
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
	runner := execRunner{}
	if !ghAvailable(ctx, runner) {
		return false, errConfigureGHUnavailable
	}
	if !ghAuthenticated(ctx, runner) {
		return false, errConfigureGHUnauthenticated
	}
	out, err := runner.Run(ctx, "gh", "api", "repos/"+owner+"/"+repo, "--jq", ".permissions.admin")
	if err != nil {
		return false, fmt.Errorf("check GitHub admin permission: %w", err)
	}
	admin, err := strconv.ParseBool(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("parse GitHub admin permission: %w", err)
	}
	return admin, nil
}

func configureCreateMirrors(ctx context.Context, outW, errW io.Writer, client *coreapi.Client, owner, repo, jurisdiction string, nonInteractive bool) ([]coreapi.ResolvedPlacement, error) {
	regions, err := availableRegions(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	if len(regions) == 0 {
		return nil, errors.New("no regions available to mirror into")
	}
	selected, err := pickConfigureRegions(ctx, outW, regions, jurisdiction, nonInteractive)
	if err != nil {
		return nil, err
	}
	results := createMirrors(ctx, errW, mirrorTargets([]coreapi.AvailableMirror{{Owner: owner, Repo: repo}}, selected), false, 30*time.Minute)
	reportErr := reportMirrorResults(outW, errW, results)
	placements := configureSuccessfulMirrorPlacements(selected, results)
	if len(placements) > 0 {
		if reportErr != nil {
			fmt.Fprintf(errW, "Continuing with %d mirror(s) that succeeded.\n", len(placements))
		}
		return placements, nil
	}
	if reportErr != nil {
		return nil, reportErr
	}
	return nil, errors.New("no usable mirrors were created")
}

func configureSuccessfulMirrorPlacements(selected []regionChoice, results []mirrorResult) []coreapi.ResolvedPlacement {
	placements := make([]coreapi.ResolvedPlacement, 0, len(results))
	for i, result := range results {
		if i >= len(selected) || result.err != nil || result.cloneURL == "" {
			continue
		}
		region := selected[i]
		placements = append(placements, coreapi.ResolvedPlacement{
			ClusterHost:  region.host,
			Cell:         coreapi.NewOptString(region.slug),
			Jurisdiction: coreapi.NewOptString(region.jurisdiction),
		})
	}
	return placements
}

func pickConfigureRegions(ctx context.Context, outW io.Writer, regions []regionChoice, jurisdiction string, nonInteractive bool) ([]regionChoice, error) {
	opts, defaults := clusterChoices(regions, jurisdiction)
	byHost := make(map[string]regionChoice, len(regions))
	for _, region := range regions {
		byHost[region.host] = region
	}
	selected := append([]string(nil), defaults...)
	if nonInteractive {
		if len(selected) == 0 && len(opts) > 0 {
			selected = append(selected, opts[0].Value)
		}
		chosen := make([]regionChoice, 0, len(selected))
		for _, host := range selected {
			if region, ok := byHost[host]; ok {
				chosen = append(chosen, region)
			}
		}
		return chosen, nil
	}
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

	value         *string
	committed     string
	highlighted   string
	refresh       func()
	layoutChanged func()
	window        tea.WindowSizeMsg
	focused       bool
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

func (field *configureUpstreamField) View() string {
	return field.Select.View() + "\n\n"
}

func (field *configureUpstreamField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case configureRadioSelectKey(keyMsg.String()):
			field.commitHighlighted()
			return field, configureWindowResize(field.window)
		case configureNextKey(keyMsg.String()):
			// Enter continues with the intentionally selected value. Merely moving
			// the cursor does not change the radio selection.
			return field, huh.NextField
		}
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		field.window = size
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
	if field.layoutChanged != nil {
		field.layoutChanged()
	}
}

type configureAgentField struct {
	*huh.MultiSelect[string]

	value            *[]string
	selectionChanged func()
	showSave         func() bool
	window           tea.WindowSizeMsg
	focused          bool
}

func (field *configureAgentField) Focus() tea.Cmd {
	field.focused = true
	return tea.Batch(field.MultiSelect.Focus(), configureRefreshFocus())
}

func (field *configureAgentField) Blur() tea.Cmd {
	field.focused = false
	return field.MultiSelect.Blur()
}

func (field *configureAgentField) View() string {
	view := field.MultiSelect.View()
	if field.showSave != nil && field.showSave() {
		view += "\n\n"
	}
	return view
}

func (field *configureAgentField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		field.window = size
	}
	var before []string
	if field.value != nil {
		before = append(before, (*field.value)...)
	}
	model, cmd := field.MultiSelect.Update(msg)
	if updated, ok := model.(*huh.MultiSelect[string]); ok {
		field.MultiSelect = updated
	}
	if _, keyPress := msg.(tea.KeyPressMsg); keyPress && field.value != nil && !sameStrings(before, *field.value) {
		if field.selectionChanged != nil {
			field.selectionChanged()
		}
		cmd = tea.Batch(cmd, configureWindowResize(field.window))
	}
	return field, cmd
}

type configureSaveField struct {
	*huh.Select[string]

	focused bool
}

func (field *configureSaveField) Focus() tea.Cmd {
	field.focused = true
	return tea.Batch(field.Select.Focus(), configureRefreshFocus())
}

func (field *configureSaveField) Blur() tea.Cmd {
	field.focused = false
	return field.Select.Blur()
}

func (field *configureSaveField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	model, cmd := field.Select.Update(msg)
	if updated, ok := model.(*huh.Select[string]); ok {
		field.Select = updated
	}
	return field, cmd
}

func configureWindowResize(size tea.WindowSizeMsg) tea.Cmd {
	if size.Width <= 0 || size.Height <= 0 {
		return nil
	}
	return func() tea.Msg { return size }
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		seen[value]--
		if seen[value] < 0 {
			return false
		}
	}
	return true
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

// promptConfigureUpstreamAndAgents presents upstream, agents, and the dynamic
// save action as one persistent form. Save is always visible, while its actions
// change based on whether the choices require only local state or a commit and
// push. Arrow keys stay within the active section; Enter advances, while
// shift+tab revisits the previous section.
func promptConfigureUpstreamAndAgents(ctx context.Context, errW io.Writer, repoRoot string, placements []coreapi.ResolvedPlacement, jurisdiction, manageRegionsHint, branch string, protected, cleanWorktree, nonInteractive bool) (coreapi.ResolvedPlacement, []agent.Agent, string, bool, error) {
	currentHost := configureCurrentUpstream(ctx, repoRoot)
	placements = configurePlacementOrder(placements, currentHost, jurisdiction)
	if len(placements) == 0 {
		return coreapi.ResolvedPlacement{}, nil, "", false, errors.New("no upstreams available")
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
		return coreapi.ResolvedPlacement{}, nil, "", false, errors.New("no agents with hook support available")
	}
	installedAgentNames := make([]string, 0)
	for _, name := range GetAgentsWithHooksInstalled(ctx) {
		installedAgentNames = append(installedAgentNames, string(name))
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
			return configureQuestionTitle("Select your upstream", upstreamControl.focused)
		}, &upstreamControl.focused).
		Description(upstreamDescription).
		Options(upstreamOptions()...).
		Height(configureFieldHeight(len(placements), upstreamDescription)).
		Value(&selectedHost)
	upstreamControl.refresh = func() {
		upstreamControl.Options(upstreamOptions()...)
	}

	agentControl := newConfigureAgentControl(agentOptions, &selectedAgentNames, true)

	mirrorChanged := func() bool {
		mirror, _ := configureSelectionChanges(selectedHost, currentHost, selectedAgentNames, installedAgentNames)
		return mirror
	}
	agentsChanged := func() bool {
		_, agents := configureSelectionChanges(selectedHost, currentHost, selectedAgentNames, installedAgentNames)
		return agents
	}
	requiresPush := func() bool {
		return agentsChanged() && cleanWorktree && branch != ""
	}
	hasChanges := func() bool {
		return mirrorChanged() || agentsChanged()
	}

	previousHasChanges := hasChanges()
	saveChoice := defaultConfigureSaveChoice(previousHasChanges, requiresPush(), protected)
	saveControl := &configureSaveField{}
	saveOptions := func() []huh.Option[string] {
		return configureSaveOptions(branch, protected, requiresPush(), hasChanges(), saveChoice)
	}
	saveControl.Select = huh.NewSelect[string]().
		TitleFunc(func() string {
			return configureQuestionTitle("Save configuration", saveControl.focused)
		}, &saveControl.focused).
		Description(configureSaveDescription(branch, protected, requiresPush(), hasChanges())).
		Options(saveOptions()...).
		Height(configureFieldHeight(len(saveOptions()), configureSaveDescription(branch, protected, requiresPush(), hasChanges()))).
		Value(&saveChoice)
	refreshSave := func() {
		changed := hasChanges()
		options := saveOptions()
		if changed != previousHasChanges || !configureOptionsContain(options, saveChoice) {
			saveChoice = defaultConfigureSaveChoice(changed, requiresPush(), protected)
			options = saveOptions()
		}
		previousHasChanges = changed
		description := configureSaveDescription(branch, protected, requiresPush(), changed)
		saveControl.Select.
			Description(description).
			Height(configureFieldHeight(len(options), description)).
			Options(options...)
	}
	upstreamControl.layoutChanged = refreshSave
	agentControl.selectionChanged = refreshSave
	agentControl.showSave = func() bool { return true }

	if nonInteractive {
		chosen, ok := placementByHost[selectedHost]
		if !ok {
			return coreapi.ResolvedPlacement{}, nil, "", false, errors.New("default upstream is no longer available")
		}
		selectedAgents, err := configureSelectedAgents(selectedAgentNames)
		if err != nil {
			return coreapi.ResolvedPlacement{}, nil, "", false, err
		}
		return chosen, selectedAgents, configureNonInteractiveSaveChoice(hasChanges(), requiresPush(), protected), agentsChanged(), nil
	}

	group := huh.NewGroup(upstreamControl, agentControl, saveControl)
	form := newConfigureForm(group)
	// Separate the form from the shell prompt or preceding onboarding status.
	fmt.Fprintln(errW)
	if err := form.RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Configure", err); cancelErr != nil {
			return coreapi.ResolvedPlacement{}, nil, "", false, cancelErr
		}
		return coreapi.ResolvedPlacement{}, nil, "", false, NewSilentError(errors.New("configure cancelled"))
	}

	chosen, ok := placementByHost[selectedHost]
	if !ok {
		return coreapi.ResolvedPlacement{}, nil, "", false, errors.New("selected upstream is no longer available")
	}
	selectedAgents, err := configureSelectedAgents(selectedAgentNames)
	if err != nil {
		return coreapi.ResolvedPlacement{}, nil, "", false, err
	}
	if !hasChanges() && saveChoice != configureSaveCancel {
		saveChoice = ""
	}
	return chosen, selectedAgents, saveChoice, agentsChanged(), nil
}

func newConfigureAgentControl(options []huh.Option[string], selected *[]string, requireOne bool) *configureAgentField {
	control := &configureAgentField{value: selected}
	field := huh.NewMultiSelect[string]().
		TitleFunc(func() string {
			return configureQuestionTitle("Select the agents for this repository", control.focused)
		}, &control.focused).
		Options(options...).
		// MultiSelect subtracts its title from an implicit viewport height, so
		// include exactly one title row to show every option without padding.
		Height(len(options) + 1).
		Value(selected)
	if requireOne {
		field = field.Validate(func(values []string) error {
			if len(values) == 0 {
				return errors.New("select at least one agent")
			}
			return nil
		})
	}
	control.MultiSelect = field
	return control
}

func promptConfigureAgentSelection(ctx context.Context, errW io.Writer, options []huh.Option[string], selected *[]string) error {
	control := newConfigureAgentControl(options, selected, false)
	fmt.Fprintln(errW)
	if err := newConfigureForm(huh.NewGroup(control)).RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Agent selection", err); cancelErr != nil {
			return cancelErr
		}
		return NewSilentError(errors.New("agent selection cancelled"))
	}
	return nil
}

func configureSelectedAgents(names []string) ([]agent.Agent, error) {
	selected := make([]agent.Agent, 0, len(names))
	for _, name := range names {
		ag, err := agent.Get(types.AgentName(name))
		if err != nil {
			return nil, fmt.Errorf("load selected agent %q: %w", name, err)
		}
		selected = append(selected, ag)
	}
	return selected, nil
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

func configureSelectionChanges(selectedHost, currentHost string, selectedAgents, installedAgents []string) (mirrorChanged, agentsChanged bool) {
	return !strings.EqualFold(selectedHost, currentHost), !sameStrings(selectedAgents, installedAgents)
}

func configureNonInteractiveSaveChoice(hasChanges, requiresPush, protected bool) string {
	if !hasChanges {
		return ""
	}
	return defaultConfigureSaveChoice(true, requiresPush, protected)
}

func configureEffectiveProtection(protected, known, nonInteractive bool) bool {
	return protected || (nonInteractive && !known)
}

func defaultConfigureSaveChoice(hasChanges, requiresPush, protected bool) string {
	if !hasChanges {
		return configureSaveCancel
	}
	if !requiresPush {
		return configureSaveLocal
	}
	if protected {
		return configureSaveNewBranch
	}
	return configureSaveDirect
}

func configureSaveOptions(branch string, protected, requiresPush, hasChanges bool, _ string) []huh.Option[string] {
	cancel := huh.NewOption("Cancel", configureSaveCancel)
	if !hasChanges {
		return []huh.Option[string]{cancel}
	}
	if !requiresPush {
		return []huh.Option[string]{
			huh.NewOption("Save", configureSaveLocal),
			cancel,
		}
	}

	newBranch := huh.NewOption("Save — push to a new branch, review before it lands", configureSaveNewBranch)
	if protected {
		// huh has no disabled-option primitive. Keep the protected destination
		// visible in the description, but omit it from keyboard navigation.
		return []huh.Option[string]{newBranch, cancel}
	}
	return []huh.Option[string]{
		huh.NewOption("Save — push to "+branch, configureSaveDirect),
		newBranch,
		cancel,
	}
}

func configureSaveDescription(branch string, protected, requiresPush, hasChanges bool) string {
	if !hasChanges {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("  Save — no changes")
	}
	if !requiresPush || !protected {
		return ""
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("  Save — push to " + branch + " — protected branch")
}

func configureOptionsContain(options []huh.Option[string], value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
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
		theme.FieldSeparator = lipgloss.NewStyle().SetString("")
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
	heading := lipgloss.NewStyle().Bold(true).Render(question)
	return marker + " " + heading
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
	if jurisdiction := strings.ToLower(strings.TrimSpace(placement.Jurisdiction.Or(""))); jurisdiction != "" {
		if name, ok := configureJurisdictionNames[jurisdiction]; ok {
			return name
		}
		return strings.ToUpper(jurisdiction)
	}
	if cell := strings.TrimSpace(placement.Cell.Or("")); cell != "" {
		return cell
	}
	return placement.ClusterHost
}

var configureJurisdictionNames = map[string]string{ //nolint:gochecknoglobals // immutable display-name lookup
	"au": "Australia",
	"eu": "European Union",
	"in": "India",
	"us": "United States",
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

func configureUseMirror(ctx context.Context, outW, errW io.Writer, repoRoot, owner, repo string, chosen coreapi.ResolvedPlacement) (string, error) {
	remotes, err := listGitRemotes(ctx, repoRoot)
	if err != nil {
		return "", err
	}
	current := ""
	if remotes[defaultMirrorRemote] {
		current, err = gitremote.GetRemoteURLInDir(ctx, repoRoot, defaultMirrorRemote)
		if err != nil {
			return "", fmt.Errorf("read origin remote URL: %w", err)
		}
	}

	forgeRemote, err := configureExistingForgeRemote(ctx, repoRoot, owner, repo, remotes, defaultMirrorRemote)
	if err != nil {
		return "", err
	}
	preserve := ""
	if info, parseErr := gitremote.ParseURL(current); parseErr == nil && info.Protocol != gitremote.ProtocolEntire {
		if forgeRemote == "" {
			preserve = availableConfigureRemoteName(remotes, defaultMirrorUpstreamRemote, mirrorCloneProviderGitHub)
			forgeRemote = preserve
		}
	}
	if forgeRemote == "" {
		forgeRemote = availableConfigureRemoteName(remotes, mirrorCloneProviderGitHub)
		forgeURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		if _, err := gitRunner(ctx, repoRoot, "remote", "add", forgeRemote, forgeURL); err != nil {
			return "", fmt.Errorf("add GitHub push remote %q: %w", forgeRemote, err)
		}
		remotes[forgeRemote] = true
		fmt.Fprintf(outW, "✓ Added GitHub push remote %q\n  %s\n", forgeRemote, forgeURL)
	}

	mirrorURL := mirrorCloneURL(chosen.ClusterHost, owner, repo)
	plan := planMirrorRemote(defaultMirrorRemote, mirrorURL, current, preserve, remotes)
	if err := applyMirrorRemotePlan(ctx, repoRoot, plan); err != nil {
		return "", err
	}
	// Report the replaced URL and where it remains reachable. The returned
	// forgeRemote is deliberately separate from the mirror fetch remote: config
	// commits must land on GitHub, never in the one-way Entire mirror.
	reportMirrorRemotePlan(outW, errW, plan)
	return forgeRemote, nil
}

func configureExistingForgeRemote(ctx context.Context, repoRoot, owner, repo string, remotes map[string]bool, exclude string) (string, error) {
	names := make([]string, 0, len(remotes))
	for name := range remotes {
		if name != exclude {
			names = append(names, name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		priority := func(name string) int {
			switch name {
			case mirrorCloneProviderGitHub:
				return 0
			case defaultMirrorUpstreamRemote:
				return 1
			default:
				return 2
			}
		}
		if priority(names[i]) != priority(names[j]) {
			return priority(names[i]) < priority(names[j])
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		raw, err := gitremote.GetRemoteURLInDir(ctx, repoRoot, name)
		if err != nil {
			return "", fmt.Errorf("read remote %q URL: %w", name, err)
		}
		info, err := gitremote.ParseURL(raw)
		if err == nil && info.Protocol != gitremote.ProtocolEntire && info.Forge == mirrorUseForge &&
			strings.EqualFold(info.Owner, owner) && strings.EqualFold(info.Repo, repo) {
			return name, nil
		}
	}
	return "", nil
}

func availableConfigureRemoteName(remotes map[string]bool, candidates ...string) string {
	for _, candidate := range candidates {
		if !remotes[candidate] {
			return candidate
		}
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("github-%d", i)
		if !remotes[candidate] {
			return candidate
		}
	}
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

func configureSaveAndPush(cmd *cobra.Command, repoRoot, owner, repo string, before map[string]string, generated []string, branch string, protected bool, choice, forgeRemote string, now func() time.Time) error {
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

	if branch == "" {
		return errors.New("cannot save configuration from a detached HEAD")
	}
	if protected && choice == configureSaveDirect {
		return fmt.Errorf("%s is protected; push to a new branch", branch)
	}
	if choice != configureSaveDirect && choice != configureSaveNewBranch {
		return fmt.Errorf("unknown save choice %q", choice)
	}
	if forgeRemote == "" {
		return errors.New("no GitHub remote available for configuration push")
	}

	pushBranch := branch
	switchedBranch := false
	if choice == configureSaveNewBranch {
		var err error
		pushBranch, err = availableConfigureBranch(cmd.Context(), repoRoot, forgeRemote, now)
		if err != nil {
			return err
		}
		if _, err := gitRunner(cmd.Context(), repoRoot, "switch", "-c", pushBranch); err != nil {
			return fmt.Errorf("create configuration branch: %w", err)
		}
		switchedBranch = true
		defer func() {
			if switchedBranch {
				// Best-effort fallback for failures after branch creation. Successful
				// paths switch explicitly below so a restore error can be returned.
				if _, restoreErr := gitRunner(context.WithoutCancel(cmd.Context()), repoRoot, "switch", branch); restoreErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not return to original branch %q: %v\n", branch, restoreErr)
				}
			}
		}()
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
	if _, err := gitRunner(cmd.Context(), repoRoot, "push", "-u", forgeRemote, pushBranch); err != nil {
		return fmt.Errorf("push configuration to GitHub remote %q: %w", forgeRemote, err)
	}
	fmt.Fprintf(outW, "✓ Pushed to %s/%s\n", forgeRemote, pushBranch)
	if choice == configureSaveNewBranch {
		if _, err := gitRunner(cmd.Context(), repoRoot, "switch", branch); err != nil {
			return fmt.Errorf("return to original branch %q: %w", branch, err)
		}
		switchedBranch = false
		fmt.Fprintf(outW, "✓ Returned to %s\n", branch)
		fmt.Fprintln(outW, "  Open a trail to merge it:")
		fmt.Fprintf(outW, "  %s/gh/%s/%s/trails/new?branch=%s\n", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(pushBranch))
	}
	return nil
}

func configureBranchHasNoUnpushedCommits(ctx context.Context, repoRoot string) bool {
	// @{u} is the branch's configured upstream before configure rewrites any
	// remotes. If it does not exist, we cannot prove a direct push is isolated to
	// the generated config commit, so only the new-branch action is safe.
	upstream, err := gitRunner(ctx, repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil || upstream == "" {
		return false
	}
	ahead, err := gitRunner(ctx, repoRoot, "rev-list", "--count", upstream+"..HEAD")
	if err != nil {
		return false
	}
	count, err := strconv.Atoi(strings.TrimSpace(ahead))
	return err == nil && count == 0
}

func configureBranchProtected(ctx context.Context, owner, repo, branch string) (bool, error) {
	// GitHub's branch `.protected` flag is true for *any* active ruleset rule.
	// A non-blocking rule such as Copilot code review therefore made ordinary
	// feature branches look push-protected. Inspect the effective rules instead
	// and only require a new branch for rules that can reject a direct update.
	out, err := exec.CommandContext(ctx, "gh", "api", "repos/"+owner+"/"+repo+"/rules/branches/"+url.PathEscape(branch)).Output()
	if err != nil {
		return false, fmt.Errorf("check GitHub branch rules: %w", err)
	}
	return configureRulesBlockDirectPush(out)
}

func configureRulesBlockDirectPush(raw []byte) (bool, error) {
	var rules []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		return false, fmt.Errorf("parse GitHub branch rules: %w", err)
	}
	for _, rule := range rules {
		switch rule.Type {
		case "pull_request", "required_status_checks", "required_deployments", "required_signatures", "merge_queue", "update", "workflows":
			return true, nil
		}
	}
	return false, nil
}

func availableConfigureBranch(ctx context.Context, repoRoot, forgeRemote string, now func() time.Time) (string, error) {
	candidates := []string{configureBranchBase, configureBranchBase + "-" + now().Format("20060102-150405")}
	for _, candidate := range candidates {
		localExists, err := configureLocalBranchExists(ctx, repoRoot, candidate)
		if err != nil {
			return "", err
		}
		if localExists {
			continue
		}
		remoteExists, err := configureRemoteBranchExists(ctx, repoRoot, forgeRemote, candidate)
		if err != nil {
			return "", err
		}
		if !remoteExists {
			return candidate, nil
		}
	}
	return "", errors.New("could not choose an unused configuration branch name")
}

func configureLocalBranchExists(ctx context.Context, repoRoot, branch string) (bool, error) {
	_, err := gitRunner(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check local configuration branch %q: %w", branch, err)
}

func configureRemoteBranchExists(ctx context.Context, repoRoot, forgeRemote, branch string) (bool, error) {
	// Query the forge directly instead of relying on remote-tracking refs, which
	// may be stale or absent in a fresh clone. --exit-code returns 2 when no ref
	// matches, and any connectivity/auth failure is treated as an error rather
	// than risking reuse of another user's branch.
	_, err := gitRunner(ctx, repoRoot, "ls-remote", "--exit-code", "--heads", forgeRemote, "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		return false, nil
	}
	return false, fmt.Errorf("check remote configuration branch %q on %q: %w", branch, forgeRemote, err)
}
