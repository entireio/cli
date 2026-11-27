package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// stampedTrailer returns the checkpoint trailer in message's final trailer
// block that prepare-commit-msg stamped itself (recordStampedTrailer), the last
// one, as post-commit picks it. Any other ID there (pasted, reused with
// `commit -C`, inherited from squashed commits) names a checkpoint this commit
// did not stamp, and would otherwise suppress the fresh trailer new work
// needs; so does an ID anywhere else in the message. A checkpoint deleted from
// this clone never counts either. rules is messageCommentRules' answer for
// message.
func stampedTrailer(ctx context.Context, message string, rules commentRules, inherited []id.CheckpointID) (id.CheckpointID, bool) {
	ids := withoutInherited(withoutDeletedCheckpoints(ctx, preparedFinalTrailers(message, rules)), checkpointSet(inherited))
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false
	}
	return lastStamped(ids, readStampedTrailers(ctx))
}

// preparedFinalTrailers returns the checkpoint IDs in the final trailer block
// of a message prepare-commit-msg was handed, as post-commit will read the
// recorded commit: cleaned as git will clean it (an edited message still
// carries git's comment block and, with `commit -v`, a scissors line and diff
// below the message), then read by git's trailer grammar.
func preparedFinalTrailers(message string, rules commentRules) []id.CheckpointID {
	message = trailers.CutAtScissors(message, rules.prefix)
	if rules.stripped {
		message = trailers.StripEditorComments(message, rules.prefix)
	}
	return trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, rules.trailerPrefix)
}

// lastStamped returns the last of ids that stamped holds.
func lastStamped(ids []id.CheckpointID, stamped map[id.CheckpointID]bool) (id.CheckpointID, bool) {
	for i := len(ids) - 1; i >= 0; i-- {
		if stamped[ids[i]] {
			return ids[i], true
		}
	}
	return id.EmptyCheckpointID, false
}

// pickCondensationTarget chooses the trailer a commit's new work condenses
// into, and reports whether its checkpoint already exists. It is the last
// trailer prepare-commit-msg stamped for this commit. Without one (an amend
// keeps the amended commit's trailer; a sequence operation runs no prepare)
// only an existing checkpoint can take the work, and only from the session
// that owns it (stampedByAnotherCommit): the last trailer whose checkpoint
// exists. A checkpoint this clone does not have is created only for a trailer
// prepare stamped, never for one the message brought (pasted, reused with
// `commit -C`, typed in the editor, a teammate's not yet fetched).
func pickCondensationTarget(ids []id.CheckpointID, stamped map[id.CheckpointID]bool, exists func(id.CheckpointID) bool) (target id.CheckpointID, preexisting, found bool) {
	if target, ok := lastStamped(ids, stamped); ok {
		return target, exists(target), true
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if exists(ids[i]) {
			return ids[i], true, true
		}
	}
	return id.EmptyCheckpointID, false, false
}

// condensationTarget picks the trailer this commit condenses into among ids,
// given the ones prepare stamped, and reports whether that checkpoint already
// existed before this post-commit began. Read once per commit: sessions
// condensing into the same fresh checkpoint later in this hook must not look
// like writes into someone else's.
func (s *ManualCommitStrategy) condensationTarget(ctx context.Context, repo *git.Repository, ids []id.CheckpointID, stamped map[id.CheckpointID]bool) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		// Nothing can be told apart without a store; only a trailer prepare
		// stamped may be written, as a new checkpoint.
		target, found = lastStamped(ids, stamped)
		return target, false, found
	}
	return pickCondensationTarget(ids, stamped, func(cpID id.CheckpointID) bool { return checkpointExists(ctx, store, cpID) })
}

type preexistingTargetKey struct{}

// withPreexistingTarget marks a post-commit whose condensation target already
// had a checkpoint when the hook began.
func withPreexistingTarget(ctx context.Context) context.Context {
	return context.WithValue(ctx, preexistingTargetKey{}, true)
}

