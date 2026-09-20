package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// resolveImportLinkCommitSHA returns the commit SHA imported checkpoints are
// anchored to: the default branch's head at import time. Preference order:
// origin's tip of the default branch (the commit most likely already known to
// the server), then the local branch tip, then HEAD. Each target must resolve
// to a commit object; an empty repo cannot import until it has a valid anchor.
// This function is the source of truth for the order; the architecture docs
// describe it but defer here.
//
// A candidate can now be SKIPPED rather than merely absent — origin's tip can
// exist as a ref while its object does not, after a prune or a partial fetch —
// and the caller is told nothing when a lower-priority candidate then wins.
// Import decisions are one-shot: nothing re-derives them later, so that
// demotion is logged at Warn, because it is the only record of why a
// checkpoint points where it does. A merely absent ref stays at Debug: a repo
// with no origin is ordinary, and warning about it would be noise on every
// import in a remoteless repo.
func resolveImportLinkCommitSHA(ctx context.Context, repo *git.Repository) (string, error) {
	var refs []plumbing.ReferenceName
	if name := strategy.GetDefaultBranchName(repo); name != "" {
		refs = append(refs, plumbing.NewRemoteReferenceName("origin", name), plumbing.NewBranchReferenceName(name))
	}
	refs = append(refs, plumbing.HEAD)

	tried := make([]string, 0, len(refs))
	for _, name := range refs {
		tried = append(tried, name.Short())
		ref, err := repo.Reference(name, true)
		if err != nil {
			logging.Debug(ctx, "import: anchor candidate absent", "ref", name.String(), "error", err)
			continue
		}
		sha, err := agentimport.ValidateAnchorCommit(repo, ref.Hash().String())
		if err == nil {
			return sha, nil
		}
		// Not about this candidate: the object format is unreadable, so every
		// remaining candidate fails the same way and the refs are not the
		// problem. Returning it keeps the cause in front of the user instead
		// of recommending they fetch code history that is already there.
		if errors.Is(err, agentimport.ErrAnchorRepoConfig) {
			return "", fmt.Errorf("cannot import sessions: %w", err)
		}
		logging.Warn(ctx, "import: anchor candidate rejected; falling through to a lower-priority ref",
			"ref", name.String(), "target", ref.Hash().String(), "error", err)
	}
	return "", fmt.Errorf("cannot import sessions without a valid anchor commit (tried %s): create a commit or fetch the repository's code history", strings.Join(tried, ", "))
}
