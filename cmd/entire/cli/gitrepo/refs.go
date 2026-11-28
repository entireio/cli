package gitrepo

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

// ListRefs maps every ref matching any of patterns to its commit, by the exact name
// git stores it under. On a case-insensitive filesystem that name can differ
// in case from the one a lookup resolves, which is what callers comparing ref
// spellings need to see.
func ListRefs(ctx context.Context, repoRoot string, patterns ...string) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)", "--end-of-options"}, patterns...)
	out, err := worktreeGitCommand(ctx, repoRoot, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref %s: %w", strings.Join(patterns, " "), err)
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

// RefUpdate is one change in an UpdateRefs transaction. A zero New deletes the
// ref; a zero Old requires that the ref does not exist yet. Otherwise the ref
// must hold Old.
type RefUpdate struct {
	Name     plumbing.ReferenceName
	New, Old plumbing.Hash
}

// UpdateRefs applies updates in one native git transaction: either every
// update lands or none does, and each is checked against its Old value.
func UpdateRefs(ctx context.Context, repoRoot string, updates []RefUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	var stdin strings.Builder
	for _, u := range updates {
		fmt.Fprintf(&stdin, "update %s %s %s\n", u.Name, u.New, u.Old)
	}
	cmd := worktreeGitCommand(ctx, repoRoot, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(stdin.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git update-ref --stdin: %w", ctx.Err())
		}
		return fmt.Errorf("git update-ref --stdin (%d updates): %w: %s", len(updates), err, strings.TrimSpace(string(out)))
	}
	return nil
}
