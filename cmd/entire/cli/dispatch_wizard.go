package cli

import (
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
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	searchpkg "github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"
)

var errDispatchCancelled = errors.New("dispatch cancelled")
var listDispatchWizardRepos = discoverAuthenticatedDispatchWizardRepos
var listDispatchWizardRepoResources = defaultListDispatchWizardRepoResources
var resolveDispatchWizardTopLevel = resolveGitTopLevel
var getDispatchWizardCurrentBranch = GetCurrentBranch
var runDispatchWizardForm = func(form *huh.Form) error { return form.Run() }

func defaultListDispatchWizardRepoResources(ctx context.Context) ([]api.Repository, error) {
	client, err := NewAuthenticatedAPIClient(false)
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
)

type dispatchWizardState struct {
	modeChoice       string
	timeWindowPreset string
	localBranchMode  string
	currentBranch    string
	currentBranchErr error
	selectedRepos    []string
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

	mode := "cloud"
	if opts.Mode == dispatchpkg.ModeLocal {
		mode = "local"
	}

	return strings.Join([]string{
		"Mode: " + mode,
		"Scope: " + scope,
		"Branches: " + branches,
	}, "\n")
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

	loadRepos := newLazyOptions(func() []huh.Option[string] {
		slugs, listErr := listDispatchWizardRepos(ctx)
		if listErr != nil || len(slugs) == 0 {
			slugs = discoverLocalRepoSlugs(ctx, currentRepo)
		}
		return buildDispatchRepoOptions(slugs)
	})

	state := newDispatchWizardState()
	state.currentBranch, state.currentBranchErr = getDispatchWizardCurrentBranch(ctx)

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(
					huh.NewOption("Local", dispatchWizardModeLocal),
					huh.NewOption("Cloud", dispatchWizardModeServer),
				).
				Value(&state.modeChoice),
		).Title("Mode").Description("Choose where the dispatch should run."),
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Repos").
				Description(fmt.Sprintf("Press / to filter. Up to %d repos.", dispatchpkg.CloudRepoLimit)).
				Filterable(true).
				OptionsFunc(loadRepos, nil).
				Value(&state.selectedRepos).
				Validate(func(value []string) error {
					selected := normalizeDispatchWizardSelections(value)
					if len(selected) == 0 {
						return errors.New("select at least one repo")
					}
					if len(selected) > dispatchpkg.CloudRepoLimit {
						return fmt.Errorf("select at most %d repos", dispatchpkg.CloudRepoLimit)
					}
					return nil
				}),
		).WithHideFunc(func() bool {
			return !state.showRepoPicker()
		}),
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

// newLazyOptions returns a func that runs loader once (under sync.Once) and
// returns the cached result on subsequent calls. Safe for concurrent use.
func newLazyOptions(loader func() []huh.Option[string]) func() []huh.Option[string] {
	var (
		once    sync.Once
		options []huh.Option[string]
	)
	return func() []huh.Option[string] {
		once.Do(func() {
			options = loader()
		})
		return options
	}
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
	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return ""
	}
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
