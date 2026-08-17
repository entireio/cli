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

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
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
	configureSaveMaxFieldHeight = 4 // title plus the maximum three visible actions
	configureGitProtocolSSH     = "ssh"
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
		branchPushSafe = configureBranchHasNoUnpushedCommits(ctx, repoRoot, branch)
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
		(len(beforeApply) != 0 || !configureBranchHasNoUnpushedCommits(ctx, repoRoot, branch)) {
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
	control := uiform.NewChecklist(
		"Which regions should this repository live in?",
		"Your data stays in the selected regions.",
		opts, &selected, true,
	)
	fmt.Fprintln(outW)
	if err := uiform.New(huh.NewGroup(control)).RunWithContext(ctx); err != nil {
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
	upstreamControl := uiform.NewRadio(
		"Select your upstream", upstreamDescription, upstreamOptions(), &selectedHost,
	)
	upstreamControl.OnRefresh(func() {
		upstreamControl.Options(upstreamOptions()...)
	})

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
	saveOptions := func() []huh.Option[string] {
		return configureSaveOptions(branch, protected, requiresPush(), hasChanges(), saveChoice)
	}
	saveDescription := configureSaveDescription(branch, protected, requiresPush(), hasChanges())
	saveControl := newConfigureSaveControl(saveDescription, saveOptions(), &saveChoice)
	refreshSave := func() {
		changed := hasChanges()
		options := saveOptions()
		if changed != previousHasChanges || !configureOptionsContain(options, saveChoice) {
			saveChoice = defaultConfigureSaveChoice(changed, requiresPush(), protected)
			options = saveOptions()
		}
		previousHasChanges = changed
		description := configureSaveDescription(branch, protected, requiresPush(), changed)
		updateConfigureSaveControl(saveControl, description, options)
	}
	upstreamControl.OnLayoutChanged(refreshSave)
	agentControl.OnSelectionChanged(refreshSave)
	agentControl.ShowSectionGapWhen(func() bool { return true })

	if nonInteractive {
		chosen, ok := placementByHost[selectedHost]
		if !ok {
			return coreapi.ResolvedPlacement{}, nil, "", false, errors.New("default upstream is no longer available")
		}
		selection, selectionErr := configureNonInteractiveAgentSelection(selectedAgentNames, agentOptions)
		if selectionErr != nil {
			return coreapi.ResolvedPlacement{}, nil, "", false, selectionErr
		}
		selectedAgentNames = selection
		selectedAgents, err := configureSelectedAgents(selectedAgentNames)
		if err != nil {
			return coreapi.ResolvedPlacement{}, nil, "", false, err
		}
		return chosen, selectedAgents, configureNonInteractiveSaveChoice(hasChanges(), requiresPush(), protected), agentsChanged(), nil
	}

	group := huh.NewGroup(upstreamControl, agentControl, saveControl)
	form := uiform.New(group)
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

func newConfigureAgentControl(options []huh.Option[string], selected *[]string, requireOne bool) *uiform.Checklist[string] {
	return uiform.NewChecklist("Select the agents for this repository", "", options, selected, requireOne)
}

func promptConfigureAgentSelection(ctx context.Context, errW io.Writer, options []huh.Option[string], selected *[]string) error {
	control := newConfigureAgentControl(options, selected, false)
	fmt.Fprintln(errW)
	if err := uiform.New(huh.NewGroup(control)).RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Agent selection", err); cancelErr != nil {
			return cancelErr
		}
		return NewSilentError(errors.New("agent selection cancelled"))
	}
	return nil
}

