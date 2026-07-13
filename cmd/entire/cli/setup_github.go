package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Bootstrap modes for `entire enable --bootstrap`. They select what happens
// when `enable` runs in a directory that is not yet a Git repository. The
// model is safe-by-default: nothing user-visible (a GitHub repo) is created
// and no existing file is committed or pushed unless the user explicitly opts
// in (bootstrap=github for creation, --commit-existing-files for content).
const (
	// bootstrapModePrompt asks interactively (every confirmation defaults to
	// NO). It is the default. Non-interactively it declines unless --yes is
	// given, in which case it behaves like bootstrapModeLocal.
	bootstrapModePrompt = "prompt"
	// bootstrapModeLocal runs `git init` only; it never touches GitHub.
	bootstrapModeLocal = "local"
	// bootstrapModeGitHub runs `git init`, creates a GitHub repository, adds it
	// as origin, and pushes the initial commit.
	bootstrapModeGitHub = "github"
	// bootstrapModeOff exits without doing anything.
	bootstrapModeOff = "off"
)

// GitHubBootstrapOptions holds the flags that let `entire enable` run on a
// folder that isn't yet a git repository.
type GitHubBootstrapOptions struct {
	// Bootstrap selects behavior in a non-git directory: one of
	// prompt|local|github|off. The empty string is treated as prompt (the
	// documented default) and lets us tell "user passed --bootstrap=prompt"
	// apart from "flag was never set" for deprecated-flag conflict detection.
	Bootstrap string
	// CommitExistingFiles opts into staging, committing, and (with
	// Bootstrap=github) pushing the files already present in the directory.
	// Default false => an EMPTY initial commit; existing files are left
	// untracked, uncommitted, and unpushed. This is the core safety primitive.
	CommitExistingFiles bool
	// InitialCommitMessage overrides the default commit message. Requires
	// CommitExistingFiles (enforced by normalizeBootstrapOptions).
	InitialCommitMessage string
	// RepoName / RepoOwner / RepoVisibility configure the new GitHub repo and
	// are valid only with Bootstrap=github.
	RepoName       string
	RepoOwner      string
	RepoVisibility string
	// Yes accepts the selected mode's defaults without prompting. It is
	// scope-bounded: with the default prompt mode it resolves to a LOCAL init
	// (never GitHub), and it never commits existing files on its own — that
	// still requires --commit-existing-files.
	Yes bool

	// Deprecated aliases (pre-existing released flags) mapped onto the model
	// above by normalizeBootstrapOptions. Cobra prints the deprecation hint.
	InitRepo          bool // --init-repo            -> --bootstrap=local
	NoInitRepo        bool // --no-init-repo         -> --bootstrap=off
	NoGitHub          bool // --no-github            -> suppress the GitHub step
	SkipInitialCommit bool // --skip-initial-commit  -> no-op (matches new default)
}

