package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"slices"
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
)

// maxReplacedCommits bounds how many dropped commits a redo considers.
const maxReplacedCommits = 200

// inheritReplacedCommitsTrailers carries checkpoint trailers from commits this
// commit re-does. After `git reset` and a new commit, the dropped commits are
// the ones the reset left unreachable from every branch and remote (see
// replacedCommits). A dropped commit is inherited when every file it changed
// that is staged now has exactly the content it had before the reset: the work
// is recommitted, not rewritten, and one coincidentally identical file among
// rewritten ones is not a redo. Returns what was inherited; the zero redo when
// nothing applies.
func (s *ManualCommitStrategy) inheritReplacedCommitsTrailers(ctx context.Context, repo *git.Repository, commitMsgFile string) redo {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	logging.Debug(logCtx, "prepare-commit-msg: checking for redone commits")
	tip, replaced, why := replacedCommits(ctx, repo)
	if len(replaced) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: no commits being redone", slog.String("reason", why))
		return redo{}
	}
	staged, err := stagedBlobs(ctx)
	if err != nil || len(staged) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: redo check has no staged blobs to compare",
			slog.Int("replaced_commits", len(replaced)), slog.Any("error", err))
		return redo{}
	}
	tipTree, err := tip.Tree()
	if err != nil {
		return redo{}
	}
	// Each dropped commit's diff is computed once; matching and ownership both
	// read it.
	changes := make([]object.Changes, len(replaced))
	for i, c := range replaced {
		changes[i], _ = firstParentChanges(c) //nolint:errcheck // a commit without a diff matches nothing
	}
	matched := make([][]string, len(replaced))
	trailerIDs := make([][]id.CheckpointID, len(replaced))
	var candidates []id.CheckpointID
	for i := range replaced {
		if matched[i] = recommitsFilesOf(changes[i], tipTree, staged); len(matched[i]) > 0 {
			trailerIDs[i] = FinalCheckpointTrailers(ctx, replaced[i].Message)
			candidates = append(candidates, trailerIDs[i]...)
		}
	}
	live := make(map[id.CheckpointID]bool)
	for _, cpID := range withoutDeletedCheckpoints(ctx, candidates) { // one deleted-list load
		live[cpID] = true
	}
	captured := s.capturedCheckpoints(ctx, repo)
	lastChangedBy := newestChangeOf(changes)
	var inherited []id.CheckpointID
	seen := make(map[id.CheckpointID]bool)
	redonePaths := make(map[string]bool)
	for i := len(replaced) - 1; i >= 0; i-- { // oldest first, like a squash message
		if len(matched[i]) == 0 {
			continue
		}
		var ids []id.CheckpointID
		heldByCheckpoint := false
		for _, cpID := range trailerIDs[i] {
			if live[cpID] {
				ids = append(ids, cpID)
				heldByCheckpoint = heldByCheckpoint || captured(cpID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		// A path's staged state is held by a checkpoint only when the commit
		// that last changed it carries one this clone actually has, and the
		// staged mode is that commit's too. Inheriting stays permissive: a
		// trailer links even when its checkpoint is missing here, but only a
		// checkpoint that exists may stand in for a fresh capture.
		for _, p := range matched[i] {
			if heldByCheckpoint && lastChangedBy[p] == i && sameModeAsTip(tipTree, p, staged[p]) {
				redonePaths[p] = true
			}
		}
		for _, cpID := range ids {
			if !seen[cpID] {
				seen[cpID] = true
				inherited = append(inherited, cpID)
			}
		}
	}
	if len(inherited) == 0 {
		logging.Debug(logCtx, "prepare-commit-msg: redone commits carry no matching trailers",
			slog.Int("replaced_commits", len(replaced)), slog.Int("staged", len(staged)))
		return redo{}
	}
	result := redo{ids: inherited, paths: redonePaths}
	for p := range staged {
		if !redonePaths[p] {
			result.others = append(result.others, p)
		}
	}
	slices.Sort(result.others)
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return redo{}
	}
	message := string(content)
	rules := messageCommentRules(ctx, message)
	// Only the final block links; a trailer quoted in the body does not.
	present := make(map[id.CheckpointID]bool)
	for _, cpID := range preparedFinalTrailers(message, rules) {
		present[cpID] = true
	}
	added := 0
	for _, cpID := range inherited {
		if !present[cpID] {
			message = addCheckpointTrailerAboveComments(message, cpID, rules)
			added++
		}
	}
	if added > 0 {
		if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil { //nolint:gosec // path from git hook arg
			return redo{}
		}
	}
	logging.Info(logCtx, "prepare-commit-msg: inherited checkpoint trailers from the commits being redone",
		slog.Int("replaced_commits", len(replaced)),
		slog.Int("inherited", len(inherited)),
		slog.Int("added", added),
		slog.Int("redone_paths", len(redonePaths)),
		slog.Int("staged", len(staged)))
	return result
}

// redo is what a commit inherited from the commits it redoes: their checkpoint
// IDs, the staged paths those checkpoints already hold (content and mode, as
// last changed by a commit whose checkpoint this clone has), and every other
// staged path. Both sets come from one rename-free view of the commit index.
// Held paths are no evidence of new session work: a reset puts them back in
// the worktree, and the turn end that follows records them as touched again
// although the session did nothing new to them.
type redo struct {
	ids    []id.CheckpointID
	paths  map[string]bool
	others []string
}

// onlyRedone reports whether everything staged is work a checkpoint holds.
func (r redo) onlyRedone() bool {
	return len(r.paths) > 0 && len(r.others) == 0
}

// capturedCheckpoints returns a memoized check that a checkpoint exists in
// this clone's store. Without a store nothing counts as captured, so a redo
// never suppresses a fresh capture it cannot vouch for.
func (s *ManualCommitStrategy) capturedCheckpoints(ctx context.Context, repo *git.Repository) func(id.CheckpointID) bool {
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return func(id.CheckpointID) bool { return false }
	}
	known := make(map[id.CheckpointID]bool)
	return func(cpID id.CheckpointID) bool {
		exists, ok := known[cpID]
		if !ok {
			exists = checkpointExists(ctx, store, cpID)
			known[cpID] = exists
		}
		return exists
	}
}

