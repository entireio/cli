package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// maxReplacedCommits bounds the walk from ORIG_HEAD back to the merge base.
const maxReplacedCommits = 200

// inheritReplacedCommitsTrailers carries checkpoint trailers from commits this
// commit re-does. After `git reset` and a new commit, the dropped commits are
// those between the merge base of HEAD and ORIG_HEAD, and ORIG_HEAD itself. A
// dropped commit is inherited when a file it changed is staged now with exactly
// the content it had at ORIG_HEAD: the work is recommitted, not rewritten.
// Returns the inherited IDs, nil when nothing applies.
func (s *ManualCommitStrategy) inheritReplacedCommitsTrailers(ctx context.Context, repo *git.Repository, commitMsgFile string) []id.CheckpointID {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	logging.Debug(logCtx, "prepare-commit-msg: checking for redone commits")
	orig, replaced, why := replacedCommits(ctx, repo)
	if len(replaced) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: no commits being redone", slog.String("reason", why))
		return nil
	}
	staged, err := stagedBlobs(ctx, repo)
	if err != nil || len(staged) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: redo check has no staged blobs to compare",
			slog.Int("replaced_commits", len(replaced)), slog.Any("error", err))
		return nil
	}
	origTree, err := orig.Tree()
	if err != nil {
		return nil
	}
	var inherited []id.CheckpointID
	seen := make(map[id.CheckpointID]bool)
	for i := len(replaced) - 1; i >= 0; i-- { // oldest first, like a squash message
		if !recommitsFilesOf(replaced[i], origTree, staged) {
			continue
		}
		for _, cpID := range trailers.ParseAllCheckpoints(replaced[i].Message) {
			if !seen[cpID] {
				seen[cpID] = true
				inherited = append(inherited, cpID)
			}
		}
	}
	if len(inherited) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: redone commits carry no matching trailers",
			slog.Int("replaced_commits", len(replaced)), slog.Int("staged", len(staged)))
		return nil
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return nil
	}
	message := string(content)
	present := make(map[id.CheckpointID]bool)
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		present[cpID] = true
	}
	added := 0
	for _, cpID := range inherited {
		if !present[cpID] {
			message = addCheckpointTrailer(message, cpID)
			added++
		}
	}
	if added > 0 {
		if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil { //nolint:gosec // path from git hook arg
			return nil
		}
	}
	logging.Info(logCtx, "prepare-commit-msg: inherited checkpoint trailers from the commits being redone",
		slog.Int("replaced_commits", len(replaced)),
		slog.Int("inherited", len(inherited)),
		slog.Int("added", added))
	return inherited
}

// replacedCommits returns ORIG_HEAD and the commits from it back to (not
// including) its merge base with HEAD: the commits a reset dropped. Nil when
// there is no ORIG_HEAD, it is HEAD, it is an ancestor of HEAD (a merge or
// pull replaced nothing), or the reset is no longer the latest ref operation:
// ORIG_HEAD outlives the reset that set it, so after a rebase or merge it
// would otherwise match unrelated commits that restore old content.
func replacedCommits(ctx context.Context, repo *git.Repository) (*object.Commit, []*object.Commit, string) {
	origHash, err := repo.ResolveRevision("ORIG_HEAD")
	if err != nil {
		return nil, nil, "no ORIG_HEAD"
	}
	if !resetIsLatestRefOperation(ctx) {
		return nil, nil, "the latest ref operation was not a reset"
	}
	head, err := repo.Head()
	if err != nil || head.Hash().Equal(*origHash) {
		return nil, nil, "ORIG_HEAD is HEAD"
	}
	base, err := computeMergeBase(repo, head.Hash(), *origHash)
	if err != nil || base.IsZero() || base.Equal(*origHash) {
		return nil, nil, "ORIG_HEAD is an ancestor of HEAD or has no single merge base"
	}
	orig, err := repo.CommitObject(*origHash)
	if err != nil {
		return nil, nil, "ORIG_HEAD is not a commit"
	}
	var out []*object.Commit
	iter := object.NewCommitPreorderIter(orig, nil, []plumbing.Hash{base})
	err = iter.ForEach(func(c *object.Commit) error {
		if len(out) >= maxReplacedCommits {
			return object.ErrCanceled // enough; the cap is deliberate
		}
		out = append(out, c)
		return nil
	})
	if err != nil && !errors.Is(err, object.ErrCanceled) {
		return nil, nil, "walk failed"
	}
	return orig, out, ""
}