// stampedByAnotherCommit reports whether checkpointID is a preexisting
// checkpoint this session did not stamp for this commit: neither the one it is
// amending (LastCheckpointID) nor its pending reservation. Writing there would
// fold new work into another commit's record.
func stampedByAnotherCommit(ctx context.Context, checkpointID id.CheckpointID, state *SessionState) bool {
	if ctx.Value(preexistingTargetKey{}) != true {
		return false
	}
	return checkpointID != state.LastCheckpointID && checkpointID != state.PendingCondensationID()
}

// amendedCheckpoint returns the link to checkpointID, a preexisting target,
// when this commit is an amend: the newest entry in HEAD's reflog that moved
// HEAD to this commit replaced the commit before it instead of advancing from
// it. Entries for other commits are passed over, because a commit started
// while this post-commit runs can land after it read HEAD, and its entry is
// then the newest. An amend keeps the amended commit's trailer, or restores
// its session's (handleAmendCommitMsg), without stamping it, so post-commit
// condenses into the existing checkpoint for the session that owns it
// (withAmendedCheckpoint). A checkpoint the replaced commit does not link, by
// trailer or by the checkpoint's record of it (LinkedCommits, for a stamped
// trailer moved out of the final block), is kept only for an owner prepare
// would have restored it for: `commit --amend -C <other>` keeps another
// commit's trailer, and its owner was never linked to this one. A message
// reused with `commit -C` keeps a trailer the same way but advances HEAD: it
// makes a new link. With no reflog entry for this commit to tell them apart
// (core.logAllRefUpdates=false), it is taken as an amend, so the owner keeps
// condensing into its checkpoint as it did before the idle check. Judged a new
// link instead, the owner would be dropped, and the amend's work would never
// reach the checkpoint its trailer names.
func (s *ManualCommitStrategy) amendedCheckpoint(ctx context.Context, repo *git.Repository, commit *object.Commit, checkpointID id.CheckpointID, preexisting bool) amendedLink {
	if !preexisting {
		return amendedLink{}
	}
	for _, e := range headReflog(ctx) {
		if !e.newHash.Equal(commit.Hash) || e.oldHash.Equal(e.newHash) {
			// A later commit, or an entry that did not move HEAD, as with
			// another hook's `git reset HEAD`.
			continue
		}
		if e.oldHash.IsZero() || slices.ContainsFunc(commit.ParentHashes, e.oldHash.Equal) {
			return amendedLink{}
		}
		link := amendedLink{checkpointID: checkpointID}
		if !s.commitLinksCheckpoint(ctx, repo, e.oldHash, checkpointID) {
			link.ownerBase = e.oldHash.String()
		}
		return link
	}
	return amendedLink{checkpointID: checkpointID}
}

// commitLinksCheckpoint reports whether the commit at hash links checkpointID:
// its final trailer block names it, or the checkpoint records the commit.
func (s *ManualCommitStrategy) commitLinksCheckpoint(ctx context.Context, repo *git.Repository, hash plumbing.Hash, checkpointID id.CheckpointID) bool {
	if c, err := repo.CommitObject(hash); err == nil && slices.Contains(LinkedCheckpoints(ctx, c.Message), checkpointID) {
		return true
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return false
	}
	summary, err := store.Read(ctx, checkpointID)
	return err == nil && summary != nil && slices.ContainsFunc(summary.LinkedCommits, func(l checkpoint.LinkedCommit) bool {
		return l.SHA == hash.String()
	})
}

func checkpointExists(ctx context.Context, store checkpoint.PersistentStore, checkpointID id.CheckpointID) bool {
	summary, err := store.Read(ctx, checkpointID)
	return err == nil && summary != nil
}

// inheritedTrailersFile carries the trailers prepare-commit-msg inherited from
// squashed commits to post-commit. It lives in the per-worktree git dir, like
// SQUASH_MSG, and is tied to the commit's parent so a commit that never
// happened cannot speak for the next one.
//
// Both possible parents are recorded: HEAD for an ordinary commit and HEAD's
// parent for an amend. git's source word cannot tell them apart (`--amend -m`
// reports "message", `-C <rev>` reports "commit"), and post-commit sees the
// real parent. The first is the HEAD prepare ran on (commitParentCandidates).
const inheritedTrailersFile = "entire-inherited-trailers.json"

