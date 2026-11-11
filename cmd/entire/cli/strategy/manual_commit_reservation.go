package strategy

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// A session's reserved checkpoint ID (PendingCondensationID) goes to one
// commit. git lets the next commit start once one is made, before its
// post-commit runs, and the session holds the reservation until that
// post-commit condenses it. So prepare passes over a reservation a commit
// already made carries (stampCarriedInFlight), and the later commit's
// post-commit, which may reach the session first, waits for the earlier one
// to condense it (awaitEarlierCondensations).

// takenEntryLife bounds how long the post-commit that took a stamp counts as
// running when proclive cannot tell whether its process is alive (on Windows,
// or for a record written on another host). A post-commit that runs longer
// loses its claim on the reservation, as if it had stopped.
const takenEntryLife = time.Minute

// earlierCondensationWait bounds how long post-commit waits for another
// commit's post-commit to condense a session (awaitEarlierCondensations). It
// is a variable so tests can shorten it.
var earlierCondensationWait = 10 * time.Second

// earlierCondensationPoll is how often that wait looks again.
const earlierCondensationPoll = 50 * time.Millisecond

// stampTaker is the post-commit that took a stamp (takeStampsIn): its
// process, where proclive can fingerprint it, and when it took the stamp.
type stampTaker struct {
	Process *proclive.Identity `json:"process,omitempty"`
	At      time.Time          `json:"at"`
}

// currentStampTaker is this process, taking stamps at now.
func currentStampTaker(now time.Time) *stampTaker {
	taker := &stampTaker{At: now}
	if self, ok := proclive.IdentityOf(os.Getpid()); ok {
		taker.Process = &self
	}
	return taker
}

// running reports whether the post-commit that took the stamp may still be
// running at now: its process is alive or, when proclive cannot tell, it took
// the stamp less than takenEntryLife ago. A nil taker took nothing.
func (t *stampTaker) running(now time.Time) bool {
	if t == nil {
		return false
	}
	if t.Process != nil {
		switch proclive.Check(*t.Process) {
		case proclive.LivenessAlive:
			return true
		case proclive.LivenessDead:
			return false
		case proclive.LivenessUnknown:
		}
	}
	return now.Sub(t.At) < takenEntryLife
}

// stampedSessionsEntry returns the sessions entry for cpID among entries.
func stampedSessionsEntry(entries []stampedSessions, cpID id.CheckpointID) (stampedSessions, bool) {
	i := slices.IndexFunc(entries, func(e stampedSessions) bool { return e.CheckpointID == cpID })
	if i < 0 {
		return stampedSessions{}, false
	}
	return entries[i], true
}

// stampCarriedInFlight reports whether a commit already made carries cpID, a
// checkpoint ID prepare stamped, while its post-commit may not be done with
// it yet. A commit prepared in between must not be given the same ID, as a
// session's reservation (freeReservation) or as a stamp its message already
// names (stampedTrailer): it would name the other commit's checkpoint, and its
// post-commit would write its own work over that checkpoint.
//
// Prepare recorded stamping cpID (stampedSessionsFile), and no post-commit
// has released the entry (releaseStampedSessions) or stopped running after
// taking it; a commit on HEAD's first parents made since the HEAD that prepare
// stamped on carries cpID. A stamp prepared on HEAD itself, as when a commit
// is tried again after its first attempt was aborted, is the same commit's,
// so the retry keeps it. Once the post-commit is done, a session that still
// holds the reservation was not condensed into it, and the next commit takes
// it, as before.
func stampCarriedInFlight(ctx context.Context, cpID id.CheckpointID) bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return false
	}
	entry, ok := stampedSessionsEntry(readStampedSessionsIn(root), cpID)
	if !ok || (entry.Taken != nil && !entry.Taken.running(time.Now())) {
		return false
	}
	return commitSinceCarries(ctx, entry.Head, cpID)
}

// commitSinceCarries reports whether a commit on HEAD's first parents, made
// after stampedOn, carries cpID (carriedSince). An unreadable HEAD ends the
// search.
func commitSinceCarries(ctx context.Context, stampedOn string, cpID id.CheckpointID) bool {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return false
	}
	return carriedSince(repo, head.Hash(), stampedOn, cpID)
}

// carriedBefore reports whether a commit before commit on its first parents,
// made after stampedOn, carries cpID (carriedSince): the stamp is that
// earlier commit's, though commit carries it too, as a message saved from the
// earlier commit does.
func carriedBefore(ctx context.Context, commit *object.Commit, stampedOn string, cpID id.CheckpointID) bool {
	if len(commit.ParentHashes) == 0 {
		return false
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false
	}
	defer repo.Close()
	return carriedSince(repo, commit.ParentHashes[0], stampedOn, cpID)
}

// carriedSince reports whether from or a commit on its first parents, made
// after stampedOn (any, when it is empty), carries cpID. It looks at most
// maxCommitsInFlight commits back, and an unreadable commit ends the search.
func carriedSince(repo *git.Repository, from plumbing.Hash, stampedOn string, cpID id.CheckpointID) bool {
	hash := from
	for range maxCommitsInFlight {
		if hash.String() == stampedOn {
			return false
		}
		commit, err := repo.CommitObject(hash)
		if err != nil {
			return false
		}
		if slices.Contains(trailers.ParseAllCheckpoints(commit.Message), cpID) {
			return true
		}
		if len(commit.ParentHashes) == 0 {
			return false
		}
		hash = commit.ParentHashes[0]
	}
	return false
}