// bootstrapRunner executes external commands. Tests override this to avoid
// shelling out to git/gh.
type bootstrapRunner interface {
	// Run executes the command and returns stdout. Stderr is available on
	// the returned *exec.ExitError for error reporting.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// RunInDir is Run with an explicit working directory.
	RunInDir(ctx context.Context, dir, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func (execRunner) RunInDir(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// printBootstrapSection writes a small section header so the bootstrap
// output has visual grouping between phases (git init → agent setup →
// commit & push). Kept simple text so it renders correctly in accessible
// mode and non-TTY captures.
func printBootstrapSection(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n", title)
}

// errBootstrapDeclined signals that the user chose not to initialize a
// repo. Returned _before_ `git init` runs; callers fall back to the
// legacy "Not a git repository" error.
var errBootstrapDeclined = errors.New("bootstrap declined")

// errBootstrapInterrupted signals that the user aborted a prompt _after_
// `git init` has already run. The local repo is in place but setup
// didn't complete; callers should surface that clearly instead of
// pretending no init happened.
var errBootstrapInterrupted = errors.New("bootstrap interrupted after init")

// ghRepoNameRe validates GitHub repository names. GitHub allows
// alphanumerics, hyphens, underscores, and periods — including as the
// first character (e.g. `.github`). We don't enforce a leading-char
// restriction here; `validateRepoName` handles the specific names GitHub
// reserves (`.`, `..`). Any other edge case is left to GitHub to reject
// so we don't over-restrict.
var ghRepoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// allowed visibility values.
const (
	visibilityPublic   = "public"
	visibilityPrivate  = "private"
	visibilityInternal = "internal"
)

// bootstrapState carries pre-setup decisions into the post-setup finalize
// step. The caller runs `runGitHubBootstrapInit` before agent setup to do
// `git init` + identity + gather GitHub choices, then runs
// `runGitHubBootstrapFinalize` afterwards so the initial commit captures
// the `.entire/`, `.claude/`, etc. files written during setup when the user
// opted into committing existing files.
type bootstrapState struct {
	runner      bootstrapRunner
	cwd         string
	useGitHub   bool
	fullName    string // owner/name, if useGitHub
	visibility  string // public/private/internal, if useGitHub
	commitFiles bool   // stage + commit (+ push) the files already in the dir
	message     string // resolved initial commit message
}

// githubResolution captures whether a GitHub repo is intended, and whether that
// still needs an interactive confirmation (deferred until after `git init` so a
// declined init makes no network call — issue #1717).
type githubResolution int

const (
	githubNo  githubResolution = iota // never create a GitHub repo
	githubYes                         // create a GitHub repo (explicit intent)
	githubAsk                         // ask the user (after init) whether to create one
)

// normalizeBootstrapOptions validates the flag combination and folds the
// deprecated aliases onto the --bootstrap/--commit-existing-files model. It
// mutates opts in place and must be called exactly once (it is not idempotent:
// the deprecated→mode mapping would trip the "don't combine" guard on a second
// pass). Cobra emits the per-flag deprecation hints via MarkDeprecated, so this
// only performs the mapping and the hard-error validation.
func normalizeBootstrapOptions(opts *GitHubBootstrapOptions) error {
	// A non-empty Bootstrap means the user passed --bootstrap explicitly.
	explicitBootstrap := opts.Bootstrap != ""

	switch opts.Bootstrap {
	case "", bootstrapModePrompt, bootstrapModeLocal, bootstrapModeGitHub, bootstrapModeOff:
	default:
		return fmt.Errorf("invalid --bootstrap %q: must be one of prompt, local, github, off", opts.Bootstrap)
	}

	// The deprecated mode flags map onto --bootstrap; combining them with an
	// explicit --bootstrap is ambiguous, so reject it.
	usesDeprecatedModeFlag := opts.InitRepo || opts.NoInitRepo || opts.NoGitHub
	if explicitBootstrap && usesDeprecatedModeFlag {
		return errors.New("--init-repo/--no-init-repo/--no-github are deprecated and cannot be combined with --bootstrap; use --bootstrap on its own")
	}

	// Map deprecated mode flags. --init-repo/--no-init-repo are mutually
	// exclusive (enforced by cobra). --no-github only suppresses the GitHub
	// step — on its own it does not opt into `git init`, matching its released
	// behavior (interactive prompt / non-interactive decline).
	switch {
	case opts.NoInitRepo:
		opts.Bootstrap = bootstrapModeOff
	case opts.InitRepo:
		opts.Bootstrap = bootstrapModeLocal
	}
	if opts.Bootstrap == "" {
		opts.Bootstrap = bootstrapModePrompt
	}

	// --repo-name/--repo-owner/--repo-visibility only make sense when a GitHub
	// repo will be created.
	if ghFlagsProvided(*opts) {
		if opts.NoGitHub {
			return errors.New("--repo-name/--repo-owner/--repo-visibility cannot be combined with --no-github (which skips GitHub)")
		}
		switch opts.Bootstrap {
		case bootstrapModeLocal, bootstrapModeOff:
			return fmt.Errorf("--repo-name/--repo-owner/--repo-visibility are only valid with --bootstrap=github, not --bootstrap=%s", opts.Bootstrap)
		}
	}

	// --skip-initial-commit is now the default (existing files are not
	// committed). Keep it working as a no-op, but reject the contradiction.
	if opts.SkipInitialCommit && opts.CommitExistingFiles {
		return errors.New("--skip-initial-commit cannot be combined with --commit-existing-files")
	}

	// A commit message only has meaning when we actually commit the existing
	// files. Fail loudly rather than silently ignoring the message or silently
	// committing content the user didn't opt into (issue #1717).
	if opts.InitialCommitMessage != "" && !opts.CommitExistingFiles {
		return errors.New("--initial-commit-message requires --commit-existing-files")
	}

	return nil
}

// runGitHubBootstrapInit handles the pre-setup half of "enable on a non-git
// folder": resolve the bootstrap mode, confirm + `git init`, ensure git
// identity, and (if we're going to create a GitHub repo) gather
// owner/name/visibility up front so all prompts happen before agent setup runs.
//
// Returns errBootstrapDeclined if the user declined (or chose --bootstrap=off).
func runGitHubBootstrapInit(ctx context.Context, w, errW io.Writer, opts GitHubBootstrapOptions) (*bootstrapState, error) {
	return runGitHubBootstrapInitWith(ctx, w, errW, opts, execRunner{})
}

// runGitHubBootstrapInitWith is the testable variant that accepts a runner.
func runGitHubBootstrapInitWith(ctx context.Context, w, errW io.Writer, opts GitHubBootstrapOptions, runner bootstrapRunner) (*bootstrapState, error) {
	// paths.RepoRoot is unavailable here — we're bootstrapping _before_ a
	// repo exists. Plain cwd is the correct target for `git init`.
	cwd, err := os.Getwd() //nolint:forbidigo // no repo yet; git init runs in cwd
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	// Validate + fold deprecated aliases before any side effect. A bad flag
	// combination must fail before `git init`, a network call, or a prompt.
	if err := normalizeBootstrapOptions(&opts); err != nil {
		return nil, err
	}

	// --bootstrap=off exits immediately: no directory read, no prompt, no
	// network. setup.go turns errBootstrapDeclined into the user-facing guidance.
	if opts.Bootstrap == bootstrapModeOff {
		return nil, errBootstrapDeclined
	}

	canPrompt := interactive.CanPromptInteractively()

	// Decide whether to run `git init` and whether a GitHub repo is intended.
	// No side effect and no network call happen inside this step; the gh probe
	// is deliberately deferred until after init consent below (issue #1717).
	proceed, ghRes, err := resolveBootstrapDecision(w, cwd, opts, canPrompt)
	if err != nil {
		return nil, err
	}
	if !proceed {
		return nil, errBootstrapDeclined
	}

	// git init — the first side effect, reached only after an explicit mode
	// opt-in, an explicit --yes, or a default-NO confirmation that the user
	// actively accepted.
	printBootstrapSection(w, "Setting up git repository")
	if err := gitInit(ctx, runner, cwd); err != nil {
		return nil, fmt.Errorf("git init: %w", err)
	}
	// Clear cached worktree root so subsequent paths.WorktreeRoot calls pick
	// up the freshly created repo.
	paths.ClearWorktreeRootCache()
	fmt.Fprintln(w, "  ✓ Initialized empty git repository")

	// Only NOW (after init consent) may we probe gh — `gh auth status` is a
	// network round-trip and must never run on a declined/local/off path
	// (issue #1717). gh is only consulted when a GitHub repo is intended.
	useGitHub := ghRes != githubNo
	if useGitHub {
		if !ghAvailable(ctx, runner) {
			fmt.Fprintln(errW, "gh CLI not found. Install it from https://cli.github.com/ and run `gh auth login` to add a GitHub remote.")
			fmt.Fprintln(errW, "Continuing with local initialization only.")
			useGitHub = false
		} else if !ghAuthenticated(ctx, runner) {
			fmt.Fprintln(errW, "gh CLI is not authenticated. Run `gh auth login` to add a GitHub remote.")
			fmt.Fprintln(errW, "Continuing with local initialization only.")
			useGitHub = false
		}
	}

	// For the interactive prompt path, confirm the GitHub step now (after init,
	// so an abort here is "interrupted after init"). Explicit github mode and
	// the --repo-* flags carry their own intent and skip this confirm.
	if useGitHub && ghRes == githubAsk {
		confirmed, err := confirmCreateGitHubRepo()
		if err != nil {
			return nil, err
		}
		if !confirmed {
			useGitHub = false
		}
	}

	// Gather GitHub repo details up front so all prompts are contiguous.
	var fullName, visibility string
	if useGitHub {
		owner, name, vis, err := selectGitHubRepo(ctx, w, errW, runner, cwd, opts)
		if err != nil {
			return nil, err
		}
		fullName = owner + "/" + name
		visibility = vis
	}

	// Resolve the commit message and ensure git identity. We always create at
	// least an empty initial commit, so identity is always required. Source it
	// from gh only when a GitHub repo is in play — a local init must make zero
	// gh calls (issue #1717 verification matrix).
	message := resolveInitialCommitMessage(opts)
	if err := ensureGitIdentity(ctx, w, errW, runner, cwd, useGitHub); err != nil {
		return nil, err
	}

	return &bootstrapState{
		runner:      runner,
		cwd:         cwd,
		useGitHub:   useGitHub,
		fullName:    fullName,
		visibility:  visibility,
		commitFiles: opts.CommitExistingFiles,
		message:     message,
	}, nil
}

// resolveBootstrapDecision returns whether to run `git init` and whether a
// GitHub repo is intended. It performs no side effect and no network call: the
// only interaction is the (default-NO) init confirmation in the interactive
// prompt path, so declining costs nothing and touches no gh (issue #1717).
func resolveBootstrapDecision(w io.Writer, cwd string, opts GitHubBootstrapOptions, canPrompt bool) (proceed bool, gh githubResolution, err error) {
	switch opts.Bootstrap {
	case bootstrapModeLocal:
		return true, githubNo, nil
	case bootstrapModeGitHub:
		return true, githubYes, nil
	default: // prompt
		// --yes is scope-bounded: on its own it accepts the safe default (a
		// local init, no GitHub). It only creates a GitHub repo when the user
		// also gave explicit GitHub intent via a --repo-* flag; --yes alone
		// never touches GitHub (issue #1717 keeper #4).
		yesResolution := githubNo
		if ghFlagsProvided(opts) {
			yesResolution = githubYes
		}
		// Non-interactive prompt: never create anything silently. Only an
		// explicit --yes proceeds.
		if !canPrompt {
			if opts.Yes {
				return true, yesResolution, nil
			}
			return false, githubNo, nil
		}
		// Interactive --yes skips the prompts and accepts the resolved defaults.
		if opts.Yes {
			return true, yesResolution, nil
		}
		// Interactive: show what will happen, then confirm (default NO).
		printBootstrapPlan(w, cwd, opts, !opts.NoGitHub)
		ok, err := confirmInitRepo()
		if err != nil {
			return false, githubNo, err
		}
		if !ok {
			return false, githubNo, nil
		}
		switch {
		case opts.NoGitHub:
			return true, githubNo, nil
		case ghFlagsProvided(opts):
			// Explicit repo details imply GitHub intent; no extra confirm.
			return true, githubYes, nil
		default:
			return true, githubAsk, nil
		}
	}
}

// runGitHubBootstrapWith runs the full bootstrap (init + finalize) in one
// call, used by tests that don't need to assert phasing. The real caller
// runs the two phases around agent setup.
func runGitHubBootstrapWith(ctx context.Context, w, errW io.Writer, opts GitHubBootstrapOptions, runner bootstrapRunner) error {
	state, err := runGitHubBootstrapInitWith(ctx, w, errW, opts, runner)
	if err != nil {
		return err
	}
	return runGitHubBootstrapFinalize(ctx, w, state)
}

// runGitHubBootstrapFinalize runs the post-setup half: create the initial
// commit, then (if requested) create the GitHub repo and push. The initial
// commit is EMPTY by default — existing files are staged only when the user
// opted in with --commit-existing-files — so nothing user-authored is ever
// committed or published without consent (issue #1717).
func runGitHubBootstrapFinalize(ctx context.Context, w io.Writer, s *bootstrapState) error {
	if s == nil {
		return nil
	}

	switch {
	case s.useGitHub && s.commitFiles:
		printBootstrapSection(w, "Publishing to GitHub")
	case s.useGitHub:
		printBootstrapSection(w, "Creating GitHub repository")
	default:
		printBootstrapSection(w, "Finalizing")
	}

	if err := doInitialCommit(ctx, s.runner, s.cwd, s.message, s.commitFiles); err != nil {
		return fmt.Errorf("initial commit: %w", err)
	}
	if s.commitFiles {
		fmt.Fprintln(w, "  ✓ Created initial commit from the existing files")
	} else {
		fmt.Fprintln(w, "  ✓ Created an empty initial commit")
	}

	if s.useGitHub {
		if err := ghRepoCreate(ctx, s.runner, s.cwd, s.fullName, s.visibility); err != nil {
			return fmt.Errorf("gh repo create: %w", err)
		}
		fmt.Fprintf(w, "  ✓ Created %s (%s)\n", s.fullName, s.visibility)
		fmt.Fprintf(w, "    https://github.com/%s\n", s.fullName)
		fmt.Fprintln(w, "  ✓ Pushed the initial commit to origin")
	}

	if !s.commitFiles {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Your existing files were left untracked. To commit them:")
		fmt.Fprintln(w, "    git add -A && git commit -m \"Add existing files\"")
		if s.useGitHub {
			fmt.Fprintln(w, "    git push")
		}
		fmt.Fprintln(w, "  Or re-run `entire enable --commit-existing-files` to commit and push them.")
	}

	fmt.Fprintln(w, "\nDone.")
	return nil
}

// ghFlagsProvided reports whether the caller has already expressed intent
// to create a GitHub repo via any of the gh-specific flags. Used to skip
// the "create on GitHub?" confirm prompt in that case.
func ghFlagsProvided(opts GitHubBootstrapOptions) bool {
	return opts.RepoName != "" || opts.RepoOwner != "" || opts.RepoVisibility != ""
}

// confirmInitRepo asks whether to `git init` in the current directory. It
// defaults to NO so a bare Enter / EOF never proceeds (issue #1717).
func confirmInitRepo() (bool, error) {
	confirmed := false // default NO — never proceed on a bare Enter / EOF.
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Initialize a Git repository here and continue?").
				Value(&confirmed),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, fmt.Errorf("init-repo prompt: %w", err)
	}
	return confirmed, nil
}

// confirmCreateGitHubRepo asks whether to also create a matching GitHub
// repository. Interactive-only; runs after `git init`, so an abort is an
// "interrupted after init". Defaults to NO: creating and pushing to a GitHub
// repository is externally visible, so a bare Enter / EOF must never publish
// the directory (issue #1717).
func confirmCreateGitHubRepo() (bool, error) {
	confirmed := false
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Also create a new repository on GitHub and push to it?").
				Description("This publishes the committed files to a new remote repository.").
				Value(&confirmed),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, errBootstrapInterrupted
		}
		return false, fmt.Errorf("github confirm prompt: %w", err)
	}
	return confirmed, nil
}

