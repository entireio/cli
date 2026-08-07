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

// resolveImportScanTips returns the commit tips `entire import --reconcile`
// walks when looking for commits with no session data: origin's default-branch
// tip, the local default-branch tip, and HEAD, deduped and in that order.
//
// It resolves all three rather than just the link anchor because they routinely
// disagree, and each disagreement hides real work: HEAD catches an unmerged
// feature branch (whose commits are unreachable from the default branch), the
// local tip catches commits not yet pushed, and origin's tip catches commits
// merged by someone else. Unresolvable refs are simply absent — a repo with no
// origin, or none at all, still scans whatever exists.
func resolveImportScanTips(repo *git.Repository) []plumbing.Hash {
	var tips []plumbing.Hash
	seen := make(map[plumbing.Hash]struct{}, 3)
	add := func(hash plumbing.Hash) {
		if hash.IsZero() {
			return
		}
		if _, dup := seen[hash]; dup {
			return
		}
		seen[hash] = struct{}{}
		tips = append(tips, hash)
	}

	if name := strategy.GetDefaultBranchName(repo); name != "" {
		if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true); err == nil {
			add(ref.Hash())
		}
		if ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
			add(ref.Hash())
		}
	}
	if head, err := repo.Head(); err == nil {
		add(head.Hash())
	}
	return tips
}
