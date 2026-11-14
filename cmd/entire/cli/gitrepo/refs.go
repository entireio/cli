package gitrepo

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

// ListRefs maps every ref matching pattern to its commit, by the exact name
// git stores it under. On a case-insensitive filesystem that name can differ
// in case from the one a lookup resolves, which is what callers comparing ref
// spellings need to see.
func ListRefs(ctx context.Context, repoRoot, pattern string) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	cmd := worktreeGitCommand(ctx, repoRoot, "for-each-ref", "--format=%(objectname) %(refname)", "--end-of-options", pattern)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref %s: %w", pattern, err)
	}
	refs := make(map[plumbing.ReferenceName]plumbing.Hash)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if hash, ok := plumbing.FromHex(oid); ok {
			refs[plumbing.ReferenceName(name)] = hash
		}
	}
	return refs, nil
}

// PackRefs moves the loose refs matching pattern into packed-refs. git before
// 2.42, Apple's bundled git among them, has no pack-refs --include; there it
// packs every loose ref instead, as git gc would.
func PackRefs(ctx context.Context, repoRoot, pattern string) error {
	out, err := worktreeGitCommand(ctx, repoRoot, "pack-refs", "--include", pattern).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("git pack-refs --include %s: %w", pattern, ctx.Err())
	}
	if allOut, allErr := worktreeGitCommand(ctx, repoRoot, "pack-refs", "--all").CombinedOutput(); allErr != nil {
		return fmt.Errorf("git pack-refs --include %s: %w: %s; git pack-refs --all: %w: %s",
			pattern, err, strings.TrimSpace(string(out)), allErr, strings.TrimSpace(string(allOut)))
	}
	return nil
}
