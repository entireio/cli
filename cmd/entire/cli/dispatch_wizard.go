package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	searchpkg "github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/spf13/cobra"
)

var errDispatchCancelled = errors.New("dispatch cancelled")
var listDispatchWizardRepos = discoverAuthenticatedDispatchWizardRepos
var listDispatchWizardRepoResources = defaultListDispatchWizardRepoResources
var resolveDispatchWizardTopLevel = resolveGitTopLevel
var getDispatchWizardCurrentBranch = GetCurrentBranch
var runDispatchWizardForm = func(form *huh.Form) error { return form.Run() }

func defaultListDispatchWizardRepoResources(ctx context.Context) ([]api.Repository, error) {
	client, err := NewAuthenticatedAPIClient(ctx, false)
	if err != nil {
		return nil, err
	}
	repos, err := client.ListRepositories(ctx, api.RepositorySortRecent)
	if err != nil {
		return nil, fmt.Errorf("list dispatch repos: %w", err)
	}
	return repos, nil
}

const (
	dispatchWizardRepoDiscoveryConcurrencyLimit = 8

	dispatchWizardModeLocal  = "local"
	dispatchWizardModeServer = "server"

	dispatchWizardBranchCurrent = "current"
	dispatchWizardBranchAll     = "all"

	dispatchWizardVoiceCustom = "custom"

	// dispatchWizardJurisdictionHome is the jurisdiction select's value for
	// "send no selector" (the gateway then routes to the caller's home) and
	// the summary label for it. A sentinel rather than "" because huh
	// pre-selects the option matching the field's initial "" value.
	dispatchWizardJurisdictionHome = "home"
)

type dispatchWizardState struct {
	modeChoice       string
	timeWindowPreset string
	localBranchMode  string
	currentBranch    string
	currentBranchErr error
	selectedRepos    []string
	jurisdiction     string
	voicePreset      string
	voiceCustom      string
	confirmRun       bool
}

func newDispatchWizardState() dispatchWizardState {
	return dispatchWizardState{
		modeChoice:       dispatchWizardModeLocal,
		timeWindowPreset: "7d",
		localBranchMode:  dispatchWizardBranchCurrent,
		voicePreset:      "neutral",
		confirmRun:       true,
	}
}

func (s dispatchWizardState) isLocal() bool {
	return s.modeChoice != dispatchWizardModeServer
}

func (s dispatchWizardState) voiceValue() string {
	switch strings.TrimSpace(s.voicePreset) {
	case "marvin":
		return "marvin"
	case dispatchWizardVoiceCustom:
		if value := strings.TrimSpace(s.voiceCustom); value != "" {
			return value
		}
	}
	return "neutral"
}

func (s dispatchWizardState) showCustomVoiceInput() bool {
	return strings.TrimSpace(s.voicePreset) == dispatchWizardVoiceCustom
}

func (s dispatchWizardState) selectedReposList() []string {
	return normalizeDispatchWizardSelections(s.selectedRepos)
}

func (s dispatchWizardState) resolveCloudRepos() []string {
	if s.isLocal() {
		return nil
	}
	return s.selectedReposList()
}

func (s dispatchWizardState) showRepoPicker() bool {
	return !s.isLocal()
}

func (s dispatchWizardState) showLocalBranchMode() bool {
	return s.isLocal()
}

func (s dispatchWizardState) resolve() (dispatchpkg.Options, error) {
	allBranches := s.isLocal() && s.localBranchMode == dispatchWizardBranchAll
	jurisdiction := ""
	if !s.isLocal() && s.jurisdiction != dispatchWizardJurisdictionHome {
		jurisdiction = s.jurisdiction
	}
	if s.isLocal() && !allBranches && s.currentBranchErr != nil {
		return dispatchpkg.Options{}, fmt.Errorf("resolve current branch for local dispatch: %w", s.currentBranchErr)
	}
	opts, err := resolveDispatchOptions(
		s.isLocal(),
		s.timeWindowPreset,
		"",
		allBranches,
		s.resolveCloudRepos(),
		s.voiceValue(),
		jurisdiction,
		false,
		func() (string, error) {
			return s.currentBranch, nil
		},
	)
	if err != nil {
		return dispatchpkg.Options{}, err
	}
	return opts, nil
}