type stagedEntry struct {
	blob    plumbing.Hash
	deleted bool
}

// stagedBlobs maps each file staged against HEAD to its index state.
func stagedBlobs(ctx context.Context, repo *git.Repository) (map[string]stagedEntry, error) {
	files, err := getStagedFiles(ctx)
	if err != nil {
		return nil, err
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err //nolint:wrapcheck // caller only tests for nil
	}
	inIndex := make(map[string]plumbing.Hash, len(idx.Entries))
	for _, e := range idx.Entries {
		inIndex[e.Name] = e.Hash
	}
	staged := make(map[string]stagedEntry, len(files))
	for _, f := range files {
		if h, ok := inIndex[f]; ok {
			staged[f] = stagedEntry{blob: h}
		} else {
			// getStagedFiles established that this path differs from HEAD. Its
			// absence from the index therefore represents a staged deletion.
			staged[f] = stagedEntry{deleted: true}
		}
	}
	return staged, nil
}

// recommitsFilesOf reports whether a file c changed is staged with the content
// it had at ORIG_HEAD.
func recommitsFilesOf(c *object.Commit, origTree *object.Tree, staged map[string]stagedEntry) bool {
	commitTree, err := c.Tree()
	if err != nil {
		return false
	}
	parent, err := c.Parents().Next()
	if err != nil {
		return false
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return false
	}
	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		return false
	}
	for _, change := range changes {
		// A rename can be partially staged as either the destination addition
		// or the source deletion, so both endpoints can independently prove
		// that part of the dropped commit is being redone.
		for _, path := range []string{change.To.Name, change.From.Name} {
			if path == "" {
				continue
			}
			entry, ok := staged[path]
			if !ok {
				continue
			}
			origEntry, absent, err := findTreeEntry(origTree, path)
			if entry.deleted && err == nil && absent {
				return true
			}
			if err == nil && !absent && origEntry.Hash.Equal(entry.blob) {
				return true
			}
		}
	}
	return false
}

// findTreeEntry distinguishes an absent path from a tree-read failure. Walking
// one component at a time also makes a non-directory ancestor an ordinary
// absence instead of asking go-git to decode that blob as a tree.
func findTreeEntry(tree *object.Tree, path string) (*object.TreeEntry, bool, error) {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		entry, err := tree.FindEntry(part)
		if errors.Is(err, object.ErrEntryNotFound) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("find tree entry %q: %w", part, err)
		}
		if i == len(parts)-1 {
			return entry, false, nil
		}
		if entry.Mode != filemode.Dir {
			return nil, true, nil
		}
		tree, err = tree.Tree(part)
		if err != nil {
			return nil, false, fmt.Errorf("open tree %q: %w", part, err)
		}
	}
	return nil, true, nil
}

// resetIsLatestRefOperation reads HEAD's reflog: skipping the commits made
// since, the most recent entry must be a reset. Anything else (rebase, merge,
// pull, checkout) means ORIG_HEAD describes an older operation.
func resetIsLatestRefOperation(ctx context.Context) bool {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "reflog", "show", "--format=%gs", "-n", "50", "HEAD")
	cmd.Dir = root
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "commit"):
			continue
		case strings.HasPrefix(line, "reset:"):
			return true
		default:
			return false
		}
	}
	return false
}