func promptConfigureAgentsAndSave(ctx context.Context, errW io.Writer, installedNames []types.AgentName, branch string, protected, pushSafe bool) ([]string, string, bool, error) {
	external.DiscoverAndRegisterAlways(ctx)
	installedSet := make(map[types.AgentName]struct{}, len(installedNames))
	selectedNames := make([]string, 0, len(installedNames))
	for _, name := range installedNames {
		installedSet[name] = struct{}{}
		selectedNames = append(selectedNames, string(name))
	}
	options := configureAgentOptions(hookAgentOptions(installedSet), installedSet)
	if len(options) == 0 {
		return nil, "", false, errors.New("no agents with hook support available")
	}

	agentsChanged := func() bool {
		installed := make([]string, 0, len(installedNames))
		for _, name := range installedNames {
			installed = append(installed, string(name))
		}
		return !uiform.EqualValues(selectedNames, installed)
	}
	requiresPush := func() bool { return agentsChanged() && pushSafe && branch != "" }

	agentControl := newConfigureAgentControl(options, &selectedNames, false)
	previousHasChanges := agentsChanged()
	saveChoice := defaultConfigureSaveChoice(previousHasChanges, requiresPush(), protected)
	saveOptions := func() []huh.Option[string] {
		return configureSaveOptions(branch, protected, requiresPush(), agentsChanged(), saveChoice)
	}
	saveDescription := configureSaveDescription(branch, protected, requiresPush(), agentsChanged())
	saveControl := newConfigureSaveControl(saveDescription, saveOptions(), &saveChoice)
	refreshSave := func() {
		changed := agentsChanged()
		options := saveOptions()
		if changed != previousHasChanges || !configureOptionsContain(options, saveChoice) {
			saveChoice = defaultConfigureSaveChoice(changed, requiresPush(), protected)
			options = saveOptions()
		}
		previousHasChanges = changed
		description := configureSaveDescription(branch, protected, requiresPush(), changed)
		updateConfigureSaveControl(saveControl, description, options)
	}
	agentControl.OnSelectionChanged(refreshSave)
	agentControl.ShowSectionGapWhen(func() bool { return true })

	fmt.Fprintln(errW)
	if err := uiform.New(huh.NewGroup(agentControl, saveControl)).RunWithContext(ctx); err != nil {
		if cancelErr := handleFormCancellation(errW, "Agent configuration", err); cancelErr != nil {
			return nil, "", false, cancelErr
		}
		return nil, "", false, NewSilentError(errors.New("agent configuration cancelled"))
	}
	return selectedNames, saveChoice, agentsChanged(), nil
}

func runConfigureAgentManagement(cmd *cobra.Command, opts EnableOptions) error {
	cmd.SilenceUsage = true
	ctx := cmd.Context()
	outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return errors.New("not a git repository")
	}
	if !interactive.CanPromptInteractively() {
		return runManageAgents(ctx, outW, opts, nil)
	}

	initialChanges, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	branch, branchErr := gitRunner(ctx, repoRoot, "branch", "--show-current")
	if branchErr != nil {
		branch = ""
	}
	owner, repo, _, identityErr := configureRepoIdentity(ctx)
	forgeRemote := ""
	if identityErr == nil {
		remotes, remotesErr := listGitRemotes(ctx, repoRoot)
		if remotesErr != nil {
			return remotesErr
		}
		forgeRemote, err = configureExistingForgeRemote(ctx, repoRoot, owner, repo, remotes, "")
		if err != nil {
			return err
		}
	}
	protected := false
	if branch != "" && identityErr == nil {
		if detected, protectionErr := configureBranchProtected(ctx, owner, repo, branch); protectionErr == nil {
			protected = detected
		}
	}
	pushSafe := branch != "" && forgeRemote != "" && len(initialChanges) == 0 &&
		configureBranchMatchesRemoteHead(ctx, repoRoot, forgeRemote, "refs/heads/"+branch)
	installedNames := GetAgentsWithHooksInstalled(ctx)
	selectedNames, saveChoice, changed, err := promptConfigureAgentsAndSave(ctx, errW, installedNames, branch, protected, pushSafe)
	if err != nil {
		return err
	}
	if saveChoice == configureSaveCancel {
		fmt.Fprintln(outW, "Agent configuration cancelled.")
		return nil
	}
	if !changed {
		fmt.Fprintln(outW, "No agent configuration changes.")
		return nil
	}

	beforeApply, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	if (saveChoice == configureSaveDirect || saveChoice == configureSaveNewBranch) &&
		(len(beforeApply) != 0 || !configureBranchMatchesRemoteHead(ctx, repoRoot, forgeRemote, "refs/heads/"+branch)) {
		saveChoice = configureSaveLocal
	}
	if err := applyAgentChanges(ctx, outW, selectedNames, installedNames, opts); err != nil {
		return err
	}
	if saveChoice != configureSaveDirect && saveChoice != configureSaveNewBranch {
		return nil
	}
	afterApply, err := configureGitChanges(ctx, repoRoot)
	if err != nil {
		return err
	}
	generated := newConfigureChanges(beforeApply, afterApply)
	return configureSaveAndPush(cmd, repoRoot, owner, repo, beforeApply, generated, branch, protected, saveChoice, forgeRemote, time.Now)
}