func (s dispatchWizardState) localBranchModeOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("Current branch", dispatchWizardBranchCurrent),
		huh.NewOption("All branches", dispatchWizardBranchAll),
	}
}

func buildDispatchWizardSummary(opts dispatchpkg.Options, scope string) string {
	if strings.TrimSpace(scope) == "" {
		scope = resolvedDispatchScope(opts)
	}

	var branches string
	switch {
	case opts.AllBranches:
		branches = "all local branches"
	case opts.Mode == dispatchpkg.ModeLocal:
		branches = "current branch"
	default:
		branches = "default branches"
	}

	lines := []string{"Mode: local", "Scope: " + scope, "Branches: " + branches}
	if opts.Mode != dispatchpkg.ModeLocal {
		lines[0] = "Mode: cloud"
		lines = append(lines, "Jurisdiction: "+cmp.Or(strings.TrimSpace(opts.Jurisdiction), dispatchWizardJurisdictionHome))
	}
	return strings.Join(lines, "\n")
}

func resolvedDispatchScope(opts dispatchpkg.Options) string {
	if len(opts.RepoPaths) > 0 {
		return "repos:" + strings.Join(opts.RepoPaths, ", ")
	}
	return "current repo"
}

func (s dispatchWizardState) previewScope(opts dispatchpkg.Options) string {
	if s.isLocal() {
		return resolvedDispatchScope(opts)
	}
	selectedRepos := s.selectedReposList()
	if len(selectedRepos) > 0 {
		return "repos:" + strings.Join(selectedRepos, ", ")
	}
	return resolvedDispatchScope(opts)
}