// plannedRepoVisibility reports the visibility the GitHub repository will be
// created with, and whether that choice is already locked in. It mirrors
// resolveVisibility's non-interactive decision: an explicit --repo-visibility
// wins, --yes locks the private default, otherwise the default is private but
// the user will still be prompted (interactive). The plan is a security
// confirmation surface, so it must state the actual resolved visibility, not a
// hardcoded default (issue #1717).
func plannedRepoVisibility(opts GitHubBootstrapOptions) (visibility string, locked bool) {
	if opts.RepoVisibility != "" {
		return strings.ToLower(opts.RepoVisibility), true
	}
	if opts.Yes {
		return visibilityPrivate, true
	}
	return visibilityPrivate, false
}

// printBootstrapPlan writes a short summary of what enabling Entire in a
// non-git directory will do, so the interactive confirmation is informed. It
// deliberately does not walk or size the directory — under safe-by-default the
// initial commit is empty unless the user opts in, so there is nothing to
// tally.
func printBootstrapPlan(w io.Writer, cwd string, opts GitHubBootstrapOptions, githubPlanned bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The current directory is not a Git repository:")
	fmt.Fprintf(w, "  %s\n", cwd)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Enabling Entire here will:")
	fmt.Fprintln(w, "  • Initialize a new Git repository (git init)")
	if opts.CommitExistingFiles {
		fmt.Fprintln(w, "  • Stage and commit the files already in this directory")
	} else {
		fmt.Fprintln(w, "  • Create an empty initial commit (your existing files are left untracked)")
	}
	if githubPlanned {
		visibility, _ := plannedRepoVisibility(opts)
		fmt.Fprintf(w, "  • Create a new %s repository on GitHub and push the initial commit to it\n", strings.ToUpper(visibility))
	}
	fmt.Fprintln(w)
}

