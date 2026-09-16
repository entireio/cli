package strategy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// pushedDestinationFileName records where checkpoints were last delivered, in
// the git common dir beside the captured election and the push queue so every
// worktree of a clone shares one answer.
//
// Written and read only by the git-refs flush: it is the memory the push queue
// lacks. git-branch keeps no equivalent because it needs none — see
// resyncCheckpointRefsOnDestinationChange.
const pushedDestinationFileName = "entire-checkpoint-destination.json"

// pushedDestinationFile stores a FINGERPRINT rather than the destination
// itself. The file answers one question — "is this the same place as last
// time" — and a hash answers it exactly, so the destination never has to be
// written down: which repository a developer sends transcripts to is worth not
// recording in the clear, and a target that did arrive carrying credentials
// (git hands a pre-push hook whatever URL it was invoked with) leaves none
// here.
//
// NOT because the derived checkpoint URLs embed a token. deriveTokenOriginURL
// and deriveCheckpointURLFromInfo both build a plain
// https://host/owner/repo.git; an earlier version of this comment cited them
// and was wrong.
type pushedDestinationFile struct {
	Fingerprint string `json:"fingerprint"`
}

func destinationFingerprint(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])
}

// loadPushedDestination reads the last delivered destination's fingerprint.
// Fail-soft, like the captured election: a missing, unreadable or corrupt file
// reads as "unknown", which suppresses the re-sync rather than forcing one. An
// unnecessary re-sync is a large push, so the ambiguous case does nothing.
func loadPushedDestination(ctx context.Context) string {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return ""
	}
	data, err := osroot.ReadFileNoFollow(root, pushedDestinationFileName)
	if err != nil {
		return ""
	}
	var f pushedDestinationFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.Fingerprint
}

// recordPushedDestination stores where this push delivered. Best-effort: losing
// the record costs at most one redundant re-sync later, which is idempotent, so
// it must never fail a push that already succeeded.
func recordPushedDestination(ctx context.Context, target string) {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return
	}
	data, err := json.Marshal(pushedDestinationFile{Fingerprint: destinationFingerprint(target)})
	if err != nil {
		return
	}
	if err := jsonutil.WriteFileAtomicIn(root, pushedDestinationFileName, data, 0o600); err != nil {
		logging.Warn(ctx, "checkpoint destination: recording the delivered destination failed",
			slog.String("error", err.Error()))
	}
}

// resyncCheckpointRefsOnDestinationChange re-queues every local checkpoint ref
// when the destination has changed since the last delivery.
//
// The queue is emptied by a successful push, so without this a checkpoint
// delivered to one destination is never offered to the next one: resolving a
// misrouted checkpoint_remote would fix future checkpoints and strand every
// earlier one where it landed. Re-queueing is safe because a ref already
// present on the destination pushes as a no-op.
//
// git-branch needs no equivalent, because its push sends the whole
// entire/checkpoints/v1 branch. That is a property of pushRefIfNeeded rather
// than an assumption, so TestV1BranchCarriesItsWholeHistoryToANewDestination
// pins it: the "has unpushed" shortcut there consults a remote-tracking ref,
// and a remote-agnostic one would strand the history exactly as the emptied
// queue does here.
//
// Two suppressions. Nothing recorded means a first push, not a change, so a
// fresh clone does not offer its whole history unasked. And the key is the
// push target as the caller resolved it — an elected remote's NAME, or a
// checkpoint_remote URL — which is what makes the check cost nothing on an
// ordinary push; repointing an elected remote's URL therefore reads as the
// same destination, since "origin" is still "origin". Seeing that would mean
// resolving the remote's push URL on every pre-push, and the misrouted
// checkpoint_remote this exists for changes the target string itself.
//
// Every local checkpoint ref, including ones fetched from the store being left
// — the same history a git-branch push would carry, for the same reason.
//
// Reports whether the destination may now be recorded as delivered. A re-sync
// that did not finish — the queue would not resolve, the refs would not
// enumerate, an enqueue failed part way — must NOT be followed by a record:
// recording makes the stored fingerprint equal the current destination, so the
// refs that never reached the queue would never be noticed again. Leaving the
// old fingerprint costs a repeated re-sync on the next push, which is
// idempotent; recording early strands them for good.
func resyncCheckpointRefsOnDestinationChange(ctx context.Context, repo *git.Repository, target string) (mayRecord bool) {
	previous := loadPushedDestination(ctx)
	if previous == "" || previous == destinationFingerprint(target) {
		return true
	}
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint destination: resolve push queue for re-sync failed",
			slog.String("error", err.Error()))
		return false
	}
	refs, err := localCheckpointRefs(repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint destination: enumerate local checkpoint refs failed",
			slog.String("error", err.Error()))
		return false
	}
	for i, ref := range refs {
		if err := queue.Enqueue(ref); err != nil {
			logging.Warn(ctx, "checkpoint destination: re-queue failed part way; leaving the destination unrecorded so the rest are retried",
				slog.String("ref", ref.String()),
				slog.Int("queued", i),
				slog.Int("refs", len(refs)),
				slog.String("error", err.Error()))
			return false
		}
	}
	logging.Info(ctx, "checkpoint destination changed; re-queued checkpoints for the new destination",
		slog.Int("refs", len(refs)))
	return true
}

// localCheckpointRefs lists every checkpoint ref held locally.
func localCheckpointRefs(repo *git.Repository) ([]plumbing.ReferenceName, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, err //nolint:wrapcheck // caller logs it with its own context
	}
	defer iter.Close()
	var refs []plumbing.ReferenceName
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if _, ok := checkpoint.ParseRef(ref.Name()); ok {
			refs = append(refs, ref.Name())
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // caller logs it with its own context
	}
	return refs, nil
}
