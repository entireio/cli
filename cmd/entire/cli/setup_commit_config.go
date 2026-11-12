package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// configCommitMessage is the message of the commit enable offers to make.
const configCommitMessage = "Enable Entire"

// Overridable in tests.
var (
	commitConfigPrompt = promptCommitConfig
	commitConfigFiles  = gitrepo.CommitPaths
)

// snapshotCleanConfig records, before setup writes anything, which of
// sharedConfigFiles have no uncommitted changes. offerCommitConfig offers to
// commit only those: a file that is already untracked, modified or deleted
// holds the developer's own changes, which a commit of the whole file would
// take along. When git cannot be asked nothing is recorded, and nothing is
// offered.
func (opts *EnableOptions) snapshotCleanConfig(ctx context.Context) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "no commit-config snapshot: worktree root unresolved", "error", err)
		return
	}
	candidates := sharedConfigFiles()
	statuses, err := gitrepo.PathStatuses(ctx, root, candidates)
	if err != nil {
		logging.Debug(ctx, "no commit-config snapshot: git status failed", "error", err)
		return
	}
	opts.configCleanBefore = make(map[string]struct{}, len(candidates))
	for _, file := range candidates {
		if _, dirty := statuses[file]; !dirty {
			opts.configCleanBefore[file] = struct{}{}
		}
	}
}

// offerCommitConfig closes a completed enable by getting the configuration it
// wrote committed, so other worktrees and clones get the same setup. It finds
// whichever of sharedConfigFiles is untracked, modified, or deleted and splits
// them by snapshotCleanConfig: the files this run changed, and the files that
// already had uncommitted changes, which are only ever listed for review. In
// an interactive terminal without --yes or --local and outside an in-progress
// merge, rebase, cherry-pick, revert or bisect, it offers to commit exactly
// the files this run changed; otherwise it lists them. In an empty repository
// it leads with why a commit is needed now. A failure to ask git skips it,
// and a failed commit is reported; the only error it returns is an
// interruption (Ctrl+C), so the command exits the way every other interrupted
// prompt does.
func offerCommitConfig(ctx context.Context, w io.Writer, opts EnableOptions) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return nil
	}
	defer repo.Close()
	emptyRepo := strategy.IsEmptyRepository(repo)

	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "skipping commit-config offer: worktree root unresolved", "error", err)
		return nil
	}
	candidates := sharedConfigFiles()
	statuses, err := gitrepo.PathStatuses(ctx, root, candidates)
	if err != nil {
		logging.Debug(ctx, "skipping commit-config offer: git status failed", "error", err)
		return nil
	}
	var pending, leftToReview []string
	for _, file := range candidates {
		if _, ok := statuses[file]; !ok {
			continue
		}
		if _, clean := opts.configCleanBefore[file]; clean {
			pending = append(pending, file)
		} else {
			leftToReview = append(leftToReview, file)
		}
	}
	if len(pending)+len(leftToReview) == 0 && !emptyRepo {
		return nil
	}

	fmt.Fprintln(w)
	if len(pending)+len(leftToReview) == 0 {
		// Empty repository with the configuration ignored: any commit will do.
		fmt.Fprintln(w, "Session checkpoints require at least one commit. Make one to get started.")
		return nil
	}
	if emptyRepo {
		fmt.Fprintln(w, "Session checkpoints require at least one commit.")
	}
	// Mid-merge, git refuses a partial commit after staging the files, and
	// mid-rebase the commit would land on the rebase's detached HEAD.
	op := strategy.GitOperationInProgress(ctx)
	listOnly := func() {
		printConfigToCommit(w, pending)
		printConfigLeftToReview(w, leftToReview)
	}
	// --local keeps this setup out of the shared configuration, so the shared
	// files it still touched are the developer's call.
	if len(pending) == 0 || opts.Yes || opts.NoCommitOffer || opts.UseLocalSettings || op != "" || !interactive.CanPromptInteractively() {
		listOnly()
		return nil
	}

	branch := currentBranchName(repo)
	// Runs while the prompt's terminal is still open: git's own output (hooks,
	// signing) and the outcome go where the question was asked.
	commitAndReport := func(render io.Writer) error {
		if err := commitConfigFiles(ctx, root, pending, configCommitMessage, render); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr //nolint:wrapcheck // wrapped below as the interruption it is
			}
			fmt.Fprintf(render, "Could not commit them: %v\n", err)
			return nil
		}
		if branch != "" {
			fmt.Fprintf(render, "✓ Committed them to %s\n", branch)
		} else {
			fmt.Fprintln(render, "✓ Committed them")
		}
		return nil
	}
	err = commitConfigPrompt(ctx, opts.promptErrOut(), pending, leftToReview, branch, commitAndReport)
	// Checked before the error is classified: Ctrl+C at the question or
	// during a slow commit hook is an interruption, which
	// handleFormCancellation would read as an answer.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("commit cancelled: %w", ctxErr)
	}
	if err != nil {
		// The prompt reports its own aborts, so this is a prompt that never
		// showed (no terminal to open) or failed: fall back to the list,
		// which is all a non-interactive enable gets anyway.
		logging.Debug(ctx, "commit-config prompt failed", "error", err)
		listOnly()
	}
	return nil
}