// selectGitHubRepo gathers owner, repo name, and visibility, respecting
// supplied flags and falling back to interactive prompts.
func selectGitHubRepo(ctx context.Context, w, errW io.Writer, runner bootstrapRunner, cwd string, opts GitHubBootstrapOptions) (owner, name, visibility string, err error) {
	currentUser, userErr := ghCurrentUser(ctx, runner)
	if userErr != nil {
		return "", "", "", fmt.Errorf("query current gh user: %w", userErr)
	}
	orgs, orgErr := ghListOrgs(ctx, runner)
	if orgErr != nil {
		// Missing read:org scope is non-fatal — we can still offer the user
		// account. Warn and continue.
		fmt.Fprintf(errW, "Warning: could not list organizations (%v). Only your user account is available.\n", orgErr)
		orgs = nil
	}

	owner, err = resolveOwner(w, currentUser, orgs, opts)
	if err != nil {
		return "", "", "", err
	}

	name, err = resolveRepoName(ctx, w, errW, runner, owner, cwd, opts)
	if err != nil {
		return "", "", "", err
	}

	visibility, err = resolveVisibility(owner, currentUser, opts)
	if err != nil {
		return "", "", "", err
	}

	return owner, name, visibility, nil
}

func resolveOwner(w io.Writer, currentUser string, orgs []string, opts GitHubBootstrapOptions) (string, error) {
	owners := append([]string{currentUser}, orgs...)
	if opts.RepoOwner != "" {
		for _, candidate := range owners {
			if candidate == opts.RepoOwner {
				return opts.RepoOwner, nil
			}
		}
		// Owner not in known list — allow it anyway; gh repo create will
		// error out later if invalid. This supports orgs the token can't
		// enumerate (e.g. missing read:org scope).
		return opts.RepoOwner, nil
	}
	if len(owners) == 1 || opts.Yes {
		fmt.Fprintf(w, "  Using GitHub owner: %s\n", currentUser)
		return currentUser, nil
	}
	if !interactive.CanPromptInteractively() {
		return currentUser, nil
	}

	options := make([]huh.Option[string], 0, len(owners))
	for _, o := range owners {
		options = append(options, huh.NewOption(o, o))
	}
	selected := currentUser
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose the GitHub owner for the new repository").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", errBootstrapInterrupted
		}
		return "", fmt.Errorf("owner prompt: %w", err)
	}
	return selected, nil
}