// reconcileRedoneFiles settles a redone commit that stamped no fresh trailer
// with the sessions in this worktree: a pure redo, or a partial redo whose
// other staged files were no session's work. No condensation consumes the held
// files the reset put back in those sessions' touched sets, so without this
// they stay pending as if the session still owed a checkpoint for them. Each
// held file the commit contains with the session's recorded content leaves
// FilesTouched, exactly as carry-forward drops committed files after a
// condensation; a held path the commit does not contain stays. Other touched
// files, turn-end steps and transcript growth are left alone: that is work no
// checkpoint holds yet.
func (s *ManualCommitStrategy) reconcileRedoneFiles(ctx context.Context, repo *git.Repository, commit *object.Commit, redone map[string]bool) {
	if len(redone) == 0 {
		return
	}
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	sessions, err := s.findExactSessionsForWorktree(ctx, worktreePath)
	if err != nil {
		return
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")
	for _, sess := range sessions {
		if sess.FullyCondensed || !slices.ContainsFunc(sess.FilesTouched, func(f string) bool { return redone[f] }) {
			continue
		}
		mutErr := MutateSessionState(ctx, sess.SessionID, func(state *SessionState) error {
			// Only held files go through the carry-forward check; every other
			// touched file keeps its place untouched (the check would also
			// prune unrelated paths, which is a condensation's call).
			var held []string
			committed := make(map[string]struct{})
			for _, f := range state.FilesTouched {
				if redone[f] {
					held = append(held, f)
					committed[f] = struct{}{}
				}
			}
			if len(held) == 0 {
				return ErrMutationSkip
			}
			stillPending := make(map[string]bool)
			for _, f := range filesWithRemainingAgentChanges(ctx, repo, state.TouchedFileHashes, commit, held, committed) {
				stillPending[f] = true
			}
			remaining := slices.DeleteFunc(slices.Clone(state.FilesTouched), func(f string) bool {
				return redone[f] && !stillPending[f]
			})
			if len(remaining) == len(state.FilesTouched) {
				return ErrMutationSkip
			}
			logging.Debug(logCtx, "post-commit: redone files settled for session",
				slog.String("session_id", state.SessionID),
				slog.Int("files_touched_before", len(state.FilesTouched)),
				slog.Int("remaining_files", len(remaining)))
			state.FilesTouched = remaining
			pruneTouchedFileHashes(state)
			return nil
		})
		if mutErr != nil {
			logging.Debug(logCtx, "post-commit: could not settle redone files",
				slog.String("session_id", sess.SessionID), slog.String("error", mutErr.Error()))
		}
	}
}

// withRedoneTrailers adds the trailers of the commits this one redoes to the
// message and to inherited, and records the combined set for post-commit.
func (s *ManualCommitStrategy) withRedoneTrailers(ctx context.Context, repo *git.Repository, commitMsgFile, source string, inherited []id.CheckpointID) ([]id.CheckpointID, redo) {
	redone := s.inheritReplacedCommitsTrailers(ctx, repo, commitMsgFile)
	if len(redone.ids) == 0 {
		return inherited, redone
	}
	inherited = append(inherited, redone.ids...)
	recordInheritedMarker(ctx, inherited, redone.paths)
	if redone.onlyRedone() {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"),
			"prepare-commit-msg: commit only redoes linked work, no new trailer",
			slog.String("source", source))
	}
	return inherited, redone
}

