package strategy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// linkAmendedCheckpoints repairs links after Git has completed the amend. The
// rewrite pair is authoritative even for -m/-F, which Git reports as "message"
// to prepare-commit-msg. Never rewrite the code commit or re-condense old data.
func (s *ManualCommitStrategy) linkAmendedCheckpoints(ctx context.Context, repo *git.Repository, pairs []rewritePair) error {
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("read amended HEAD: %w", err)
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return fmt.Errorf("open amended checkpoint store: %w", err)
	}
	infos, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("list amended checkpoint links: %w", err)
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve amend worktree: %w", err)
	}
	link := checkpoint.LinkedCommit{SHA: head.Hash().String()}
	if election, resolveErr := ResolveCheckpointSyncRemote(ctx); resolveErr == nil {
		if forge, owner, name, remoteErr := gitremote.ResolveRemoteRepo(ctx, election.Name); remoteErr == nil && forge != "" {
			link.Repo = forge + "/" + owner + "/" + name
		}
	}
	var writeErrors []error
	for _, pair := range pairs {
		if !plumbing.IsHash(pair.OldSHA) || !plumbing.IsHash(pair.NewSHA) {
			continue
		}
		oldHash := plumbing.NewHash(pair.OldSHA)
		if !plumbing.NewHash(pair.NewSHA).Equal(head.Hash()) || oldHash.Equal(head.Hash()) {
			continue
		}
		old, readErr := repo.CommitObject(oldHash)
		if readErr != nil {
			return fmt.Errorf("read replaced commit: %w", readErr)
		}
		current, readErr := repo.CommitObject(head.Hash())
		if readErr != nil {
			return fmt.Errorf("read amended commit: %w", readErr)
		}
		if !slices.EqualFunc(old.ParentHashes, current.ParentHashes, plumbing.Hash.Equal) {
			continue // a mapping for a different lineage is not an amend
		}
		oldIDs, parseErr := parsedCommitTrailers(ctx, root, old.Message)
		if parseErr != nil {
			return parseErr
		}
		newIDs, parseErr := parsedCommitTrailers(ctx, root, current.Message)
		if parseErr != nil {
			return parseErr
		}
		// An earlier -m amend may have no trailers at all; carry its stored
		// associations forward too, so repeated amends do not lose history.
		for _, cid := range checkpoint.CheckpointsForCommit(infos, oldHash.String(), oldIDs) {
			if slices.Contains(newIDs, cid) {
				continue
			}
			if err := store.Write(ctx, checkpoint.CheckpointCommitLinks{CheckpointID: cid, Links: []checkpoint.LinkedCommit{link}}); err != nil {
				writeErrors = append(writeErrors, fmt.Errorf("link amended checkpoint %s: %w", cid, err))
			}
		}
	}
	return errors.Join(writeErrors...)
}

// Git's parser accepts only real trailers, not checkpoint-shaped lines in the
// subject/body. That distinction matters here because this read authorizes a
// write to an existing checkpoint. No repository override or index is needed.
func parsedCommitTrailers(ctx context.Context, root, message string) ([]id.CheckpointID, error) {
	cmd := exec.CommandContext(ctx, "git", "interpret-trailers", "--parse")
	cmd.Dir = root
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("parse amended commit trailers: %w", err)
	}
	return trailers.ParseAllCheckpoints(string(out)), nil
}