func resolveRepoName(ctx context.Context, w, errW io.Writer, runner bootstrapRunner, owner, cwd string, opts GitHubBootstrapOptions) (string, error) {
	suggested := slugifyRepoName(filepath.Base(cwd))

	if opts.RepoName != "" {
		if err := validateRepoName(opts.RepoName); err != nil {
			return "", err
		}
		exists, checkErr := ghRepoExists(ctx, runner, owner, opts.RepoName)
		if checkErr != nil {
			fmt.Fprintf(errW, "Warning: could not check if %s/%s already exists (%v).\n", owner, opts.RepoName, checkErr)
		} else if exists {
			return "", fmt.Errorf("repository %s/%s already exists on GitHub", owner, opts.RepoName)
		}
		return opts.RepoName, nil
	}

	if opts.Yes {
		// Check availability before blindly using the suggested name.
		exists, checkErr := ghRepoExists(ctx, runner, owner, suggested)
		if checkErr != nil {
			// Check failed — proceed with the suggested name and let gh
			// error later if the name is actually taken.
			fmt.Fprintf(errW, "Warning: could not check if %s/%s already exists (%v).\n", owner, suggested, checkErr)
			return suggested, nil
		}
		if !exists {
			return suggested, nil
		}
		// Name taken. If a TTY is available, fall back to the interactive
		// prompt so the user can pick a different name instead of failing.
		if interactive.CanPromptInteractively() {
			fmt.Fprintf(w, "  %s/%s already exists on GitHub.\n", owner, suggested)
		} else {
			return "", fmt.Errorf("repository %s/%s already exists on GitHub (use --repo-name to specify a different name)", owner, suggested)
		}
	}
	if !interactive.CanPromptInteractively() {
		return suggested, nil
	}

	name := suggested
	for {
		var input string
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Repository name").
					Description(fmt.Sprintf("Press enter to use %q", name)).
					Value(&input),
			),
		).WithOutput(w)
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return "", errBootstrapInterrupted
			}
			return "", fmt.Errorf("repo name prompt: %w", err)
		}
		if strings.TrimSpace(input) != "" {
			name = strings.TrimSpace(input)
		}
		if err := validateRepoName(name); err != nil {
			fmt.Fprintf(errW, "Invalid name: %v\n", err)
			continue
		}
		exists, checkErr := ghRepoExists(ctx, runner, owner, name)
		if checkErr != nil {
			fmt.Fprintf(errW, "Warning: could not check if %s/%s already exists (%v). Proceeding; gh will error out if it is taken.\n", owner, name, checkErr)
			return name, nil
		}
		if exists {
			fmt.Fprintf(w, "%s/%s already exists on GitHub. Pick a different name.\n", owner, name)
			continue
		}
		return name, nil
	}
}