type inheritedTrailers struct {
	Parents []string          `json:"parents"`
	IDs     []id.CheckpointID `json:"ids"`
	// Redone are the committed paths a redo's inherited checkpoints already
	// hold; post-commit does not count them as a session's overlap evidence.
	Redone []string `json:"redone,omitempty"`
}

// recordedOn returns the HEAD the prepare that recorded marker ran on, empty
// before the first commit.
func (marker inheritedTrailers) recordedOn() string {
	if len(marker.Parents) == 0 {
		return ""
	}
	return marker.Parents[0]
}

// recordInheritedTrailers tells post-commit which trailers this commit
// inherited, so it never condenses into one: an inherited trailer links an
// existing checkpoint, which may simply not be in this clone's store yet.
// With nothing inherited it clears any marker a commit that never happened
// left, but not one HEAD's commit, or a commit below it, may still be waiting
// to take (markerAwaitsCommitOnHead, which also reads HEAD's reflog).
func recordInheritedTrailers(ctx context.Context, inherited []id.CheckpointID) {
	recordInheritedMarker(ctx, inherited, nil)
}

// recordInheritedMarker is recordInheritedTrailers that also records the
// paths a redo's inherited checkpoints hold.
func recordInheritedMarker(ctx context.Context, inherited []id.CheckpointID, redone map[string]bool) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return
	}
	if len(inherited) == 0 {
		if _, err := osroot.LstatNoSymlinks(root, inheritedTrailersFile); err != nil {
			return // absent is the usual case
		}
		chain := headFirstParents(ctx, maxCommitsInFlight)
		reflog := headReflog(ctx)
		release := lockStampRecords(ctx, root)
		defer release()
		if !markerAwaitsCommitOnHead(root, chain, reflog) {
			_ = osroot.RemoveNoSymlinks(root, inheritedTrailersFile) //nolint:errcheck // absent once post-commit took it
		}
		return
	}
	marker := inheritedTrailers{
		IDs:     inherited,
		Parents: commitParentCandidates(ctx),
		Redone:  slices.Sorted(maps.Keys(redone)),
	}
	release := lockStampRecords(ctx, root)
	defer release()
	writeInheritedTrailers(ctx, root, marker)
}

// markerAwaitsCommitOnHead reports whether the inherited marker in root may be
// one that HEAD's commit, or a commit below it, recorded and whose post-commit
// has not taken it yet: git runs post-commit after the commit is made, so the
// next commit's prepare can run first. That is a marker for a commit on one of
// HEAD's ancestors, recorded by a prepare that did not run on HEAD itself,
// with only commits moving HEAD since one was made on the HEAD that prepare
// ran on. A prepare whose commit was cancelled leaves a marker the same shape
// once HEAD is reset back onto a commit on top of it, and an amend of that
// commit would take it. chain is HEAD and its first parents
// (headFirstParents), reflog is HEAD's reflog (headReflog). Callers hold the
// stamp-records lock.
func markerAwaitsCommitOnHead(root *os.Root, chain []string, reflog []reflogEntry) bool {
	if len(chain) < 2 {
		return false
	}
	marker, ok := readInheritedMarkerIn(root)
	if !ok || marker.recordedOn() == chain[0] {
		return false
	}
	onHead := slices.ContainsFunc(marker.Parents, func(parent string) bool {
		return slices.Contains(chain[1:], parent)
	})
	return onHead && committedOnSince(reflog, marker.recordedOn())
}

// committedOnSince reports whether HEAD's reflog, newest first, shows only
// commits since a commit made on base. Resets that did not move HEAD are
// skipped, as replacedCommits skips them. Any other entry, or a reflog that
// does not reach such a commit (none kept, or GIT_REFLOG_ACTION renamed the
// commits), says no, so the marker is cleared as it was before it could wait.
func committedOnSince(reflog []reflogEntry, base string) bool {
	for _, e := range reflog {
		if strings.HasPrefix(e.msg, "reset:") && e.oldHash == e.newHash {
			continue // unstaging or a stash: HEAD did not move
		}
		if !strings.HasPrefix(e.msg, "commit") {
			return false
		}
		if e.oldHash.String() == base {
			return true
		}
	}
	return false
}