// freeReservation returns state's reserved checkpoint ID when this commit may
// take it, or the empty ID. It reads the reservation again after checking
// that no commit carries it: state is the snapshot prepare loaded, and the
// earlier commit's post-commit may have condensed the session into it and
// released its entry since, which the check alone cannot tell from a
// reservation no commit has carried.
func freeReservation(ctx context.Context, state *SessionState) id.CheckpointID {
	reserved := state.PendingCondensationID()
	if reserved.IsEmpty() {
		return id.EmptyCheckpointID
	}
	if stampCarriedInFlight(ctx, reserved) {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: reserved checkpoint ID already carried by a commit awaiting its post-commit",
			slog.String("session_id", state.SessionID),
			slog.String("reserved_checkpoint_id", reserved.String()))
		return id.EmptyCheckpointID
	}
	if now, ok := savedReservation(ctx, state.SessionID); ok && now != reserved {
		return id.EmptyCheckpointID
	}
	return reserved
}

// savedReservation returns the checkpoint ID sessionID's saved state reserves
// now, and whether that state could be read.
func savedReservation(ctx context.Context, sessionID string) (id.CheckpointID, bool) {
	state, err := LoadSessionState(ctx, sessionID)
	if err != nil || state == nil {
		return id.EmptyCheckpointID, false
	}
	return state.PendingCondensationID(), true
}

// awaitEarlierCondensations waits while a session about to be condensed into
// cpID, commit's checkpoint, still reserves another checkpoint ID that an
// earlier commit carries (carriedByEarlierCommit: one before commit on its
// first parents, or the commit an amend replaced) whose post-commit took the
// stamp and is still running. The two post-commits run
// side by side, and this one can reach the session first. It would skip the
// session then (postCommitProcessSessionLocked keeps a reservation for its own
// checkpoint), and with no other session condensing into cpID, the commit
// would name a checkpoint that is never written. It never waits for a later
// commit's reservation, so two post-commits never wait for each other. The
// wait ends once the session no longer holds the reservation, the other
// post-commit releases its entry or stops running, or earlierCondensationWait
// has passed. It reports whether it waited, so the caller reads the sessions
// again.
func awaitEarlierCondensations(ctx context.Context, sessions []*SessionState, commit *object.Commit, cpID id.CheckpointID) bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return false
	}
	deadline := time.Now().Add(earlierCondensationWait)
	waited := false
	for _, sess := range sessions {
		reserved := sess.PendingCondensationID()
		if reserved.IsEmpty() || reserved == cpID || !carriedByEarlierCommit(ctx, root, commit, reserved) {
			continue
		}
		for condensingElsewhere(ctx, root, sess.SessionID, reserved) {
			if !time.Now().Before(deadline) {
				logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: another commit's post-commit has not condensed the session into its checkpoint yet; going on without it",
					slog.String("session_id", sess.SessionID),
					slog.String("reserved_checkpoint_id", reserved.String()),
					slog.String("checkpoint_id", cpID.String()))
				return waited
			}
			waited = true
			select {
			case <-ctx.Done():
				return waited
			case <-time.After(earlierCondensationPoll):
			}
		}
	}
	return waited
}

// carriedByEarlierCommit reports whether prepare recorded stamping reserved,
// and a commit made since that commit comes after carries it: one before
// commit on its first parents (carriedBefore) or, when commit is an amend, the
// commit it replaced or one before that (carriedByReplaced).
func carriedByEarlierCommit(ctx context.Context, root *os.Root, commit *object.Commit, reserved id.CheckpointID) bool {
	entry, ok := stampedSessionsEntry(readStampedSessionsIn(root), reserved)
	return ok && (carriedBefore(ctx, commit, entry.Head, reserved) || carriedByReplaced(ctx, commit, entry.Head, reserved))
}

// carriedByReplaced reports whether commit is an amend (commitReplaced) and
// the commit it replaced, or one before that on its first parents made after
// stampedOn, carries cpID (carriedSince). An amend does not descend from the
// commit it replaces, so carriedBefore does not reach it. With no reflog to
// tell, commit is not taken as an amend.
func carriedByReplaced(ctx context.Context, commit *object.Commit, stampedOn string, cpID id.CheckpointID) bool {
	replaced, _ := commitReplaced(ctx, commit)
	if replaced.IsZero() {
		return false
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false
	}
	defer repo.Close()
	return carriedSince(repo, replaced, stampedOn, cpID)
}

// condensingElsewhere reports whether sessionID's saved state still reserves
// reserved, and the post-commit that took reserved's stamp is still running.
func condensingElsewhere(ctx context.Context, root *os.Root, sessionID string, reserved id.CheckpointID) bool {
	entry, ok := stampedSessionsEntry(readStampedSessionsIn(root), reserved)
	if !ok || !entry.Taken.running(time.Now()) {
		return false
	}
	now, read := savedReservation(ctx, sessionID)
	return read && now == reserved
}