func resolveVisibility(owner, currentUser string, opts GitHubBootstrapOptions) (string, error) {
	isOrg := owner != currentUser

	if opts.RepoVisibility != "" {
		vis := strings.ToLower(opts.RepoVisibility)
		switch vis {
		case visibilityPublic, visibilityPrivate:
			return vis, nil
		case visibilityInternal:
			if !isOrg {
				return "", errors.New("visibility 'internal' is only available for organization repositories")
			}
			return vis, nil
		default:
			return "", fmt.Errorf("invalid visibility %q: must be one of public, private, internal", opts.RepoVisibility)
		}
	}
	if opts.Yes || !interactive.CanPromptInteractively() {
		return visibilityPrivate, nil
	}

	options := []huh.Option[string]{
		huh.NewOption("Private", visibilityPrivate),
		huh.NewOption("Public", visibilityPublic),
	}
	if isOrg {
		options = append(options, huh.NewOption("Internal", visibilityInternal))
	}
	selected := visibilityPrivate
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Repository visibility").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", errBootstrapInterrupted
		}
		return "", fmt.Errorf("visibility prompt: %w", err)
	}
	return selected, nil
}

// resolveInitialCommitMessage returns the message for the initial commit.
// normalizeBootstrapOptions guarantees a non-empty InitialCommitMessage
// implies CommitExistingFiles, so this is a pure default lookup.
func resolveInitialCommitMessage(opts GitHubBootstrapOptions) string {
	if opts.InitialCommitMessage != "" {
		return opts.InitialCommitMessage
	}
	return "Initial commit"
}

// gitInit runs `git init` in the given directory.
func gitInit(ctx context.Context, runner bootstrapRunner, dir string) error {
	if _, err := runner.RunInDir(ctx, dir, "git", "init"); err != nil {
		return fmt.Errorf("run git init: %w", err)
	}
	return nil
}

// doInitialCommit creates the initial commit. When commitExisting is true it
// stages every file first; otherwise it stages nothing and records an empty
// commit — the safe default that leaves existing files untracked (issue #1717).
// `--allow-empty` lets a fresh directory (or the safe default) still anchor the
// branch and give a GitHub push a HEAD to send.
func doInitialCommit(ctx context.Context, runner bootstrapRunner, dir, message string, commitExisting bool) error {
	if commitExisting {
		if _, err := runner.RunInDir(ctx, dir, "git", "add", "-A"); err != nil {
			return wrapExecError("git add", err)
		}
	}
	// Disable GPG signing for this commit only. Fresh environments often
	// have commit.gpgsign=true inherited from a global config but no
	// working signer; passing -c keeps the user's global config intact.
	if _, err := runner.RunInDir(ctx, dir, "git", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", message); err != nil {
		return wrapExecError("git commit", err)
	}
	return nil
}