func configureNonInteractiveAgentSelection(selected []string, options []huh.Option[string]) ([]string, error) {
	if len(selected) != 0 {
		return selected, nil
	}
	defaultAgent := agent.Default()
	if defaultAgent == nil {
		return nil, errors.New("no agents selected and no default agent is available")
	}
	defaultName := string(defaultAgent.Name())
	for _, option := range options {
		if option.Value == defaultName {
			return []string{defaultName}, nil
		}
	}
	return nil, fmt.Errorf("no agents selected and default agent %q does not support repository hooks", defaultName)
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

func configureSelectionChanges(selectedHost, currentHost string, selectedAgents, installedAgents []string) (mirrorChanged, agentsChanged bool) {
	return !strings.EqualFold(selectedHost, currentHost), !uiform.EqualValues(selectedAgents, installedAgents)
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

func newConfigureSaveControl(description string, options []huh.Option[string], value *string) *uiform.ActionSelect[string] {
	control := uiform.NewActionSelect("Save configuration", description, options, value)
	// Reserve room for the largest action set so a third action grows into the
	// blank row below instead of moving the whole form upward.
	control.Height(configureSaveMaxFieldHeight)
	return control
}

func updateConfigureSaveControl(control *uiform.ActionSelect[string], description string, options []huh.Option[string]) {
	control.Select.
		Description(description).
		Height(configureSaveMaxFieldHeight).
		Options(options...)
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

func configureAgentOptions(options []huh.Option[string], selected map[types.AgentName]struct{}) []huh.Option[string] {
	plain := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		item := huh.NewOption(option.Key, option.Value)
		if _, ok := selected[types.AgentName(option.Value)]; ok {
			item = item.Selected(true)
		}
		plain = append(plain, item)
	}
	return plain
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
		forgeURL := configureGitHubForgeURL(ctx, owner, repo)
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
	if err := configureRetargetCurrentBranch(ctx, repoRoot, forgeRemote); err != nil {
		return "", err
	}
	// Report the replaced URL and where it remains reachable. The returned
	// forgeRemote is deliberately separate from the mirror fetch remote: config
	// commits must land on GitHub, never in the one-way Entire mirror.
	reportMirrorRemotePlan(outW, errW, plan)
	return forgeRemote, nil
}

var configureGitHubProtocol = func(ctx context.Context) string { //nolint:gochecknoglobals // test seam for the user's gh protocol preference
	out, err := exec.CommandContext(ctx, "gh", "config", "get", "git_protocol", "--host", "github.com").Output()
	if err != nil {
		return "https"
	}
	protocol := strings.ToLower(strings.TrimSpace(string(out)))
	if protocol == configureGitProtocolSSH {
		return protocol
	}
	return "https"
}

func configureGitHubForgeURL(ctx context.Context, owner, repo string) string {
	if configureGitHubProtocol(ctx) == configureGitProtocolSSH {
		return fmt.Sprintf("git@github.com:%s/%s.git", owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
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
	configurationCommitted := false
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
				// paths restore explicitly below so an error can be returned.
				if restoreErr := restoreConfigureOriginalBranch(context.WithoutCancel(cmd.Context()), repoRoot, pushBranch, branch, generated, configurationCommitted); restoreErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not return configuration to original branch %q: %v\n", branch, restoreErr)
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
	configurationCommitted = true
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
		if err := restoreConfigureOriginalBranch(cmd.Context(), repoRoot, pushBranch, branch, generated, configurationCommitted); err != nil {
			return err
		}
		switchedBranch = false
		fmt.Fprintf(outW, "✓ Returned to %s with configuration applied locally\n", branch)
		fmt.Fprintln(outW, "  Open a trail to merge it:")
		fmt.Fprintf(outW, "  %s/gh/%s/%s/trails/new?branch=%s\n", configureWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(pushBranch))
	}
	return nil
}

func restoreConfigureOriginalBranch(ctx context.Context, repoRoot, sourceBranch, originalBranch string, generated []string, committed bool) error {
	if _, err := gitRunner(ctx, repoRoot, "switch", originalBranch); err != nil {
		return fmt.Errorf("return to original branch %q: %w", originalBranch, err)
	}

	var args []string
	if committed {
		// Switching to the original branch removes files introduced by the
		// configuration commit. Restore only the generated paths from the review
		// branch into the worktree, without touching the original branch's index,
		// so the repository remains configured while awaiting review.
		args = []string{"restore", "--source", sourceBranch, "--worktree", "--"}
	} else {
		// A failure before commit can carry staged generated files across the
		// switch. Unstage those paths while preserving their worktree contents.
		args = []string{"reset", "--"}
	}
	args = append(args, generated...)
	if _, err := gitRunner(ctx, repoRoot, args...); err != nil {
		return fmt.Errorf("restore configuration on original branch %q: %w", originalBranch, err)
	}
	return nil
}

func configureRetargetCurrentBranch(ctx context.Context, repoRoot, forgeRemote string) error {
	branch, err := gitRunner(ctx, repoRoot, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("read current branch before retargeting upstream: %w", err)
	}
	if branch == "" {
		return nil // detached HEAD; no branch to retarget
	}
	key := "branch." + branch + ".remote"
	trackingRemote, err := gitRunner(ctx, repoRoot, "config", "--get", key)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil // branch has no configured upstream
		}
		return fmt.Errorf("read tracking remote for branch %q: %w", branch, err)
	}
	if trackingRemote != defaultMirrorRemote || trackingRemote == forgeRemote {
		return nil
	}
	if _, err := gitRunner(ctx, repoRoot, "config", "--local", key, forgeRemote); err != nil {
		return fmt.Errorf("retarget branch %q to GitHub remote %q: %w", branch, forgeRemote, err)
	}
	return nil
}

func configureBranchHasNoUnpushedCommits(ctx context.Context, repoRoot, branch string) bool {
	if branch == "" {
		return false
	}
	// Compare HEAD with the live branch on the configured tracking remote. A
	// remote-tracking ref can remain stale after origin is repointed to the Entire
	// mirror, incorrectly making every later configure run look ahead forever.
	remote, err := gitRunner(ctx, repoRoot, "config", "--get", "branch."+branch+".remote")
	if err != nil || remote == "" || remote == "." {
		return false
	}
	mergeRef, err := gitRunner(ctx, repoRoot, "config", "--get", "branch."+branch+".merge")
	if err != nil || !strings.HasPrefix(mergeRef, "refs/heads/") {
		return false
	}
	return configureBranchMatchesRemoteHead(ctx, repoRoot, remote, mergeRef)
}

func configureBranchMatchesRemoteHead(ctx context.Context, repoRoot, remote, branchRef string) bool {
	if remote == "" || !strings.HasPrefix(branchRef, "refs/heads/") {
		return false
	}
	remoteHead, err := gitRunner(ctx, repoRoot, "ls-remote", "--exit-code", "--heads", remote, branchRef)
	if err != nil {
		return false
	}
	fields := strings.Fields(remoteHead)
	if len(fields) < 2 || fields[1] != branchRef {
		return false
	}
	head, err := gitRunner(ctx, repoRoot, "rev-parse", "HEAD")
	return err == nil && strings.TrimSpace(head) == fields[0]
}

var configureGitHubAPI = func(ctx context.Context, endpoint string) ([]byte, error) { //nolint:gochecknoglobals // test seam for GitHub protection APIs
	cmd := exec.CommandContext(ctx, "gh", "api", endpoint)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(exitErr.Stderr)), err)
	}
	return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
}