func readInheritedMarkerIn(root *os.Root) (inheritedTrailers, bool) {
	var marker inheritedTrailers
	data, err := osroot.ReadFileNoFollow(root, inheritedTrailersFile)
	if err != nil || json.Unmarshal(data, &marker) != nil {
		return inheritedTrailers{}, false
	}
	return marker, true
}

func writeInheritedTrailers(ctx context.Context, root *os.Root, marker inheritedTrailers) {
	data, err := json.Marshal(marker)
	if err != nil {
		return
	}
	if err := jsonutil.WriteFileAtomicIn(root, inheritedTrailersFile, data, 0o600); err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not record inherited trailers",
			slog.String("error", err.Error()))
	}
}

// commitParentCandidates returns the parents the commit being prepared can
// have: HEAD, and HEAD's first parent if it is an amend. Empty before the
// first commit.
func commitParentCandidates(ctx context.Context) []string {
	return headFirstParents(ctx, 2)
}

// maxCommitsInFlight bounds how many commits back from HEAD the stamp records
// look for a commit whose post-commit has not run yet.
const maxCommitsInFlight = 16

// headFirstParents returns HEAD and its first-parent ancestors, newest first,
// at most limit of them. Empty before the first commit.
func headFirstParents(ctx context.Context, limit int) []string {
	chain, _ := walkHeadFirstParents(ctx, limit)
	return chain
}

// walkHeadFirstParents is headFirstParents that also reports whether the walk
// reached a root commit, rather than stopping at limit or at an unreadable
// HEAD or commit.
func walkHeadFirstParents(ctx context.Context, limit int) (chain []string, reachedRoot bool) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, false
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return nil, false
	}
	hash := head.Hash()
	chain = []string{hash.String()}
	for len(chain) < limit {
		commit, err := repo.CommitObject(hash)
		if err != nil {
			return chain, false
		}
		if len(commit.ParentHashes) == 0 {
			return chain, true
		}
		hash = commit.ParentHashes[0]
		chain = append(chain, hash.String())
	}
	return chain, false
}

// inFlight is what a post-commit knows of the commits whose post-commit may
// not have run when its own does (commitsUpToHead).
type inFlight struct {
	// commits are the post-commit's commit and the commits on top of it up to
	// HEAD.
	commits map[string]bool
	// unknown is set when HEAD's first parents could not be followed to the
	// commit or to a root commit: HEAD or a commit could not be read, or the
	// walk ran to its bound. Any commit may then be a later one.
	unknown bool
}

// mayBePending reports whether a record prepared with HEAD at hash may be for
// a commit whose post-commit has not run yet, so the take must leave it: hash
// is the commit or one on top of it, or the walk could not tell.
func (f inFlight) mayBePending(hash string) bool {
	return f.unknown || f.commits[hash]
}

// commitsUpToHead returns commit and the commits on top of it up to HEAD,
// along HEAD's first parents: the commits whose post-commit may not have run
// when commit's does, since git lets the next commit start once one is made.
// Only commit when HEAD's first parents reach a root commit without it (HEAD
// moved elsewhere), and unknown when the walk stops short of both.
func commitsUpToHead(ctx context.Context, commit string) inFlight {
	out := inFlight{commits: map[string]bool{commit: true}}
	chain, reachedRoot := walkHeadFirstParents(ctx, maxCommitsInFlight)
	i := slices.Index(chain, commit)
	switch {
	case i >= 0:
		for _, hash := range chain[:i] {
			out.commits[hash] = true
		}
	case !reachedRoot:
		out.unknown = true
	}
	return out
}

