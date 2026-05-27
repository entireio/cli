package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

type discoveryOptions struct {
	since string
	head  string
}

type discoveredCheckpoint struct {
	ID      checkpointID.CheckpointID
	Commits []discoveredCommit
}

type discoveredCommit struct {
	Hash     plumbing.Hash
	ShortSHA string
	Date     time.Time
}

type historyTip struct {
	name string
	hash plumbing.Hash
}

type discoveryScope struct {
	excluded  map[plumbing.Hash]bool
	sinceHash plumbing.Hash
	hasSince  bool
}

func discoverCheckpointHistory(ctx context.Context, repo *git.Repository, opts discoveryOptions) ([]discoveredCheckpoint, error) {
	checkpoints, _, err := discoverCheckpointHistoryWithSkippedOrphans(ctx, repo, opts)
	return checkpoints, err
}

func discoverCheckpointHistoryWithSkippedOrphans(ctx context.Context, repo *git.Repository, opts discoveryOptions) ([]discoveredCheckpoint, int, error) {
	checkpoints, checkpointIndexes, err := discoverTrailerCheckpointHistory(ctx, repo, opts)
	if err != nil {
		return nil, 0, err
	}

	v2OrphansSkipped, err := addV2OrphanCheckpoints(ctx, repo, opts, checkpointIndexes, &checkpoints)
	if err != nil {
		return nil, 0, err
	}

	sortDiscoveredCheckpoints(checkpoints)
	return checkpoints, v2OrphansSkipped, nil
}

func discoverTrailerCheckpointHistory(ctx context.Context, repo *git.Repository, opts discoveryOptions) ([]discoveredCheckpoint, map[string]int, error) {
	scope, err := newDiscoveryScope(ctx, repo, opts.since)
	if err != nil {
		return nil, nil, err
	}

	tips, err := historyTips(ctx, repo, opts.head, scope)
	if err != nil {
		return nil, nil, err
	}

	seenCommits := make(map[plumbing.Hash]bool)
	checkpointIndexes := make(map[string]int)
	checkpoints := make([]discoveredCheckpoint, 0)

	for _, tip := range tips {
		if err := scanTip(ctx, repo, tip, scope.excluded, seenCommits, checkpointIndexes, &checkpoints); err != nil {
			return nil, nil, err
		}
	}

	return checkpoints, checkpointIndexes, nil
}

func newDiscoveryScope(ctx context.Context, repo *git.Repository, since string) (discoveryScope, error) {
	if since == "" {
		return discoveryScope{excluded: make(map[plumbing.Hash]bool)}, nil
	}

	sinceHash, err := resolveRevision(repo, since)
	if err != nil {
		return discoveryScope{}, fmt.Errorf("resolve --since %q: %w", since, err)
	}
	excluded, err := reachableCommits(ctx, repo, sinceHash)
	if err != nil {
		return discoveryScope{}, err
	}
	return discoveryScope{
		excluded:  excluded,
		sinceHash: sinceHash,
		hasSince:  true,
	}, nil
}

func historyTips(ctx context.Context, repo *git.Repository, head string, scope discoveryScope) ([]historyTip, error) {
	if head != "" {
		hash, err := resolveRevision(repo, head)
		if err != nil {
			return nil, fmt.Errorf("resolve --head %q: %w", head, err)
		}
		if err := requireTipContainsSince(ctx, repo, hash, head, scope); err != nil {
			return nil, err
		}
		return []historyTip{{name: head, hash: hash}}, nil
	}

	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	defer iter.Close()

	var tips []historyTip
	seenHashes := make(map[plumbing.Hash]bool)
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if !isHistoryRef(ref) {
			return nil
		}

		hash := ref.Hash()
		if seenHashes[hash] {
			return nil
		}
		include, includeErr := tipContainsSince(ctx, repo, hash, scope)
		if includeErr != nil {
			return fmt.Errorf("check whether %s contains --since: %w", ref.Name(), includeErr)
		}
		if !include {
			return nil
		}
		seenHashes[hash] = true
		tips = append(tips, historyTip{name: ref.Name().String(), hash: hash})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate refs: %w", err)
	}

	if len(tips) == 0 {
		headRef, headErr := repo.Head()
		if headErr != nil {
			return nil, fmt.Errorf("find HEAD: %w", headErr)
		}
		include, includeErr := tipContainsSince(ctx, repo, headRef.Hash(), scope)
		if includeErr != nil {
			return nil, fmt.Errorf("check whether HEAD contains --since: %w", includeErr)
		}
		if include {
			tips = append(tips, historyTip{name: headRef.Name().String(), hash: headRef.Hash()})
		}
	}

	sort.Slice(tips, func(i, j int) bool {
		return tips[i].name < tips[j].name
	})
	return tips, nil
}

