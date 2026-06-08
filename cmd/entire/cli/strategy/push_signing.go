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