// linkedTrailersOf returns the checkpoint trailers commit links
// (LinkedCheckpoints), plus any trailer prepare-commit-msg stamped that a
// paragraph added below it (an editor, a commit-msg hook) moved out of the
// final trailer block, leaving out the ones prepare recorded as inherited and any
// checkpoint deleted from this clone: post-commit must never condense new work
// into a deleted ID, which would re-create it. records is what prepare
// recorded for commit (takeStampRecords). It also returns the stamped
// trailers found outside the final trailer block.
func linkedTrailersOf(ctx context.Context, commit *object.Commit, records stampRecords) (ids, moved []id.CheckpointID) {
	linked := LinkedCheckpoints(ctx, commit.Message)
	moved = stampedOutsideFinalBlock(linked, commit.Message, records.stamped)
	ids = withoutInherited(append(slices.Clone(moved), linked...), records.inherited)
	return withoutDeletedCheckpoints(ctx, ids), moved
}

// stampedOutsideFinalBlock returns the stamped trailers message names outside
// its final trailer block (linked). The stamp record, not the trailer's
// position, proves the ID is this commit's. Callers put them before linked, so
// a stamped trailer in the final trailer block stays the condensation target.
func stampedOutsideFinalBlock(linked []id.CheckpointID, message string, stamped map[id.CheckpointID]bool) []id.CheckpointID {
	var moved []id.CheckpointID
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		if stamped[cpID] && !slices.Contains(linked, cpID) {
			moved = append(moved, cpID)
		}
	}
	return moved
}

type recordedCommitLinkKey struct{}

// withRecordedCommitLink marks a post-commit whose condensation target is a
// stamped trailer outside the commit's final trailer block. Commands that
// write into a commit's checkpoint read only that block, so the checkpoint
// records the commit (LinkedCommits) for them to find it by, as attach does
// for a pushed commit it links without a trailer.
func withRecordedCommitLink(ctx context.Context, commitSHA string) context.Context {
	return context.WithValue(ctx, recordedCommitLinkKey{}, commitSHA)
}

// recordedCommitLink returns the commit withRecordedCommitLink marked.
func recordedCommitLink(ctx context.Context) (string, bool) {
	sha, ok := ctx.Value(recordedCommitLinkKey{}).(string)
	return sha, ok && sha != ""
}

// stampedTrailersFile lists the trailers prepare-commit-msg stamped that no
// post-commit has taken yet: IDs it generated or took from a session's
// reservation. An amend's restored trailer names an existing checkpoint and is
// not listed. Post-commit may create the checkpoint of only these
// (pickCondensationTarget), and takes the ones its commit carries
// (takeStampRecords). Per worktree, like inheritedTrailersFile. An entry a
// commit that never happened left stays until the next post-commit, or the
// one after when it was stamped on top of that post-commit's commit; it names
// a checkpoint only this clone minted and nothing holds, so no commit can
// steal it.
const stampedTrailersFile = "entire-stamped-trailers.json"