// printConfigToCommit lists files, if any, with the reason to commit them.
func printConfigToCommit(w io.Writer, files []string) {
	printConfigFiles(w, "Make sure to commit these files to your Git repo so Entire works across worktrees and clones:", files)
}

// printConfigLeftToReview lists files, if any, that had uncommitted changes
// before setup ran and so are never part of the commit setup offers. It does
// not say Entire needs them: one may belong to an agent that is not enabled.
func printConfigLeftToReview(w io.Writer, files []string) {
	printConfigFiles(w, "These files had uncommitted changes before this run, so they are left for you. Review them and commit the ones that belong to your Entire setup:", files)
}

func printConfigFiles(w io.Writer, heading string, files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Fprintln(w, heading)
	for _, file := range files {
		fmt.Fprintf(w, "  %s\n", file)
	}
}

// sharedConfigFiles returns the worktree-relative files setup writes or
// removes that are meant to be committed: the project settings,
// .entire/.gitignore, and for every registered agent — not only those with
// hooks installed now, so a configuration changed by deselecting an agent is
// included — its hook config and Entire's skill files (search skill,
// agent-help skill, and the legacy search subagent setup deletes). Some of
// these, such as an agent's own settings file, also hold the developer's
// configuration, which is why snapshotCleanConfig decides what may be
// committed.
func sharedConfigFiles() []string {
	files := []string{EntireSettingsFile, paths.EntireDir + "/.gitignore"}
	add := func(p string) {
		if p = filepath.ToSlash(p); p != "" && !slices.Contains(files, p) {
			files = append(files, p)
		}
	}
	for _, name := range agent.List() {
		if ag, err := agent.Get(name); err == nil {
			if locator, ok := agent.AsHookConfigLocator(ag); ok {
				add(locator.HookConfigRelPath())
			}
		}
		add(searchSkillTemplatePath(name))
		add(agentHelpSkillTemplatePath(name))
		add(legacySearchSubagentPath(name))
	}
	return files
}

// currentBranchName returns the branch HEAD points at, including an unborn
// one, or "" for a detached HEAD.
func currentBranchName(repo *git.Repository) string {
	head, err := repo.Reference(plumbing.HEAD, false)
	if err != nil || head.Type() != plumbing.SymbolicReference || !head.Target().IsBranch() {
		return ""
	}
	return head.Target().Short()
}

// promptCommitConfig lists leftToReview and then files, asks whether to commit
// files to branch, with yes as the answer Enter gives since every one of them
// is a change setup made, and on yes runs commitAndReport. The lists, the
// question, an abort's "Commit cancelled.", and commitAndReport's output share
// one writer through
// runPromptFormThen — errOut when it is a terminal, otherwise the controlling
// terminal, held open until all of them are written — so the developer never
// answers about files they cannot see, and sees what came of it, whichever
// stream is redirected. An abort inside the form is an answer and returns
// nil; an interruption (ctx cancelled) or a form failure is returned.
func promptCommitConfig(ctx context.Context, errOut io.Writer, files, leftToReview []string, branch string, commitAndReport func(io.Writer) error) error {
	title := "Commit them now? (HEAD is detached)"
	if branch != "" {
		title = fmt.Sprintf("Commit them to %s now?", branch)
	}
	commit := true
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(title).
				Affirmative("Yes, commit").
				Negative("No").
				Value(&commit),
		),
	)
	preamble := func(w io.Writer) error {
		// The files the question is about come last, next to it.
		printConfigLeftToReview(w, leftToReview)
		printConfigToCommit(w, files)
		return nil
	}
	_, err := runPromptFormThen(ctx, errOut, form, preamble, func(render io.Writer, formErr error) error {
		if formErr != nil {
			if ctx.Err() != nil {
				// An interruption, for the caller to report; handleFormCancellation
				// would read context.Canceled as an abort.
				return formErr
			}
			return handleFormCancellation(render, "Commit", formErr)
		}
		if !commit {
			return nil
		}
		return commitAndReport(render)
	})
	return err
}