func configureBranchProtected(ctx context.Context, owner, repo, branch string) (bool, error) {
	base := "repos/" + owner + "/" + repo
	escapedBranch := url.PathEscape(branch)

	// GitHub's branch `.protected` flag is true for *any* active ruleset rule.
	// A non-blocking rule such as Copilot code review therefore made ordinary
	// feature branches look push-protected. Inspect effective rulesets first and
	// only treat rules that can reject a direct update as blocking.
	rules, rulesErr := configureGitHubAPI(ctx, base+"/rules/branches/"+escapedBranch)
	if rulesErr == nil {
		blocked, err := configureRulesBlockDirectPush(rules)
		if err != nil {
			rulesErr = err
		} else if blocked {
			return true, nil
		}
	}

	// Effective rulesets do not include classic branch-protection rules. A 200
	// from the classic endpoint means protection is configured; conservatively
	// use a review branch because its requirements may reject this user's push.
	_, classicErr := configureGitHubAPI(ctx, base+"/branches/"+escapedBranch+"/protection")
	if classicErr == nil {
		return true, nil
	}
	if !configureGitHubAPINotFound(classicErr) {
		return false, fmt.Errorf("check classic GitHub branch protection: %w", classicErr)
	}
	if rulesErr != nil {
		return false, fmt.Errorf("check GitHub branch rules: %w", rulesErr)
	}
	return false, nil
}

func configureGitHubAPINotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
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
		case "pull_request", "required_status_checks", "required_deployments", "required_signatures", "merge_queue", "lock_branch", "update", "workflows":
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