// recordStampedTrailer adds cpID to the trailers prepare-commit-msg stamped,
// with what prepare linked it to (stampedSessionsFile), and reports whether it
// did. Prepare writes a trailer only once its stamp is recorded: post-commit
// would never create the checkpoint of an unrecorded one, so the commit would
// name a checkpoint that does not exist.
func recordStampedTrailer(ctx context.Context, cpID id.CheckpointID) bool {
	err := writeStampRecords(ctx, stampedSessions{
		CheckpointID:      cpID,
		SessionIDs:        linkedSessions(ctx),
		TrustedSessionIDs: trustedSessions(ctx),
		Head:              headHash(ctx),
	})
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not record the stamped trailer, leaving the commit unlinked",
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// stampedSessionsFile records, for each trailer prepare-commit-msg stamped,
// the sessions it linked the commit to (withLinkedSessions) and the HEAD it
// ran on. Post-commit keeps those sessions in its own matching, so it
// condenses into the trailer the sessions prepare linked. It sits beside
// stampedTrailersFile, not in it, so a binary that reads that file as a list of
// IDs is unaffected. Prepare writes both under the stamp-records lock and
// post-commit takes both under it, so a post-commit that finds its stamp finds
// the entry too, however another commit's hooks overlap it. With no entry for
// its trailer (a stamp an older binary wrote), post-commit matches afresh, as
// before. The entry also names the linked sessions prepare stamped on their
// shape alone (withTrustedSessions), which post-commit condenses without
// overlap evidence however they have settled since.
const stampedSessionsFile = "entire-stamped-sessions.json"

type stampedSessions struct {
	CheckpointID      id.CheckpointID `json:"checkpoint_id"`
	SessionIDs        []string        `json:"session_ids"`
	TrustedSessionIDs []string        `json:"trusted_session_ids,omitempty"`
	// Head is the commit HEAD named when prepare stamped the trailer, empty
	// before the first commit or from a binary that did not record it. A
	// post-commit keeps the stamps prepared on its own commit or on one made on
	// top of it since (takeStampRecords).
	Head string `json:"head,omitempty"`
}

// writeStampRecords records entry's trailer as stamped, and entry with it,
// replacing an earlier entry for the trailer (a reservation stamped again by a
// later prepare). Both must land: the entry's HEAD is what keeps the stamp
// when an earlier commit's post-commit takes the records first, so a stamp
// without it can be dropped before its own post-commit, leaving the commit a
// trailer for a checkpoint that is never written. The entry is written first,
// and restored if the stamp then fails; an entry without a stamp is dropped by
// the next post-commit.
func writeStampRecords(ctx context.Context, entry stampedSessions) error {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return err
	}
	release := lockStampRecords(ctx, root)
	defer release()
	before := readStampedSessionsIn(root)
	entries := slices.DeleteFunc(slices.Clone(before), func(e stampedSessions) bool {
		return e.CheckpointID == entry.CheckpointID
	})
	if err := writeStampedSessionsIn(root, append(entries, entry)); err != nil {
		return fmt.Errorf("record the stamp's sessions entry: %w", err)
	}
	if err := writeStampedTrailerIn(root, entry.CheckpointID); err != nil {
		if restoreErr := writeStampedSessionsIn(root, before); restoreErr != nil {
			logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not restore the stamped sessions",
				slog.String("error", restoreErr.Error()))
		}
		return err
	}
	return nil
}

