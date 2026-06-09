package strategy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

type signingAction int

const (
	signingActionRetry signingAction = iota
	signingActionSkip
	signingActionAbort
)

// signingProgress renders a two-line push-signing display:
//
//	[entire] Signing commits:
//	        3/10: <subject>
//
// The header prints once on the first Update; the status line below it is
// rewritten in place on each subsequent Update. On a non-TTY the status line
// is appended fresh for each commit instead of being overwritten, so CI logs
// retain a record of every signed commit.
type signingProgress struct {
	out         io.Writer
	isTTY       bool
	total       int
	headerDrawn bool
	statusDrawn bool
}

func newSigningProgress(out io.Writer, total int) *signingProgress {
	return &signingProgress{
		out:   out,
		isTTY: interactive.IsTerminalWriter(out),
		total: total,
	}
}

// Update writes the progress line for commit i (1-indexed) with the given
// subject. The first call also prints the static "[entire] Signing commits:"
// header.
func (p *signingProgress) Update(i int, subject string) {
	if !p.headerDrawn {
		fmt.Fprintln(p.out, "[entire] Signing commits:")
		p.headerDrawn = true
	}
	line := fmt.Sprintf("        %d/%d: %s", i, p.total, truncatedSubject(subject))
	if !p.isTTY {
		fmt.Fprintln(p.out, line)
		return
	}
	if p.statusDrawn {
		// Move cursor up one line so we can rewrite the status line in place.
		fmt.Fprint(p.out, "\033[1A")
	}
	fmt.Fprintf(p.out, "\033[2K%s\n", line)
	p.statusDrawn = true
}

// Detach forgets that we've drawn a status line so the next Update prints
// below whatever currently sits on screen (used before showing a prompt that
// writes output we shouldn't overwrite). The header tracking is preserved.
func (p *signingProgress) Detach() {
	p.statusDrawn = false
}

var errSigningAborted = errors.New("checkpoint signing aborted by user")

// signCommitForPush is the strict signer used at push time. Overridden in
// tests.
var signCommitForPush = checkpoint.SignCommit

// promptOnSigningFailure asks the user what to do on a signing failure.
// Overridden in tests; defaults to a stdin/stderr prompt when a TTY is
// available and to "skip" when not.
var promptOnSigningFailure = defaultPromptOnSigningFailure

// signAndPersistCommits cherry-picks each commit in commits onto base
// (oldest-first), signing each new commit before persisting it. On signing
// failure it consults promptOnSigningFailure and either retries, includes
// the commit unsigned, or aborts (returning errSigningAborted).
//
// Tree construction uses a diff-based approach: each commit's delta (relative
// to its own parent) is applied to the current chain tip's tree. This is
// equivalent to what cherryPickOnto does, but with strict signing added.
// Using per-commit deltas rather than the original TreeHash ensures that
// cross-clone cherry-picks accumulate trees correctly — each replica's commits
// only carry their own checkpoint files, so a simple tree-replace would
// overwrite the other clone's data.
//
// Returns the hash of the new chain tip on success. Returns base unchanged
// when signing is disabled in settings (caller is expected to push the
// chain as-is in that case).
func signAndPersistCommits(ctx context.Context, repo *git.Repository, repoPath string, base plumbing.Hash, commits []*object.Commit, stderr io.Writer) (plumbing.Hash, error) {
	if checkpoint.ShouldSkipPushSigning(ctx) {
		return base, nil
	}

	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load shallow hashes: %w", err)
	}

	total := len(commits)
	progress := newSigningProgress(stderr, total)

	currentTip := base
	for i, original := range commits {
		changes, err := treeChangesForCherryPick(ctx, repo, original, shallow)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("tree changes for %s: %w", original.Hash.String()[:7], err)
		}

		var treeHash plumbing.Hash
		switch {
		case len(changes) == 0:
			// No changes relative to parent (e.g. empty-tree orphan init). Use
			// the original tree directly; ApplyTreeChanges with no changes would
			// return the base tree, which is wrong for root commits.
			treeHash = original.TreeHash
		case currentTip == plumbing.ZeroHash:
			treeHash, err = checkpoint.ApplyTreeChanges(ctx, repo, plumbing.ZeroHash, changes)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("apply tree changes for %s: %w", original.Hash.String()[:7], err)
			}
		default:
			tipCommit, tipErr := repo.CommitObject(currentTip)
			if tipErr != nil {
				return plumbing.ZeroHash, fmt.Errorf("get tip commit %s: %w", currentTip.String()[:7], tipErr)
			}
			treeHash, err = checkpoint.ApplyTreeChanges(ctx, repo, tipCommit.TreeHash, changes)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("apply tree changes for %s: %w", original.Hash.String()[:7], err)
			}
		}

		built, err := buildCherryPickCommit(ctx, repo, treeHash, currentTip, original)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("build cherry-pick: %w", err)
		}

		subject := commitSubject(original.Message)
		progress.Update(i+1, subject)

		signErr := signCommitForPush(ctx, built)
		for signErr != nil && !errors.Is(signErr, checkpoint.ErrSigningDisabled) {
			// Detach so the prompt's output lines are not overwritten by the
			// next Update.
			progress.Detach()
			action := promptOnSigningFailure(ctx, subject, signErr, stderr)
			switch action {
			case signingActionRetry:
				signErr = signCommitForPush(ctx, built)
			case signingActionSkip:
				logging.Warn(ctx, "signing skipped for commit",
					slog.String("subject", subject),
					slog.String("error", signErr.Error()),
				)
				built.Signature = ""
				signErr = nil
			case signingActionAbort:
				return plumbing.ZeroHash, errSigningAborted
			}
		}

		newHash, err := persistCherryPickCommit(repo, built)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("persist commit: %w", err)
		}
		currentTip = newHash
	}

	return currentTip, nil
}