// wrapExecError formats err with stderr from *exec.ExitError when available,
// so callers see git's actual complaint instead of an opaque "exit status N".
func wrapExecError(prefix string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if stderr := strings.TrimSpace(string(ee.Stderr)); stderr != "" {
			return fmt.Errorf("%s: %w: %s", prefix, err, stderr)
		}
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// ensureGitIdentity guarantees the repo has a user.name/user.email set at
// some scope. If neither is configured, we source values from `gh api user`
// when allowGH is set (a GitHub repo is being created, so gh is already in
// play), otherwise prompt (interactive) or fail with a helpful message
// (non-interactive). A local-only init passes allowGH=false so it makes zero
// gh calls. Values are written to the local repo config only, so the user's
// global state is never mutated.
func ensureGitIdentity(ctx context.Context, w, _ io.Writer, runner bootstrapRunner, dir string, allowGH bool) error {
	// `git config --get` exits non-zero when the key isn't set. Treat any
	// error as "unset" rather than fatal so we can fall through to sourcing
	// the identity from elsewhere.
	nameOut, nameErr := runner.RunInDir(ctx, dir, "git", "config", "--get", "user.name")
	emailOut, emailErr := runner.RunInDir(ctx, dir, "git", "config", "--get", "user.email")
	var existingName, existingEmail string
	if nameErr == nil {
		existingName = strings.TrimSpace(nameOut)
	}
	if emailErr == nil {
		existingEmail = strings.TrimSpace(emailOut)
	}
	if existingName != "" && existingEmail != "" {
		return nil
	}

	// Only try to fill in what's missing. If the user has a name set
	// globally but no email, we want to keep their name and just source
	// the email.
	var ghName, ghEmail string
	if allowGH && ghAvailable(ctx, runner) && ghAuthenticated(ctx, runner) {
		if n, e, err := ghUserIdentity(ctx, runner); err == nil {
			ghName, ghEmail = n, e
		}
	}

	name, email, err := resolveGitIdentity(w, existingName, existingEmail, ghName, ghEmail)
	if err != nil {
		return err
	}

	// Write only the fields that were missing. Leaving the already-set
	// field alone means we never silently replace the user's globally
	// configured name/email.
	if existingName == "" {
		if _, err := runner.RunInDir(ctx, dir, "git", "config", "user.name", name); err != nil {
			return fmt.Errorf("git config user.name: %w", err)
		}
	}
	if existingEmail == "" {
		if _, err := runner.RunInDir(ctx, dir, "git", "config", "user.email", email); err != nil {
			return fmt.Errorf("git config user.email: %w", err)
		}
	}
	return nil
}

// resolveGitIdentity returns the name/email to use, given any values
// already configured at a wider scope and any values from `gh api user`.
// Only prompts for fields that are still empty after those fallbacks.
func resolveGitIdentity(w io.Writer, existingName, existingEmail, ghName, ghEmail string) (string, string, error) {
	name := existingName
	email := existingEmail
	if name == "" {
		name = ghName
	}
	if email == "" {
		email = ghEmail
	}

	if name != "" && email != "" {
		// Announce only when we had to fill something in from gh —
		// silence is fine when the user's existing config covered both.
		if (existingName == "" && ghName != "") || (existingEmail == "" && ghEmail != "") {
			fmt.Fprintf(w, "  Using git identity: %s <%s>\n", name, email)
		}
		return name, email, nil
	}

	if !interactive.CanPromptInteractively() {
		return "", "", errors.New(`git identity not configured. Set it with:
  git config --global user.name "Your Name"
  git config --global user.email "you@example.com"`)
	}

	// Prompt only for the still-missing fields.
	var fields []huh.Field
	if name == "" {
		fields = append(fields, huh.NewInput().Title("Git user.name").Value(&name))
	}
	if email == "" {
		fields = append(fields, huh.NewInput().Title("Git user.email").Value(&email))
	}
	form := NewAccessibleForm(huh.NewGroup(fields...))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", "", errBootstrapInterrupted
		}
		return "", "", fmt.Errorf("git identity prompt: %w", err)
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		return "", "", errors.New("git user.name and user.email are both required")
	}
	return strings.TrimSpace(name), strings.TrimSpace(email), nil
}

// ghUserResponse is the subset of `gh api user` fields we care about.
type ghUserResponse struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ghUserIdentity returns a best-effort (name, email) from `gh api user`.
// Missing name falls back to login; missing email falls back to the GitHub
// no-reply address, which is always accepted by GitHub.
func ghUserIdentity(ctx context.Context, runner bootstrapRunner) (string, string, error) {
	out, err := runner.Run(ctx, "gh", "api", "user")
	if err != nil {
		return "", "", fmt.Errorf("gh api user: %w", err)
	}
	var resp ghUserResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return "", "", fmt.Errorf("parse gh user response: %w", err)
	}
	name := resp.Name
	if name == "" {
		name = resp.Login
	}
	email := resp.Email
	if email == "" && resp.ID != 0 && resp.Login != "" {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", resp.ID, resp.Login)
	}
	if name == "" || email == "" {
		return "", "", errors.New("gh user response missing identity fields")
	}
	return name, email, nil
}