// headHash returns the commit HEAD names, empty before the first commit.
func headHash(ctx context.Context) string {
	if candidates := commitParentCandidates(ctx); len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func writeStampedSessionsIn(root *os.Root, entries []stampedSessions) error {
	if len(entries) == 0 {
		return removeRecord(root, stampedSessionsFile)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode stamped sessions: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, stampedSessionsFile, data, 0o600) //nolint:wrapcheck // callers log it with context
}

// removeRecord removes one of the stamp records; an absent one is already gone.
func removeRecord(root *os.Root, name string) error {
	if err := osroot.RemoveNoSymlinks(root, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err //nolint:wrapcheck // callers log it with context
	}
	return nil
}

func readStampedSessionsIn(root *os.Root) []stampedSessions {
	data, err := osroot.ReadFileNoFollow(root, stampedSessionsFile)
	if err != nil {
		return nil
	}
	var entries []stampedSessions
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	return entries
}

func writeStampedTrailerIn(root *os.Root, cpID id.CheckpointID) error {
	stamped := readStampedTrailersIn(root)
	if slices.Contains(stamped, cpID) {
		return nil
	}
	return writeStampedTrailersIn(root, append(stamped, cpID))
}

func writeStampedTrailersIn(root *os.Root, stamped []id.CheckpointID) error {
	if len(stamped) == 0 {
		return removeRecord(root, stampedTrailersFile)
	}
	data, err := json.Marshal(stamped)
	if err != nil {
		return fmt.Errorf("encode stamped trailers: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, stampedTrailersFile, data, 0o600) //nolint:wrapcheck // callers log it with context
}

// readStampedTrailers returns the trailers prepare-commit-msg stamped that no
// post-commit has taken yet.
func readStampedTrailers(ctx context.Context) map[id.CheckpointID]bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	return checkpointSet(readStampedTrailersIn(root))
}

func readStampedTrailersIn(root *os.Root) []id.CheckpointID {
	data, err := osroot.ReadFileNoFollow(root, stampedTrailersFile)
	if err != nil {
		return nil
	}
	var stamped []id.CheckpointID
	if json.Unmarshal(data, &stamped) != nil {
		return nil
	}
	return stamped
}

func checkpointSet(ids []id.CheckpointID) map[id.CheckpointID]bool {
	set := make(map[id.CheckpointID]bool, len(ids))
	for _, cpID := range ids {
		set[cpID] = true
	}
	return set
}

// stampRecordsLockName serializes the records prepare-commit-msg leaves for
// post-commit in the per-worktree git dir (stampedTrailersFile,
// stampedSessionsFile, inheritedTrailersFile). git releases the index lock
// before it runs prepare-commit-msg for a plain commit, and before
// post-commit for every commit, so in one worktree another commit's prepare
// can run while a post-commit takes the records, and two post-commits can
// overlap. Unserialized, either can lose an update the other made.
const stampRecordsLockName = "entire-stamp-records.lock"

// stampRecordsLockWait bounds the wait for the lock. A holder only reads and
// writes the small records, so a longer wait means the holder is stuck.
const stampRecordsLockWait = 2 * time.Second

// lockStampRecords takes the stamp-records lock in root, the per-worktree git
// dir, and returns its release. When the lock cannot be taken the hook goes
// on unserialized, as before the lock existed: a race can then lose a record,
// while giving up would lose this hook's records every time.
func lockStampRecords(ctx context.Context, root *os.Root) func() {
	waitCtx, cancel := context.WithTimeout(ctx, stampRecordsLockWait)
	defer cancel()
	release, err := flock.AcquireContextIn(waitCtx, root, stampRecordsLockName)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "could not lock the commit stamp records; going on unserialized",
			slog.String("error", err.Error()))
		return func() {}
	}
	return release
}

// stampRecords is what prepare-commit-msg recorded for one commit
// (takeStampRecords).
type stampRecords struct {
	// stamped are the trailers the commit carries that prepare stamped (see
	// pickCondensationTarget).
	stamped map[id.CheckpointID]bool
	// sessions is what prepare recorded with each trailer the commit carries,
	// by checkpoint ID.
	sessions map[id.CheckpointID]stampedSessions
	// inherited are the trailers prepare recorded as inherited, and redone
	// the committed paths a redo's inherited checkpoints hold.
	inherited map[id.CheckpointID]bool
	redone    map[string]bool
}

// takeStampRecords returns, for post-commit, what prepare-commit-msg recorded
// for commit, and consumes it. It holds the stamp-records lock throughout, so
// it takes every record in one step that a racing prepare or post-commit
// cannot split.
//
// It consumes only what is the commit's. git lets the next commit start once
// this one is made, before its post-commit runs, so later commits' prepares
// may already have stamped, and those commits may have landed: a stamp the
// commit does not carry is kept when it was stamped on commit or on a commit
// on top of it up to HEAD (commitsUpToHead), for the post-commit of the commit
// that carries it. Any other is one a commit that never happened left, and is
// dropped. In the same way, an inherited marker for another parent is dropped
// unless a prepare on one of those commits recorded it.
func takeStampRecords(ctx context.Context, commit *object.Commit) stampRecords {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return stampRecords{}
	}
	head := commit.Hash.String()
	var parent string
	if len(commit.ParentHashes) > 0 {
		parent = commit.ParentHashes[0].String()
	}
	carried := checkpointSet(trailers.ParseAllCheckpoints(commit.Message))

	release := lockStampRecords(ctx, root)
	defer release()
	// Read HEAD under the lock: a prepare whose records are here read HEAD
	// before writing them, so the commit it stamped on has landed by now.
	pending := commitsUpToHead(ctx, head)
	var out stampRecords
	out.inherited, out.redone = takeInheritedMarkerIn(ctx, root, parent, pending)
	out.stamped, out.sessions = takeStampsIn(ctx, root, carried, pending)
	return out
}

