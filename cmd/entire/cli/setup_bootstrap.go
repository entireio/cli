package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// BootstrapOptions holds flags that let `entire enable` run on a folder
// that isn't yet a git repository. All fields are optional; supplying one
// skips the matching interactive prompt.
//
// Bootstrap is deliberately local-only: it runs `git init` and (optionally)
// an initial commit, and never creates or pushes to a remote. Publishing a
// directory is the user's call to make with their own forge tooling (`gh
// repo create`, `entire repo create`, a web UI), not a side effect of
// enabling Entire.
type BootstrapOptions struct {
	// InitRepo is true if --init-repo was passed (accept git init without prompt).
	InitRepo bool
	// NoInitRepo is true if --no-init-repo was passed (decline without prompt).
	NoInitRepo bool
	// InitialCommitMessage overrides the default commit message prompt.
	InitialCommitMessage string
	// SkipInitialCommit leaves the newly-created files unstaged so the
	// user can commit themselves.
	SkipInitialCommit bool
	// Yes accepts all defaults without prompting: init repo and commit with
	// the default message.
	Yes bool
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
// initial commit). Kept simple text so it renders correctly in accessible
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

const defaultInitialCommitMessage = "Initial commit"

type bootstrapSetupChoice string

const (
	bootstrapSetupLocal   bootstrapSetupChoice = "local"
	bootstrapSetupCustom  bootstrapSetupChoice = "custom"
	bootstrapSetupDecline bootstrapSetupChoice = "decline"
)

// bootstrapState carries pre-setup decisions into the post-setup finalize
// step. The caller runs `runBootstrapInit` before agent setup to do
// `git init` + identity + the initial-commit decision, then runs
// `runBootstrapFinalize` afterwards so the initial commit captures
// the `.entire/`, `.claude/`, etc. files written during setup.
type bootstrapState struct {
	runner  bootstrapRunner
	cwd     string
	commit  bool   // false means the user opted out of the initial commit
	message string // resolved initial commit message (empty when !commit)
}

// runBootstrapInit handles the pre-setup half of "enable on a non-git
// folder": confirm + `git init`, ensure a git identity, and resolve the
// initial-commit decision up front so all prompts happen before agent setup
// runs. No remote is created or contacted.
//
// Returns errBootstrapDeclined if the user declined the init prompt.
func runBootstrapInit(ctx context.Context, w io.Writer, opts BootstrapOptions) (*bootstrapState, error) {
	return runBootstrapInitWith(ctx, w, opts, execRunner{})
}

// runBootstrapInitWith is the testable variant that accepts a runner.
func runBootstrapInitWith(ctx context.Context, w io.Writer, opts BootstrapOptions, runner bootstrapRunner) (*bootstrapState, error) {
	// paths.RepoRoot is unavailable here — we're bootstrapping _before_ a
	// repo exists. Plain cwd is the correct target for `git init`.
	cwd, err := os.Getwd() //nolint:forbidigo // no repo yet; git init runs in cwd
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	// Step 1: decide whether to init here — and, on the bare interactive
	// path, how: one select carries both the init consent and the setup
	// preset, so the common flow costs a single answer. Explicit flags,
	// --yes, and non-interactive runs keep the granular confirm + resolver
	// contracts unchanged.
	setupChoice := bootstrapSetupCustom
	if shouldPromptBootstrapSetupChoice(opts) {
		setupChoice, err = promptBootstrapSetupChoice(w, cwd)
		if err != nil {
			return nil, err
		}
		if setupChoice == bootstrapSetupDecline {
			return nil, errBootstrapDeclined
		}
	} else {
		proceed, confirmErr := confirmInitRepo(cwd, opts)
		if confirmErr != nil {
			return nil, confirmErr
		}
		if !proceed {
			return nil, errBootstrapDeclined
		}
	}

	// Step 2: git init.
	printBootstrapSection(w, "Setting up git repository")
	if err := gitInit(ctx, runner, cwd); err != nil {
		return nil, fmt.Errorf("git init: %w", err)
	}
	// Clear cached worktree root so subsequent paths.WorktreeRoot calls pick
	// up the freshly created repo.
	paths.ClearWorktreeRootCache()
	fmt.Fprintln(w, "  ✓ Initialized empty git repository")

	// Step 3: resolve commit message (+ skip decision) and ensure git
	// identity. Must run after `git init` so `git config` reads are
	// scoped correctly. The identity check is skipped when the user opts
	// out of the commit, since nothing will be authored.
	message, commit := defaultInitialCommitMessage, true
	if setupChoice == bootstrapSetupCustom {
		message, commit, err = resolveCommitMessage(opts)
		if err != nil {
			return nil, err
		}
	}
	// Identity is deliberately NOT resolved here. It is deferred to the enable
	// command so agent selection completes before any profile lookup or login
	// begins; see runEnableIdentityPreflight.

	return &bootstrapState{
		runner:  runner,
		cwd:     cwd,
		commit:  commit,
		message: message,
	}, nil
}

// runBootstrapFinalize runs the post-setup half: stage + initial commit,
// now including the `.entire/`, agent hook, and settings files written by
// the enable flow. If the user opted out of the initial commit we print
// next-step instructions instead.
func runBootstrapFinalize(ctx context.Context, w io.Writer, s *bootstrapState) error {
	if s == nil {
		return nil
	}

	if s.commit {
		printBootstrapSection(w, "Finalizing")
		committed, err := doInitialCommit(ctx, s.runner, s.cwd, s.message)
		if err != nil {
			return fmt.Errorf("initial commit: %w", err)
		}
		if committed {
			fmt.Fprintln(w, "  ✓ Created initial commit")
		} else {
			fmt.Fprintln(w, "  ✓ Nothing to commit — the folder has no files yet")
		}
	} else {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Skipped initial commit. When you're ready:")
		fmt.Fprintln(w, "    git add -A && git commit -m \"Initial commit\"")
	}

	fmt.Fprintln(w, "\nDone.")
	return nil
}

// confirmInitRepo returns true if we should proceed with `git init`. It
// respects --init-repo / --no-init-repo; otherwise prompts. In
// non-interactive mode we return false without printing anything so
// the caller (setup.go) owns the "Not a git repository" message and
// doesn't end up with duplicate output on stdout + stderr.
func confirmInitRepo(cwd string, opts BootstrapOptions) (bool, error) {
	if opts.NoInitRepo {
		return false, nil
	}
	if opts.InitRepo || opts.Yes {
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		return false, nil
	}

	// Default to No: `entire enable` is often run reflexively inside an
	// existing project, so a stray run in the wrong (non-repo) directory
	// must not initialize a repo just because the user pressed Enter. The
	// absolute path is in the title so a wrong-directory mistake is obvious
	// in both interactive and accessible modes.
	confirmed := false
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Warning: Not a git repository. Initialize a new one in %q?", cwd)).
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

// shouldPromptBootstrapSetupChoice reports whether this is the bare
// interactive bootstrap path. Any option that expresses a granular choice —
// including --init-repo / --no-init-repo, which answer the init consent the
// merged select carries — keeps the established flag behavior instead of
// being overwritten by a preset.
func shouldPromptBootstrapSetupChoice(opts BootstrapOptions) bool {
	return interactive.CanPromptInteractively() &&
		!opts.Yes &&
		!opts.InitRepo &&
		!opts.NoInitRepo &&
		opts.InitialCommitMessage == "" &&
		!opts.SkipInitialCommit
}

// promptBootstrapSetupChoice merges the init consent and the common
// bootstrap decisions into one select, so the bare interactive path costs a
// single answer. It runs _before_ `git init`: declining — including Ctrl-C —
// leaves the folder untouched. The selected commit is still deferred until
// Entire has written its settings and agent configuration.
//
// The wrong-directory guard from the granular confirm (issue #1717) carries
// over: the absolute path stays in the title (the accessible renderer drops
// descriptions), and the menu makes the choice visible before Enter lands on
// the recommended preset.
func promptBootstrapSetupChoice(w io.Writer, cwd string) (bootstrapSetupChoice, error) {
	choice := bootstrapSetupLocal
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[bootstrapSetupChoice]().
				Title(fmt.Sprintf("No git repository in %q. Set one up?", cwd)).
				Options(
					huh.NewOption("Yes, with one initial commit (recommended)", bootstrapSetupLocal),
					huh.NewOption("Yes, customize...", bootstrapSetupCustom),
					huh.NewOption("No", bootstrapSetupDecline),
				).
				Value(&choice),
		),
	).WithOutput(w)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return bootstrapSetupDecline, nil
		}
		return "", fmt.Errorf("git setup prompt: %w", err)
	}
	return choice, nil
}