func buildDispatchCommand(opts dispatchpkg.Options) string {
	return strings.Join(compactStrings([]string{
		"entire dispatch",
		mapBoolToFlag(opts.Mode == dispatchpkg.ModeLocal, "--local"),
		renderStringFlag("--since", strings.TrimSpace(opts.Since)),
		mapBoolToFlag(opts.AllBranches, "--all-branches"),
		renderStringFlag("--repos", strings.Join(opts.RepoPaths, ",")),
		renderStringFlag("--jurisdiction", strings.TrimSpace(opts.Jurisdiction)),
		renderStringFlag("--voice", strings.TrimSpace(opts.Voice)),
	}), " ")
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func mapBoolToFlag(enabled bool, flag string) string {
	if enabled {
		return flag
	}
	return ""
}

func renderStringFlag(name string, value string) string {
	if value == "" {
		return ""
	}
	return name + " " + quoteShellValue(value)
}

func quoteShellValue(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, " ,:\t") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

func runDispatchWizard(cmd *cobra.Command) (dispatchpkg.Options, error) {
	ctx := cmd.Context()

	currentRepo, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return dispatchpkg.Options{}, fmt.Errorf("not in a git repository: %w", err)
	}

	state := newDispatchWizardState()
	state.currentBranch, state.currentBranchErr = getDispatchWizardCurrentBranch(ctx)

	// The cloud catalogue (see dispatchWizardScope) loads once, on first use
	// by the Jurisdiction select's OptionsFunc — which huh runs on a tea.Cmd
	// goroutine under its own spinner, so the form never blocks on it and a
	// local dispatch never pays for it.
	loadScope := sync.OnceValue(func() *dispatchWizardScope {
		return loadDispatchWizardScope(ctx, currentRepo)
	})
	jurisdiction := &dispatchJurisdictionAccessor{state: &state}

	validateRepos := func(value []string) error {
		// In local mode, the repo list is unused (and --repos is invalid), so don't require a selection.
 		if state.isLocal() {
 			return nil
 		}
		selected := normalizeDispatchWizardSelections(value)
		if len(selected) == 0 {
			return errors.New("select at least one repo")
		}
		if len(selected) > dispatchpkg.CloudRepoLimit {
			return fmt.Errorf("select at most %d repos", dispatchpkg.CloudRepoLimit)
		}
		return nil
	}
	repoPicker := huh.NewMultiSelect[string]().
		Title("Repos").
		Description(fmt.Sprintf("Press / to filter. Up to %d repos.", dispatchpkg.CloudRepoLimit)).
		Filterable(true).
		Value(&state.selectedRepos).
		Validate(validateRepos)

	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(
					huh.NewOption("Local", dispatchWizardModeLocal),
					huh.NewOption("Cloud", dispatchWizardModeServer),
				).
				Value(&state.modeChoice),
		).Title("Mode").Description("Choose where the dispatch should run."),
	}
	if IsAccessibleMode() {
		// huh's accessible runner ignores hide funcs and never evaluates an
		// OptionsFunc, so the jurisdiction picker cannot be dynamic there.
		// Route to the default jurisdiction (home when the caller has repos
		// there) and offer only the repos placed in it, so what is offered and
		// where the request is sent agree. Scoping to another jurisdiction is
		// `entire dispatch --jurisdiction <slug> --repos <slug>` (no wizard).
		scope := loadScope()
		state.jurisdiction = scope.defaultJurisdiction
		repoPicker.Options(buildDispatchRepoOptions(scope.reposIn(scope.defaultJurisdiction))...)
		groups = append(groups, huh.NewGroup(repoPicker))
	} else {
		groups = append(groups,
			huh.NewGroup(
				huh.NewSelect[string]().
					OptionsFunc(func() []huh.Option[string] { return loadScope().options() }, nil).
					Accessor(jurisdiction),
			).Title("Jurisdiction").Description("A dispatch covers repos placed in one jurisdiction and is generated there. Only repos placed in the selection are offered next.").
				WithHideFunc(func() bool {
					return !state.showRepoPicker()
				}),
			huh.NewGroup(
				// Re-evaluated when the jurisdiction changes; huh also narrows
				// the bound selection to the new options, so a repo picked
				// under another jurisdiction never rides along.
				repoPicker.OptionsFunc(func() []huh.Option[string] {
					return buildDispatchRepoOptions(loadScope().reposIn(jurisdiction.Snapshot()))
				}, &state.jurisdiction),
			).WithHideFunc(func() bool {
				return !state.showRepoPicker()
			}),
		)
	}
	groups = append(groups,
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(
					huh.NewOption("1 day", "1d"),
					huh.NewOption("7 days", "7d"),
					huh.NewOption("14 days", "14d"),
					huh.NewOption("30 days", "30d"),
				).
				Value(&state.timeWindowPreset),
		).Title("Window").Description("Choose the time window."),
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(state.localBranchModeOptions()...).
				Height(0).
				Value(&state.localBranchMode),
		).Title("Branch mode").Description("Choose how local dispatch should interpret branch scope.").
			WithHideFunc(func() bool {
				return !state.showLocalBranchMode()
			}),
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(
					huh.NewOption("Neutral", "neutral"),
					huh.NewOption("Marvin", "marvin"),
					huh.NewOption("Custom", dispatchWizardVoiceCustom),
				).
				Value(&state.voicePreset),
		).Title("Voice").Description("Choose a preset voice."),
		huh.NewGroup(
			huh.NewInput().
				Placeholder("Dry, skeptical release note narrator").
				Value(&state.voiceCustom).
				Validate(func(value string) error {
					if state.showCustomVoiceInput() && strings.TrimSpace(value) == "" {
						return errors.New("enter a custom voice")
					}
					return nil
				}),
		).Title("Custom voice").Description("Describe the dispatch voice.").
			WithHideFunc(func() bool {
				return !state.showCustomVoiceInput()
			}),
		huh.NewGroup(
			huh.NewNote().
				Title("Resolved options").
				DescriptionFunc(func() string {
					opts, resolveErr := state.resolve()
					if resolveErr != nil {
						return "Validation error: " + resolveErr.Error()
					}
					return buildDispatchWizardSummary(opts, state.previewScope(opts))
				}, &state),
			huh.NewNote().
				Title("Command").
				DescriptionFunc(func() string {
					opts, resolveErr := state.resolve()
					if resolveErr != nil {
						return "Validation error: " + resolveErr.Error()
					}
					return buildDispatchCommand(opts)
				}, &state),
			huh.NewConfirm().
				Title("Run dispatch?").
				Affirmative("Run").
				Negative("Cancel").
				Value(&state.confirmRun),
		).Title("Confirm").Description("Review the resolved command and run it."),
	)
	form := NewAccessibleForm(groups...)

	fmt.Fprintln(cmd.OutOrStdout())

	if err := runDispatchWizardForm(form); err != nil {
		if handled := handleFormCancellation(cmd.OutOrStdout(), "dispatch", err); handled == nil {
			return dispatchpkg.Options{}, errDispatchCancelled
		}
		return dispatchpkg.Options{}, fmt.Errorf("run dispatch wizard: %w", err)
	}
	if !state.confirmRun {
		fmt.Fprintln(cmd.OutOrStdout(), "dispatch cancelled.")
		return dispatchpkg.Options{}, errDispatchCancelled
	}

	return state.resolve()
}