// truncatedSubject returns the first line of subject, rune-truncated to 80
// visible characters with an ellipsis suffix when shortened.
func truncatedSubject(subject string) string {
	subject = strings.SplitN(subject, "\n", 2)[0]
	const maxLen = 80
	if len([]rune(subject)) > maxLen {
		subject = string([]rune(subject)[:maxLen-1]) + "…"
	}
	return subject
}

func commitSubject(message string) string {
	return strings.SplitN(message, "\n", 2)[0]
}

// defaultPromptOnSigningFailure asks the user what to do when stderr is a
// TTY; defaults to skip silently in non-interactive contexts.
//
// Two gates must hold before we prompt: CanPromptInteractively (a controlling
// terminal exists, so reading stdin won't hang) AND IsTerminalWriter(stderr)
// (the question itself will be visible to the user). When pre-push runs as a
// git hook with stderr piped or redirected, /dev/tty may still exist but the
// user can't see what we'd ask them — so we skip instead of risking a hang.
func defaultPromptOnSigningFailure(_ context.Context, subject string, signErr error, stderr io.Writer) signingAction {
	if !interactive.CanPromptInteractively() || !interactive.IsTerminalWriter(stderr) {
		return signingActionSkip
	}
	fmt.Fprintf(stderr, "Failed to sign commit: %s\nError: %v\nRetry, skip, or abort? [r/s/a]: ", subject, signErr)
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return signingActionSkip
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "r", "retry":
			return signingActionRetry
		case "s", "skip":
			return signingActionSkip
		case "a", "abort":
			return signingActionAbort
		}
		fmt.Fprint(stderr, "Please answer r, s, or a: ")
	}
}