// resolveCommitMessage returns the message to use for the initial
// commit. The second return value is false when the user chose to skip
// the initial commit entirely; callers must skip `doInitialCommit`.
func resolveCommitMessage(opts BootstrapOptions) (string, bool, error) {
	if opts.SkipInitialCommit {
		return "", false, nil
	}
	if opts.InitialCommitMessage != "" {
		return opts.InitialCommitMessage, true, nil
	}
	if opts.Yes || !interactive.CanPromptInteractively() {
		return defaultInitialCommitMessage, true, nil
	}

	const (
		choiceDefault   = "default"
		choiceCustomize = "custom"
		choiceSkip      = "skip"
	)
	choice := choiceDefault
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Initial commit").
				Options(
					huh.NewOption(`Commit with default message "Initial commit"`, choiceDefault),
					huh.NewOption("Customize message...", choiceCustomize),
					huh.NewOption("Skip — I'll commit manually later", choiceSkip),
				).
				Value(&choice),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", false, errBootstrapInterrupted
		}
		return "", false, fmt.Errorf("commit message prompt: %w", err)
	}

	switch choice {
	case choiceSkip:
		return "", false, nil
	case choiceCustomize:
		input := defaultInitialCommitMessage
		custom := NewAccessibleForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Initial commit message").
					Value(&input),
			),
		)
		if err := custom.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return "", false, errBootstrapInterrupted
			}
			return "", false, fmt.Errorf("commit message prompt: %w", err)
		}
		if strings.TrimSpace(input) == "" {
			return defaultInitialCommitMessage, true, nil
		}
		return input, true, nil
	default:
		return defaultInitialCommitMessage, true, nil
	}
}

