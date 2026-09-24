package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// maxReplacedCommits bounds how many dropped commits a redo considers.
const maxReplacedCommits = 200

// inheritReplacedCommitsTrailers carries checkpoint trailers from commits this
// commit re-does. After `git reset` and a new commit, the dropped commits are
// the ones the reset left unreachable from every branch and remote (see
// replacedCommits). A dropped commit is inherited when every file it changed
// that is staged now has exactly the content it had before the reset: the work
// is recommitted, not rewritten, and one coincidentally identical file among
// rewritten ones is not a redo. Returns the inherited IDs, nil when nothing
// applies.
func (s *ManualCommitStrategy) inheritReplacedCommitsTrailers(ctx context.Context, repo *git.Repository, commitMsgFile string) []id.CheckpointID {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	logging.Debug(logCtx, "prepare-commit-msg: checking for redone commits")
	tip, replaced, why := replacedCommits(ctx, repo)
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
	tipTree, err := tip.Tree()
	if err != nil {
		return nil
	}
	var inherited []id.CheckpointID
	seen := make(map[id.CheckpointID]bool)
	for i := len(replaced) - 1; i >= 0; i-- { // oldest first, like a squash message
		if !recommitsFilesOf(replaced[i], tipTree, staged) {
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

// withRedoneTrailers adds the trailers of the commits this one redoes to the
// message and to inherited, and records the combined set for post-commit.
func (s *ManualCommitStrategy) withRedoneTrailers(ctx context.Context, repo *git.Repository, commitMsgFile string, inherited []id.CheckpointID) []id.CheckpointID {
	redone := s.inheritReplacedCommitsTrailers(ctx, repo, commitMsgFile)
	if len(redone) == 0 {
		return inherited
	}
	inherited = append(inherited, redone...)
	recordInheritedTrailers(ctx, inherited)
	return inherited
}

// prepareAmendCommitMsg keeps or restores the amended commit's own trailer and
// folds in a reset-dropped commit's trailers: `git reset --soft HEAD~1 && git
// commit --amend` squashes the last two commits, and the amended commit carries
// the work of both.
func (s *ManualCommitStrategy) prepareAmendCommitMsg(ctx context.Context, commitMsgFile string) error {
	if err := s.handleAmendCommitMsg(ctx, commitMsgFile); err != nil {
		return err
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil // the amend itself is prepared; folding is best-effort
	}
	defer repo.Close()
	recordInheritedTrailersOnAmend(ctx, s.inheritReplacedCommitsTrailers(ctx, repo, commitMsgFile))
	return nil
}

// replacedCommits finds the commits the latest reset dropped: the tip HEAD left
// and the commits reachable from it that no branch, remote or HEAD still
// reaches. The reset is read from HEAD's reflog rather than ORIG_HEAD, which a
// later `git reset` to HEAD or `git stash` overwrites. Walking back from the
// newest entry, resets that did not move HEAD (unstaging, stash) are skipped,
// and commits made since count only while each of them redoes some of the
// dropped work; any other ref operation (checkout, merge, rebase, pull) means
// there is no redo in progress.
func replacedCommits(ctx context.Context, repo *git.Repository) (*object.Commit, []*object.Commit, string) {
	var since []plumbing.Hash
	var tipHash plumbing.Hash
	for _, e := range headReflog(ctx) {
		switch {
		case strings.HasPrefix(e.msg, "commit"):
			since = append(since, e.newHash)
			continue
		case strings.HasPrefix(e.msg, "reset:") && e.oldHash == e.newHash:
			continue // unstaging or a stash: HEAD did not move
		case strings.HasPrefix(e.msg, "reset:"):
			tipHash = e.oldHash
		default:
			return nil, nil, "the latest ref operation was not a reset"
		}
		break
	}
	if tipHash.IsZero() {
		return nil, nil, "no reset in HEAD's reflog"
	}
	tip, err := repo.CommitObject(tipHash)
	if err != nil {
		return nil, nil, "the reset's old HEAD is not a commit"
	}
	dropped := unreachableCommits(ctx, repo, tipHash)
	if len(dropped) == 0 {
		return nil, nil, "the reset dropped nothing a branch does not still reach"
	}
	tipTree, err := tip.Tree()
	if err != nil {
		return nil, nil, "the reset's old HEAD has no tree"
	}
	changed := changedPaths(dropped)
	for _, h := range since {
		c, err := repo.CommitObject(h)
		if err != nil || !redoesDroppedWork(c, changed, tipTree) {
			return nil, nil, "a commit since the reset redid none of the dropped work"
		}
	}
	return tip, dropped, ""
}

type reflogEntry struct {
	oldHash, newHash plumbing.Hash
	msg              string
}

// maxReflogBytes bounds how much of HEAD's reflog a commit hook reads.
const maxReflogBytes = 64 << 10

// headReflog returns HEAD's most recent reflog entries, newest first, read
// from the per-worktree logs/HEAD so each entry carries the old and new HEAD.
func headReflog(ctx context.Context) []reflogEntry {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	f, err := osroot.OpenNoFollow(root, "logs/HEAD")
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := max(info.Size()-maxReflogBytes, 0)
	data := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line may be cut
	}
	var out []reflogEntry
	for i := len(lines) - 1; i >= 0; i-- {
		head, msg, ok := strings.Cut(lines[i], "\t")
		fields := strings.Fields(head)
		if !ok || len(fields) < 2 || !plumbing.IsHash(fields[0]) || !plumbing.IsHash(fields[1]) {
			continue
		}
		out = append(out, reflogEntry{oldHash: plumbing.NewHash(fields[0]), newHash: plumbing.NewHash(fields[1]), msg: msg})
	}
	return out
}

// unreachableCommits lists the commits reachable from tip that HEAD, no remote
// and no other branch reaches: the work only the reset's old HEAD holds.
// Merged-in history and a teammate's commits still on another branch are never
// dropped. A branch pointing at tip itself is a backup of the very work being
// redone, so it does not count as still reaching it.
func unreachableCommits(ctx context.Context, repo *git.Repository, tip plumbing.Hash) []*object.Commit {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}
	refsCmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(objectname)", "refs/heads", "refs/remotes")
	refsCmd.Dir = root
	refsCmd.Env = gitrepo.EnvWithoutRepoOverrides()
	refs, err := refsCmd.Output()
	if err != nil {
		return nil
	}
	var exclude strings.Builder
	exclude.WriteString("^HEAD\n")
	for _, ref := range strings.Fields(string(refs)) {
		if plumbing.IsHash(ref) && ref != tip.String() {
			exclude.WriteString("^" + ref + "\n")
		}
	}
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--max-count="+strconv.Itoa(maxReplacedCommits), tip.String(), "--stdin")
	cmd.Dir = root
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.Stdin = strings.NewReader(exclude.String())
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var commits []*object.Commit
	for _, line := range strings.Fields(string(out)) {
		if !plumbing.IsHash(line) {
			continue
		}
		if c, err := repo.CommitObject(plumbing.NewHash(line)); err == nil {
			commits = append(commits, c)
		}
	}
	return commits
}

// changedPaths is every path the given commits changed relative to their first
// parent.
func changedPaths(commits []*object.Commit) map[string]bool {
	out := make(map[string]bool)
	for _, c := range commits {
		changes, err := firstParentChanges(c)
		if err != nil {
			continue
		}
		for _, change := range changes {
			for _, p := range []string{change.To.Name, change.From.Name} {
				if p != "" {
					out[p] = true
				}
			}
		}
	}
	return out
}

// redoesDroppedWork reports whether c recommitted a dropped path with the
// content it had at the reset's old HEAD.
func redoesDroppedWork(c *object.Commit, dropped map[string]bool, tipTree *object.Tree) bool {
	changes, err := firstParentChanges(c)
	if err != nil {
		return false
	}
	tree, err := c.Tree()
	if err != nil {
		return false
	}
	for _, change := range changes {
		for _, p := range []string{change.To.Name, change.From.Name} {
			if p == "" || !dropped[p] {
				continue
			}
			want, wantAbsent, wantErr := findTreeEntry(tipTree, p)
			got, gotAbsent, gotErr := findTreeEntry(tree, p)
			if wantErr != nil || gotErr != nil {
				continue
			}
			if wantAbsent == gotAbsent && (wantAbsent || want.Hash.Equal(got.Hash)) {
				return true
			}
		}
	}
	return false
}

func firstParentChanges(c *object.Commit) (object.Changes, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only test for nil
	}
	parent, err := c.Parents().Next()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only test for nil
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only test for nil
	}
	return parentTree.Diff(tree) //nolint:wrapcheck // callers only test for nil
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

// recommitsFilesOf reports whether c's work is what is staged: at least one
// file c changed is staged, and every staged file c changed has the content it
// had at the reset's old HEAD (tipTree).
func recommitsFilesOf(c *object.Commit, tipTree *object.Tree, staged map[string]stagedEntry) bool {
	changes, err := firstParentChanges(c)
	if err != nil {
		return false
	}
	matched := false
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
			tipEntry, absent, err := findTreeEntry(tipTree, path)
			switch {
			case err != nil:
				return false
			case entry.deleted && absent, !entry.deleted && !absent && tipEntry.Hash.Equal(entry.blob):
				matched = true
			default:
				return false // staged, but rewritten: not this commit's work
			}
		}
	}
	return matched
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