// buildDispatchRepoOptions dedupes but preserves the caller's order so each
// source can pick its own order: the API path surfaces recent-first, and the
// local-discovery fallback surfaces the current repo first.
func buildDispatchRepoOptions(slugs []string) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(slugs))
	seen := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		if slug == "" {
			continue
		}
		if _, ok := seen[slug]; ok {
			continue
		}
		seen[slug] = struct{}{}
		options = append(options, huh.NewOption(slug, slug))
	}
	return options
}

func normalizeDispatchWizardSelections(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func discoverLocalRepoRoots(ctx context.Context, currentRepo string) []string {
	rootSet := map[string]struct{}{currentRepo: {}}
	parent := filepath.Dir(currentRepo)

	entries, err := os.ReadDir(parent)
	if err == nil {
		candidates := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(parent, entry.Name())
			if _, statErr := os.Stat(filepath.Join(candidate, ".git")); statErr != nil {
				continue
			}
			candidates = append(candidates, candidate)
		}

		resolved := make([]string, len(candidates))
		sem := make(chan struct{}, dispatchWizardRepoDiscoveryConcurrencyLimit)
		var wg sync.WaitGroup
		wg.Add(len(candidates))
		for i, candidate := range candidates {
			sem <- struct{}{}
			go func(i int, candidate string) {
				defer wg.Done()
				defer func() { <-sem }()
				if repoRoot, resolveErr := resolveDispatchWizardTopLevel(ctx, candidate); resolveErr == nil {
					resolved[i] = repoRoot
				}
			}(i, candidate)
		}
		wg.Wait()
		for _, repoRoot := range resolved {
			if repoRoot != "" {
				rootSet[repoRoot] = struct{}{}
			}
		}
	}

	repoRoots := make([]string, 0, len(rootSet))
	for repoRoot := range rootSet {
		repoRoots = append(repoRoots, repoRoot)
	}
	sort.Slice(repoRoots, func(i, j int) bool {
		if repoRoots[i] == currentRepo {
			return true
		}
		if repoRoots[j] == currentRepo {
			return false
		}
		return filepath.Base(repoRoots[i]) < filepath.Base(repoRoots[j])
	})
	return repoRoots
}

func discoverLocalRepoSlugs(ctx context.Context, currentRepo string) []string {
	repoRoots := discoverLocalRepoRoots(ctx, currentRepo)
	repoSlugs := make([]string, 0, len(repoRoots))
	seenRepoSlugs := make(map[string]struct{}, len(repoRoots))
	for _, repoRoot := range repoRoots {
		repoSlug := discoverRepoSlug(repoRoot)
		if repoSlug == "" {
			continue
		}
		if _, ok := seenRepoSlugs[repoSlug]; ok {
			continue
		}
		seenRepoSlugs[repoSlug] = struct{}{}
		repoSlugs = append(repoSlugs, repoSlug)
	}
	return repoSlugs
}

func resolveGitTopLevel(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// discoverAuthenticatedDispatchWizardRepos drops repos with zero checkpoints —
// dispatching them would produce nothing. Server order (recent-first) is
// preserved.
func discoverAuthenticatedDispatchWizardRepos(ctx context.Context) ([]string, error) {
	repos, err := listDispatchWizardRepoResources(ctx)
	if err != nil {
		logging.Warn(ctx, "dispatch wizard repo list failed", "error", err)
		return nil, err
	}

	slugs := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.CheckpointCount <= 0 {
			continue
		}
		slug := strings.TrimSpace(repo.FullName)
		if slug == "" {
			continue
		}
		slugs = append(slugs, slug)
	}
	return slugs, nil
}

func discoverRepoSlug(repoRoot string) string {
	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		return ""
	}
	defer repo.Close()

	remote, err := repo.Remote("origin")
	if err != nil || len(remote.Config().URLs) == 0 {
		return ""
	}
	owner, repoName, err := searchpkg.ParseGitHubRemote(remote.Config().URLs[0])
	if err != nil {
		return ""
	}
	return owner + "/" + repoName
}