func requireTipContainsSince(ctx context.Context, repo *git.Repository, tipHash plumbing.Hash, tipName string, scope discoveryScope) error {
	contains, err := tipContainsSince(ctx, repo, tipHash, scope)
	if err != nil {
		return fmt.Errorf("check whether --head %q contains --since: %w", tipName, err)
	}
	if !contains {
		return fmt.Errorf("%s is not an ancestor of --head %q", scope.sinceHash, tipName)
	}
	return nil
}

func tipContainsSince(ctx context.Context, repo *git.Repository, tipHash plumbing.Hash, scope discoveryScope) (bool, error) {
	if !scope.hasSince {
		return true, nil
	}
	return commitReachableFrom(ctx, repo, tipHash, scope.sinceHash)
}

func isHistoryRef(ref *plumbing.Reference) bool {
	if ref.Type() != plumbing.HashReference {
		return false
	}
	name := ref.Name()
	if !name.IsBranch() && !name.IsRemote() {
		return false
	}
	if isInternalHistoryRefName(name) {
		return false
	}
	return !strings.HasSuffix(name.String(), "/HEAD")
}

func isInternalHistoryRefName(name plumbing.ReferenceName) bool {
	if name == plumbing.NewBranchReferenceName(paths.MetadataBranchName) ||
		name == plumbing.NewBranchReferenceName(paths.TrailsBranchName) {
		return true
	}

	remotePrefix := "refs/remotes/"
	nameString := name.String()
	if !strings.HasPrefix(nameString, remotePrefix) {
		return false
	}
	remoteAndBranch := strings.TrimPrefix(nameString, remotePrefix)
	_, branchName, ok := strings.Cut(remoteAndBranch, "/")
	if !ok {
		return false
	}
	return branchName == paths.MetadataBranchName || branchName == paths.TrailsBranchName
}

func resolveRevision(repo *git.Repository, revision string) (plumbing.Hash, error) {
	if isShortHexRevision(revision) {
		if err := rejectAmbiguousCommitPrefix(repo, revision); err != nil {
			return plumbing.ZeroHash, err
		}
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return plumbing.ZeroHash, err //nolint:wrapcheck // callers add flag-specific context
	}
	if hash == nil {
		return plumbing.ZeroHash, fmt.Errorf("revision %q resolved to no commit", revision)
	}
	return *hash, nil
}

func isShortHexRevision(revision string) bool {
	if revision == "" || len(revision) >= len(plumbing.ZeroHash.String()) {
		return false
	}
	for _, r := range revision {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func rejectAmbiguousCommitPrefix(repo *git.Repository, revision string) error {
	prefix := strings.ToLower(revision)
	iter, err := repo.CommitObjects()
	if err != nil {
		return fmt.Errorf("list commit objects for revision %q: %w", revision, err)
	}
	defer iter.Close()

	var matches []plumbing.Hash
	err = iter.ForEach(func(commit *object.Commit) error {
		if strings.HasPrefix(commit.Hash.String(), prefix) {
			matches = append(matches, commit.Hash)
			if len(matches) == 2 {
				return storer.ErrStop
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, storer.ErrStop) {
		return fmt.Errorf("scan commit objects for revision %q: %w", revision, err)
	}
	if len(matches) < 2 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].String() < matches[j].String()
	})
	return fmt.Errorf("ambiguous revision %q matches commit prefixes %s and %s", revision, matches[0], matches[1])
}

func reachableCommits(ctx context.Context, repo *git.Repository, from plumbing.Hash) (map[plumbing.Hash]bool, error) {
	iter, err := repo.Log(&git.LogOptions{From: from, Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("get log from %s: %w", from, err)
	}
	defer iter.Close()

	commits := make(map[plumbing.Hash]bool)
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context canceled while excluding commits: %w", err)
		}
		commits[commit.Hash] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate commits reachable from %s: %w", from, err)
	}
	return commits, nil
}

func commitReachableFrom(ctx context.Context, repo *git.Repository, from, target plumbing.Hash) (bool, error) {
	iter, err := repo.Log(&git.LogOptions{From: from, Order: git.LogOrderCommitterTime})
	if err != nil {
		return false, fmt.Errorf("get log from %s: %w", from, err)
	}
	defer iter.Close()

	found := false
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context canceled while checking ancestry: %w", err)
		}
		if commit.Hash == target {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	if errors.Is(err, storer.ErrStop) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("iterate commits from %s: %w", from, err)
	}
	return found, nil
}

func scanTip(ctx context.Context, repo *git.Repository, tip historyTip, excluded, seenCommits map[plumbing.Hash]bool, checkpointIndexes map[string]int, checkpoints *[]discoveredCheckpoint) error {
	iter, err := repo.Log(&git.LogOptions{From: tip.hash, Order: git.LogOrderCommitterTime})
	if err != nil {
		return fmt.Errorf("get log from %s: %w", tip.name, err)
	}
	defer iter.Close()

	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context canceled while scanning commits: %w", err)
		}
		if excluded[commit.Hash] || seenCommits[commit.Hash] {
			return nil
		}
		seenCommits[commit.Hash] = true
		addCheckpointCommit(commit, checkpointIndexes, checkpoints)
		return nil
	})
	if err != nil {
		return fmt.Errorf("iterate commits from %s: %w", tip.name, err)
	}
	return nil
}