// ghAvailable reports whether the gh CLI is installed.
func ghAvailable(ctx context.Context, runner bootstrapRunner) bool {
	_, err := runner.Run(ctx, "gh", "--version")
	return err == nil
}

// ghAuthenticated reports whether `gh auth status` succeeds.
func ghAuthenticated(ctx context.Context, runner bootstrapRunner) bool {
	_, err := runner.Run(ctx, "gh", "auth", "status")
	return err == nil
}

// ghCurrentUser returns the authenticated GitHub user's login.
func ghCurrentUser(ctx context.Context, runner bootstrapRunner) (string, error) {
	out, err := runner.Run(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ghListOrgs returns the orgs the authenticated user belongs to, sorted
// alphabetically. Requires the `read:org` token scope.
func ghListOrgs(ctx context.Context, runner bootstrapRunner) ([]string, error) {
	out, err := runner.Run(ctx, "gh", "api", "user/orgs", "--jq", ".[].login")
	if err != nil {
		return nil, fmt.Errorf("gh api user/orgs: %w", err)
	}
	var orgs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			orgs = append(orgs, line)
		}
	}
	sort.Strings(orgs)
	return orgs, nil
}

// ghRepoExists checks whether <owner>/<name> exists on GitHub.
func ghRepoExists(ctx context.Context, runner bootstrapRunner, owner, name string) (bool, error) {
	_, err := runner.Run(ctx, "gh", "repo", "view", owner+"/"+name, "--json", "name")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := string(exitErr.Stderr)
		if strings.Contains(msg, "Could not resolve") || strings.Contains(msg, "not found") || strings.Contains(msg, "Not Found") {
			return false, nil
		}
	}
	return false, fmt.Errorf("gh repo view: %w", err)
}

// ghRepoCreate creates a GitHub repo from the local source directory, adds
// origin as its remote, and pushes the initial commit.
func ghRepoCreate(ctx context.Context, runner bootstrapRunner, dir, fullName, visibility string) error {
	// Create the remote repo and add origin, but don't push yet. We push
	// separately below with --no-verify so the pre-push hook doesn't run
	// on this first push: the entire/checkpoints/v1 branch has nothing to
	// checkpoint (no sessions yet), and if it's pushed alongside the
	// default branch GitHub can pick it as the default.
	//
	// Capture `gh repo create`'s stdout instead of streaming it — its own
	// "✓ Created repository..." / "✓ Added remote..." lines would
	// duplicate our own summary in runGitHubBootstrapFinalize.
	args := []string{
		"repo", "create", fullName,
		"--" + visibility,
		"--source=.",
		"--remote=origin",
	}
	if _, err := runner.RunInDir(ctx, dir, "gh", args...); err != nil {
		return fmt.Errorf("gh repo create: %w", ghRunnerErr(err))
	}
	// -q silences "Enumerating objects..." etc. --no-verify bypasses the
	// pre-push hook so entire/checkpoints/v1 isn't pushed alongside the
	// default branch. We always have at least an empty initial commit to push.
	if _, err := runner.RunInDir(ctx, dir, "git", "push", "-q", "--no-verify", "-u", "origin", "HEAD"); err != nil {
		return fmt.Errorf("git push: %w", ghRunnerErr(err))
	}
	return nil
}

// ghRunnerErr extracts an exec.ExitError's stderr into the returned
// error so the user sees a useful diagnostic when gh/git fail under a
// captured-stdout call.
func ghRunnerErr(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err
}

// slugifyRepoName turns a folder name into a GitHub-safe repo name. Invalid
// characters are replaced with '-', and runs of '-' are collapsed.
func slugifyRepoName(folder string) string {
	var b strings.Builder
	b.Grow(len(folder))
	for _, r := range folder {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	slug := b.String()
	// Collapse repeated dashes.
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-.")
	if slug == "" {
		slug = "my-repo"
	}
	return slug
}

// validateRepoName checks whether name is a valid GitHub repo name.
func validateRepoName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > 100 {
		return errors.New("name must be at most 100 characters")
	}
	if strings.Contains(name, "/") {
		return errors.New("name must not contain '/' (pass --repo-owner separately)")
	}
	if name == "." || name == ".." {
		return errors.New("name cannot be '.' or '..'")
	}
	if !ghRepoNameRe.MatchString(name) {
		return errors.New("name may only contain letters, digits, '.', '-', '_'")
	}
	return nil
}
