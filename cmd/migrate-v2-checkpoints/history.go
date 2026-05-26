package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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

func discoverCheckpointHistory(ctx context.Context, repo *git.Repository, opts discoveryOptions) ([]discoveredCheckpoint, error) {
	excluded, err := excludedCommits(ctx, repo, opts.since)
	if err != nil {
		return nil, err
	}

	tips, err := historyTips(repo, opts.head)
	if err != nil {
		return nil, err
	}

	seenCommits := make(map[plumbing.Hash]bool)
	checkpointIndexes := make(map[string]int)
	checkpoints := make([]discoveredCheckpoint, 0)

	for _, tip := range tips {
		if err := scanTip(ctx, repo, tip, excluded, seenCommits, checkpointIndexes, &checkpoints); err != nil {
			return nil, err
		}
	}

	sortDiscoveredCheckpoints(checkpoints)
	return checkpoints, nil
}

func excludedCommits(ctx context.Context, repo *git.Repository, since string) (map[plumbing.Hash]bool, error) {
	if since == "" {
		return make(map[plumbing.Hash]bool), nil
	}

	sinceHash, err := resolveRevision(repo, since)
	if err != nil {
		return nil, fmt.Errorf("resolve --since %q: %w", since, err)
	}
	return reachableCommits(ctx, repo, sinceHash)
}

func historyTips(repo *git.Repository, head string) ([]historyTip, error) {
	if head != "" {
		hash, err := resolveRevision(repo, head)
		if err != nil {
			return nil, fmt.Errorf("resolve --head %q: %w", head, err)
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
		tips = append(tips, historyTip{name: headRef.Name().String(), hash: headRef.Hash()})
	}

	sort.Slice(tips, func(i, j int) bool {
		return tips[i].name < tips[j].name
	})
	return tips, nil
}

func isHistoryRef(ref *plumbing.Reference) bool {
	if ref.Type() != plumbing.HashReference {
		return false
	}
	name := ref.Name()
	if !name.IsBranch() && !name.IsRemote() {
		return false
	}
	return !strings.HasSuffix(name.String(), "/HEAD")
}

func resolveRevision(repo *git.Repository, revision string) (plumbing.Hash, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return plumbing.ZeroHash, err //nolint:wrapcheck // callers add flag-specific context
	}
	if hash == nil {
		return plumbing.ZeroHash, fmt.Errorf("revision %q resolved to no commit", revision)
	}
	return *hash, nil
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

func addCheckpointCommit(commit *object.Commit, checkpointIndexes map[string]int, checkpoints *[]discoveredCheckpoint) {
	ids := trailers.ParseAllCheckpoints(commit.Message)
	if len(ids) == 0 {
		return
	}

	discovered := discoveredCommit{
		Hash:     commit.Hash,
		ShortSHA: shortHash(commit.Hash),
		Date:     commit.Author.When,
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
		for _, commit := range checkpoint.Commits {
			fmt.Fprintf(w, " %s", commit.ShortSHA)
		}
		fmt.Fprintln(w)
	}
}

func shortHash(hash plumbing.Hash) string {
	full := hash.String()
	if len(full) <= checkpointID.ShortIDLength {
		return full
	}
	return full[:checkpointID.ShortIDLength]
}