func addV2OrphanCheckpoints(ctx context.Context, repo *git.Repository, opts discoveryOptions, checkpointIndexes map[string]int, checkpoints *[]discoveredCheckpoint) (int, error) {
	v2CheckpointIDs, err := listV2MainCheckpointIDs(ctx, repo)
	if err != nil {
		return 0, err
	}
	if len(v2CheckpointIDs) == 0 {
		return 0, nil
	}

	if hasCommitScope(opts) {
		_, unscopedIndexes, err := discoverTrailerCheckpointHistory(ctx, repo, discoveryOptions{})
		if err != nil {
			return 0, err
		}

		return countMissingCheckpointIDs(v2CheckpointIDs, unscopedIndexes), nil
	}

	for _, cpID := range v2CheckpointIDs {
		key := cpID.String()
		if _, exists := checkpointIndexes[key]; exists {
			continue
		}
		checkpointIndexes[key] = len(*checkpoints)
		*checkpoints = append(*checkpoints, discoveredCheckpoint{ID: cpID})
	}

	return 0, nil
}

func hasCommitScope(opts discoveryOptions) bool {
	return opts.since != "" || opts.head != ""
}

func countMissingCheckpointIDs(ids []checkpointID.CheckpointID, indexes map[string]int) int {
	missing := 0
	for _, cpID := range ids {
		if _, exists := indexes[cpID.String()]; !exists {
			missing++
		}
	}
	return missing
}

func listV2MainCheckpointIDs(ctx context.Context, repo *git.Repository) ([]checkpointID.CheckpointID, error) {
	v2Store := checkpoint.NewV2GitStore(repo)
	_, rootTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2MainRefName))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s ref state: %w", paths.V2MainRefName, err)
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, fmt.Errorf("read %s root tree: %w", paths.V2MainRefName, err)
	}

	var ids []checkpointID.CheckpointID
	err = checkpoint.WalkCheckpointShards(ctx, repo, rootTree, func(cpID checkpointID.CheckpointID, cpTreeHash plumbing.Hash) error {
		cpTree, cpTreeErr := repo.TreeObject(cpTreeHash)
		if cpTreeErr != nil {
			return fmt.Errorf("read v2 checkpoint %s tree: %w", cpID, cpTreeErr)
		}
		if _, fileErr := cpTree.File(paths.MetadataFileName); fileErr == nil {
			ids = append(ids, cpID)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s checkpoints: %w", paths.V2MainRefName, err)
	}

	sort.Slice(ids, func(i, j int) bool {
		return ids[i].String() < ids[j].String()
	})
	return ids, nil
}

func addCheckpointCommit(commit *object.Commit, checkpointIndexes map[string]int, checkpoints *[]discoveredCheckpoint) {
	ids := trailers.ParseAllCheckpoints(commit.Message)
	if len(ids) == 0 {
		return
	}

	discovered := discoveredCommit{
		Hash:     commit.Hash,
		ShortSHA: shortHash(commit.Hash),
		Date:     commit.Committer.When,
	}

	for _, id := range ids {
		key := id.String()
		index, ok := checkpointIndexes[key]
		if !ok {
			index = len(*checkpoints)
			checkpointIndexes[key] = index
			*checkpoints = append(*checkpoints, discoveredCheckpoint{ID: id})
		}
		(*checkpoints)[index].Commits = append((*checkpoints)[index].Commits, discovered)
	}
}

func sortDiscoveredCheckpoints(checkpoints []discoveredCheckpoint) {
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].ID.String() < checkpoints[j].ID.String()
	})
	for i := range checkpoints {
		sort.Slice(checkpoints[i].Commits, func(j, k int) bool {
			left := checkpoints[i].Commits[j]
			right := checkpoints[i].Commits[k]
			if !left.Date.Equal(right.Date) {
				return left.Date.After(right.Date)
			}
			return left.Hash.String() < right.Hash.String()
		})
	}
}

func writeCheckpointList(w io.Writer, checkpoints []discoveredCheckpoint) {
	for _, checkpoint := range checkpoints {
		fmt.Fprint(w, checkpoint.ID)
		if len(checkpoint.Commits) == 0 {
			fmt.Fprint(w, " (orphan)")
		}
		for _, commit := range checkpoint.Commits {
			fmt.Fprintf(w, " %s", commit.ShortSHA)
		}
		fmt.Fprintln(w)
	}
}

func writeDiscoveryWarnings(w io.Writer, v2OrphansSkipped int) {
	if v2OrphansSkipped == 0 {
		return
	}
	fmt.Fprintf(w, "warning: %d v2 orphans skipped; re-run without --since/--head to include them\n", v2OrphansSkipped)
}

func shortHash(hash plumbing.Hash) string {
	full := hash.String()
	if len(full) <= checkpointID.ShortIDLength {
		return full
	}
	return full[:checkpointID.ShortIDLength]
}