// takeStampsIn takes the stamps and sessions entries of the trailers carried,
// keeps the ones that may be stamped on a commit in pending, and drops the
// rest. Callers
// hold the stamp-records lock.
func takeStampsIn(ctx context.Context, root *os.Root, carried map[id.CheckpointID]bool, pending inFlight) (map[id.CheckpointID]bool, map[id.CheckpointID]stampedSessions) {
	entries := readStampedSessionsIn(root)
	stampedOn := make(map[id.CheckpointID]string, len(entries))
	for _, e := range entries {
		stampedOn[e.CheckpointID] = e.Head
	}
	stamped := make(map[id.CheckpointID]bool)
	var kept []id.CheckpointID
	for _, cpID := range readStampedTrailersIn(root) {
		switch {
		case carried[cpID]:
			stamped[cpID] = true
		case pending.mayBePending(stampedOn[cpID]):
			kept = append(kept, cpID)
		}
	}
	sessions := make(map[id.CheckpointID]stampedSessions)
	var keptEntries []stampedSessions
	for _, e := range entries {
		switch {
		case carried[e.CheckpointID]:
			sessions[e.CheckpointID] = e
		case slices.Contains(kept, e.CheckpointID):
			keptEntries = append(keptEntries, e)
		}
	}

	logCtx := logging.WithComponent(ctx, "checkpoint")
	if err := writeStampedSessionsIn(root, keptEntries); err != nil {
		logging.Debug(logCtx, "post-commit: could not rewrite the stamped sessions", slog.String("error", err.Error()))
	}
	if err := writeStampedTrailersIn(root, kept); err != nil {
		logging.Debug(logCtx, "post-commit: could not rewrite the stamped trailers", slog.String("error", err.Error()))
	}
	return stamped, sessions
}

// takeInheritedMarkerIn returns the trailers prepare-commit-msg recorded as
// inherited for a commit on parent, with the recorded redone paths, and
// consumes the marker unless a prepare on a commit in pending recorded it:
// when pending is unknown, it leaves any marker that is not for parent.
// Callers hold the stamp-records lock.
func takeInheritedMarkerIn(ctx context.Context, root *os.Root, parent string, pending inFlight) (map[id.CheckpointID]bool, map[string]bool) {
	if _, err := osroot.LstatNoSymlinks(root, inheritedTrailersFile); err != nil {
		return nil, nil
	}
	marker, ok := readInheritedMarkerIn(root)
	mine := ok && !pending.commits[marker.recordedOn()] && slices.Contains(marker.Parents, parent)
	if ok && !mine && pending.mayBePending(marker.recordedOn()) {
		return nil, nil
	}
	if err := removeRecord(root, inheritedTrailersFile); err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "post-commit: could not remove the inherited trailers marker",
			slog.String("error", err.Error()))
	}
	if !mine {
		return nil, nil
	}
	out := checkpointSet(marker.IDs)
	var redone map[string]bool
	for _, p := range marker.Redone {
		if redone == nil {
			redone = make(map[string]bool, len(marker.Redone))
		}
		redone[p] = true
	}
	return out, redone
}

// withoutInherited drops the trailers a commit inherited: post-commit condenses
// only into what prepare stamped.
func withoutInherited(ids []id.CheckpointID, inherited map[id.CheckpointID]bool) []id.CheckpointID {
	if len(inherited) == 0 {
		return ids
	}
	out := make([]id.CheckpointID, 0, len(ids))
	for _, cpID := range ids {
		if !inherited[cpID] {
			out = append(out, cpID)
		}
	}
	return out
}

// perWorktreeGitRoot opens the per-worktree git dir, where SQUASH_MSG and the
// sequencer markers live. The root is the shared registry handle: never close it.
func perWorktreeGitRoot(ctx context.Context) (*os.Root, error) {
	gitDir, err := GetGitDir(ctx)
	if err != nil {
		return nil, err
	}
	return gitdir.OpenAt(gitDir) //nolint:wrapcheck // callers treat any error as "no marker"
}
