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
// Returns the hash of the new chain tip on success. Returns base unchanged
// when signing is disabled in settings (caller is expected to push the
// chain as-is in that case).
func signAndPersistCommits(ctx context.Context, repo *git.Repository, base plumbing.Hash, commits []*object.Commit, stderr io.Writer) (plumbing.Hash, error) {
	if checkpoint.ShouldSkipPushSigning(ctx) {
		return base, nil
	}

	total := len(commits)
	currentTip := base
	for i, original := range commits {
		built, err := buildCherryPickCommit(ctx, repo, original.TreeHash, currentTip, original)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("build cherry-pick: %w", err)
		}

		subject := commitSubject(original.Message)
		fmt.Fprintln(stderr, signingProgressMessage(i+1, total, subject))

		signErr := signCommitForPush(ctx, built)
		for signErr != nil && !errors.Is(signErr, checkpoint.ErrSigningDisabled) {
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

func signingProgressMessage(i, total int, subject string) string {
	subject = strings.SplitN(subject, "\n", 2)[0]
	const maxLen = 80
	if len([]rune(subject)) > maxLen {
		subject = string([]rune(subject)[:maxLen-1]) + "…"
	}
	return fmt.Sprintf("Signing commit %d/%d: %s", i, total, subject)
}

func commitSubject(message string) string {
	return strings.SplitN(message, "\n", 2)[0]
}

// defaultPromptOnSigningFailure asks the user what to do when stderr is a
// TTY; defaults to skip silently in non-interactive contexts.
func defaultPromptOnSigningFailure(_ context.Context, subject string, signErr error, stderr io.Writer) signingAction {
	if !interactive.CanPromptInteractively() {
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

// signLocalCommitsForPush fetches the remote-tracking tip for ref, finds
// the local-only commits above it, signs and cherry-picks them onto that
// tip, and advances the local ref to the new signed tip. The caller is then
// expected to do a fast-forward push.
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

	if fetchErr := fetchRefBestEffort(ctx, target, ref); fetchErr != nil {
		logging.Debug(ctx, "fetch for sign-at-push failed; treating all local commits as new",
			slog.String("ref", ref.String()),
			slog.String("error", fetchErr.Error()))
	}

	localRef, err := repo.Reference(ref, true)
	if err != nil {
		// No local ref yet — nothing to sign or push.
		return nil
	}

	remoteHash := lookupRemoteTipForSigning(ctx, repo, target, ref)
	// When there is no remote tip (first push), fall back to HEAD so that only
	// commits on the checkpoint branch itself are signed. Without this guard the
	// traversal walks back through the user's working-branch history and
	// cherry-picks a synthetic signed copy of every ancestor.
	if remoteHash == plumbing.ZeroHash {
		if head, headErr := repo.Head(); headErr == nil {
			remoteHash = head.Hash()
		}
	}

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

	newTip, err := signAndPersistCommits(ctx, repo, remoteHash, commits, stderr)
	if err != nil {
		return err
	}

	refsBundle := checkpoint.ResolveCommittedRefs(ctx)
	return AdvanceLocalRef(ctx, repo, refsBundle, ref, newTip)
}

// lookupRemoteTipForSigning returns the hash of the remote-tracking ref for
// target/ref, or ZeroHash when no tracking ref exists (first push).
func lookupRemoteTipForSigning(ctx context.Context, repo *git.Repository, target string, ref plumbing.ReferenceName) plumbing.Hash {
	if !ref.IsBranch() {
		return plumbing.ZeroHash
	}
	rname := plumbing.NewRemoteReferenceName(target, ref.Short())
	r, err := repo.Reference(rname, true)
	if err != nil {
		logging.Debug(ctx, "no remote-tracking ref; treating all local commits as new",
			slog.String("ref", rname.String()),
			slog.String("error", err.Error()))
		return plumbing.ZeroHash
	}
	return r.Hash()
}

// fetchRefBestEffort fetches ref from target into the standard remote-tracking
// ref (refs/remotes/<target>/<branch>) without doing any local rebase. It is
// called before signing so that lookupRemoteTipForSigning finds a fresh tip.
// Failures are silently ignored by the caller — this is a best-effort update.
func fetchRefBestEffort(ctx context.Context, target string, ref plumbing.ReferenceName) error {
	if !ref.IsBranch() || remote.IsURL(target) {
		// Non-branch refs and URL targets don't have a standard remote-tracking
		// ref, so there is nothing useful to fetch here.
		return nil
	}

	fetchTarget, err := remote.ResolveFetchTarget(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	refSpec := fmt.Sprintf("+%s:refs/remotes/%s/%s", ref.String(), target, ref.Short())
	if _, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
	}); fetchErr != nil {
		return fmt.Errorf("fetch ref %s from %s: %w", ref, target, fetchErr)
	}
	return nil
}