// signLocalCommitsForPush fetches the remote tip for ref, finds the
// local-only commits above it, signs and cherry-picks them onto that tip, and
// advances the local ref to the new signed tip. The caller is then expected to
// do a fast-forward push.
//
// When checkpoint signing is disabled in settings, this is a no-op so the
// caller's plain push goes through unchanged.
func signLocalCommitsForPush(ctx context.Context, target string, ref plumbing.ReferenceName, stderr io.Writer) error {
	if checkpoint.ShouldSkipPushSigning(ctx) {
		return nil
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	defer repo.Close()

	localRef, err := repo.Reference(ref, true)
	if err != nil {
		// No local ref yet — nothing to sign or push.
		return nil
	}

	remoteHash, cleanup := resolveRemoteTipForSigning(ctx, repo, target, ref, stderr)
	defer cleanup()

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return fmt.Errorf("repo path: %w", err)
	}

	commits, err := collectCommitsSince(ctx, repo, repoPath, localRef.Hash(), remoteHash)
	if err != nil {
		return fmt.Errorf("collect commits since remote tip: %w", err)
	}
	if len(commits) == 0 {
		return nil
	}

	// In cross-clone scenarios the remote already has its own orphan-init
	// commit. Each clone also has its own local orphan-init commit (different
	// hash). When we cherry-pick B's local chain onto the remote tip we must
	// skip B's orphan-init, otherwise the remote accumulates duplicate
	// empty-tree commits. Only apply this filter when the remote already has
	// commits (remoteHash != ZeroHash); on first-ever push the local orphan
	// is the intended root and must be included.
	//
	// This mirrors the filter that metadata_reconcile.go applies in its
	// disconnected path.
	signingCommits := commits
	if remoteHash != plumbing.ZeroHash {
		dataCommits := commits[:0]
		for _, c := range commits {
			tree, treeErr := c.Tree()
			if treeErr != nil {
				return fmt.Errorf("read tree for commit %s: %w", c.Hash.String()[:7], treeErr)
			}
			if len(tree.Entries) > 0 {
				dataCommits = append(dataCommits, c)
			}
		}
		if len(dataCommits) == 0 {
			return nil
		}
		signingCommits = dataCommits
	}

	newTip, err := signAndPersistCommits(ctx, repo, repoPath, remoteHash, signingCommits, stderr)
	if err != nil {
		return err
	}

	refsBundle := checkpoint.ResolveCommittedRefs(ctx)
	return AdvanceLocalRef(ctx, repo, refsBundle, ref, newTip)
}

// resolveRemoteTipForSigning fetches ref from target into the appropriate
// local-side ref and returns the fetched tip hash. URL targets get a
// transient temp ref (refs/entire-sign-tmp/<rest>) that the returned cleanup
// removes; named-remote targets reuse the standard remote-tracking ref and
// the cleanup is a no-op.
//
// Returns ZeroHash + nil cleanup if anything fails (e.g. remote has no ref
// yet, network failure, non-branch ref). Best-effort: failures only mean the
// caller will treat the local chain as entirely new, which is the same
// pre-fix behaviour for URL targets.
func resolveRemoteTipForSigning(ctx context.Context, repo *git.Repository, target string, ref plumbing.ReferenceName, stderr io.Writer) (plumbing.Hash, func()) {
	cleanup := func() {}
	if !ref.IsBranch() {
		return plumbing.ZeroHash, cleanup
	}

	fetchTarget, err := remote.ResolveFetchTarget(ctx, target)
	if err != nil {
		logging.Debug(ctx, "resolve fetch target failed; treating local chain as new",
			slog.String("target", target),
			slog.String("error", err.Error()))
		return plumbing.ZeroHash, cleanup
	}

	var fetchedRefName plumbing.ReferenceName
	var refSpec string
	usedTempRef := remote.IsURL(fetchTarget)
	if usedTempRef {
		tmp := plumbing.ReferenceName("refs/entire-sign-tmp/" + strings.TrimPrefix(ref.String(), "refs/"))
		refSpec = fmt.Sprintf("+%s:%s", ref.String(), tmp.String())
		fetchedRefName = tmp
		cleanup = func() {
			_ = repo.Storer.RemoveReference(tmp) //nolint:errcheck // best-effort cleanup
		}
	} else {
		refSpec = fmt.Sprintf("+%s:refs/remotes/%s/%s", ref.String(), target, ref.Short())
		fetchedRefName = plumbing.NewRemoteReferenceName(target, ref.Short())
	}

	fmt.Fprintln(stderr, "[entire] Fetching latest from remote...")

	if _, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
	}); fetchErr != nil {
		logging.Debug(ctx, "pre-sign fetch failed; treating local chain as new",
			slog.String("ref", ref.String()),
			slog.String("target", target),
			slog.String("error", fetchErr.Error()))
		cleanup()
		return plumbing.ZeroHash, func() {}
	}

	r, err := repo.Reference(fetchedRefName, true)
	if err != nil {
		logging.Debug(ctx, "fetched ref not present after pre-sign fetch",
			slog.String("ref", fetchedRefName.String()),
			slog.String("error", err.Error()))
		cleanup()
		return plumbing.ZeroHash, func() {}
	}
	return r.Hash(), cleanup
}