// gitInit runs `git init` in the given directory.
func gitInit(ctx context.Context, runner bootstrapRunner, dir string) error {
	if _, err := runner.RunInDir(ctx, dir, "git", "init"); err != nil {
		return fmt.Errorf("run git init: %w", err)
	}
	return nil
}

// doInitialCommit stages all files and creates a commit. Returns whether a
// commit was actually created (false if there were no files to stage).
func doInitialCommit(ctx context.Context, runner bootstrapRunner, dir, message string) (bool, error) {
	if _, err := runner.RunInDir(ctx, dir, "git", "add", "-A"); err != nil {
		return false, wrapExecError("git add", err)
	}
	// Check if the staging area has anything at all.
	// --no-optional-locks keeps this a read: a bare `git status` rewrites
	// .git/index to refresh its stat cache (issue #2111).
	out, err := runner.RunInDir(ctx, dir, "git", "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return false, wrapExecError("git status", err)
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	// Disable GPG signing for this commit only. Fresh environments often
	// have commit.gpgsign=true inherited from a global config but no
	// working signer; passing -c keeps the user's global config intact.
	if _, err := runner.RunInDir(ctx, dir, "git", "-c", "commit.gpgsign=false", "commit", "-m", message); err != nil {
		return false, wrapExecError("git commit", err)
	}
	return true, nil
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

// ghCurrentUser returns the authenticated GitHub user's login. Read-only:
// `entire trail list --author me` resolves itself through it.
func ghCurrentUser(ctx context.Context, runner bootstrapRunner) (string, error) {
	out, err := runner.Run(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(out), nil
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
