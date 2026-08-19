package cli

import (
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// resolveImportLinkCommitSHA returns the commit SHA imported checkpoints are
// anchored to: the default branch's head at import time. Preference order:
// origin's tip of the default branch (the commit most likely already known to
// the server), then the local branch tip, then HEAD. Best-effort — returns ""
// when nothing resolves (e.g. an empty repo); import proceeds without a link.
// This function is the source of truth for the order; the architecture docs
// describe it but defer here.
func resolveImportLinkCommitSHA(repo *git.Repository) string {
	if name := strategy.GetDefaultBranchName(repo); name != "" {
		if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true); err == nil {
			return ref.Hash().String()
		}
		if ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
			return ref.Hash().String()
		}
	}
	if head, err := repo.Head(); err == nil {
		return head.Hash().String()
	}
	return ""
}