// prepareAmendCommitMsg keeps or restores the amended commit's own trailer and
// folds in a reset-dropped commit's trailers: `git reset --soft HEAD~1 && git
// commit --amend` squashes the last two commits, and the amended commit carries
// the work of both.
func (s *ManualCommitStrategy) prepareAmendCommitMsg(ctx context.Context, commitMsgFile string, strippedDeleted bool) error {
	if err := s.handleAmendCommitMsg(ctx, commitMsgFile, strippedDeleted); err != nil {
		return err
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil // the amend itself is prepared; folding is best-effort
	}
	defer repo.Close()
	recordInheritedTrailers(ctx, s.inheritReplacedCommitsTrailers(ctx, repo, commitMsgFile).ids)
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
	tipTree, err := tip.Tree()
	if err != nil {
		return nil, nil, "the reset's old HEAD has no tree"
	}
	// A commit since the reset that recommitted nothing at the old HEAD's
	// content cannot have redone dropped work. Checked first, newest first,
	// because it is the usual answer for every ordinary commit after a reset
	// and it needs no subprocess or walk of the dropped commits.
	sinceCommits := make([]*object.Commit, 0, len(since))
	for _, h := range since {
		c, err := repo.CommitObject(h)
		if err != nil || !redoesDroppedWork(c, nil, tipTree) {
			return nil, nil, "a commit since the reset redid none of the dropped work"
		}
		sinceCommits = append(sinceCommits, c)
	}
	dropped := unreachableCommits(ctx, repo, tipHash)
	if len(dropped) == 0 {
		return nil, nil, "the reset dropped nothing a branch does not still reach"
	}
	changed := changedPaths(dropped)
	for _, c := range sinceCommits {
		if !redoesDroppedWork(c, changed, tipTree) {
			return nil, nil, "a commit since the reset redid none of the dropped work"
		}
	}
	return tip, dropped, ""
}

type reflogEntry struct {
	oldHash, newHash plumbing.Hash
	msg              string
}

// maxReflogBytes bounds how much of HEAD's reflog a commit hook reads, and
// maxReflogEntries is the same bound, roughly, for the git fallback.
const (
	maxReflogBytes   = 64 << 10
	maxReflogEntries = 400
)

// headReflog returns HEAD's most recent reflog entries, newest first, read
// from the per-worktree logs/HEAD so each entry carries the old and new HEAD.
func headReflog(ctx context.Context) []reflogEntry {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	f, err := osroot.OpenNoFollow(root, "logs/HEAD")
	if errors.Is(err, fs.ErrNotExist) {
		// The reftable backend keeps reflogs in .git/reftable/, which only
		// git reads; the files backend just has none yet.
		return headReflogFromGit(ctx)
	}
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

// headReflogFromGit reads HEAD's reflog through git, for the reftable backend.
// git prints only each entry's new HEAD; an entry's old HEAD is the new HEAD of
// the entry before it.
func headReflogFromGit(ctx context.Context) []reflogEntry {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "reflog", "show", "-n", strconv.Itoa(maxReflogEntries), "--format=%H%x09%gs", "HEAD")
	cmd.Dir = root
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not read HEAD's reflog",
			slog.String("error", err.Error()))
		return nil
	}
	var entries []reflogEntry
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, line := range lines {
		hash, msg, ok := strings.Cut(line, "\t")
		if !ok || !plumbing.IsHash(hash) {
			continue
		}
		e := reflogEntry{newHash: plumbing.NewHash(hash), msg: msg}
		if i+1 < len(lines) {
			if older, _, ok := strings.Cut(lines[i+1], "\t"); ok && plumbing.IsHash(older) {
				e.oldHash = plumbing.NewHash(older)
			}
		}
		entries = append(entries, e)
	}
	return entries
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
// content it had at the reset's old HEAD. A nil dropped set accepts any path,
// the cheap necessary condition checked before the dropped commits are known.
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
			if p == "" || (dropped != nil && !dropped[p]) {
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
	mode    filemode.FileMode
	deleted bool
}

// stagedBlobs maps each file staged against HEAD to what the commit index
// stages. A detected rename would report only its destination, hiding the
// staged deletion of its source, so rename detection is off.
func stagedBlobs(ctx context.Context) (map[string]stagedEntry, error) {
	changes, err := getStagedChanges(ctx, "--no-renames")
	if err != nil {
		return nil, err
	}
	staged := make(map[string]stagedEntry, len(changes))
	for _, c := range changes {
		staged[c.path] = c.stagedEntry
	}
	return staged, nil
}

// recommitsFilesOf returns the staged paths that recommit a dropped commit's
// work (its first-parent changes), or nil when it is not being redone: at
// least one file it changed is staged, and every staged file it changed has
// the content it had at the reset's old HEAD (tipTree).
func recommitsFilesOf(changes object.Changes, tipTree *object.Tree, staged map[string]stagedEntry) []string {
	var matched []string
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
				return nil
			case entry.deleted && absent, !entry.deleted && !absent && tipEntry.Hash.Equal(entry.blob):
				matched = append(matched, path)
			default:
				return nil // staged, but rewritten: not this commit's work
			}
		}
	}
	return matched
}

// newestChangeOf maps each path the replaced commits changed to the index of
// the newest one that changed it (replaced, and so changes, is newest first).
func newestChangeOf(changes []object.Changes) map[string]int {
	out := make(map[string]int)
	for i := len(changes) - 1; i >= 0; i-- {
		for _, change := range changes[i] {
			for _, p := range []string{change.To.Name, change.From.Name} {
				if p != "" {
					out[p] = i
				}
			}
		}
	}
	return out
}

// sameModeAsTip reports whether a staged path has the mode it had at the
// reset's old HEAD. recommitsFilesOf matched its content already; a staged
// deletion has no mode to compare.
func sameModeAsTip(tipTree *object.Tree, path string, entry stagedEntry) bool {
	if entry.deleted {
		return true
	}
	tipEntry, absent, err := findTreeEntry(tipTree, path)
	return err == nil && !absent && tipEntry.Mode == entry.mode
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
