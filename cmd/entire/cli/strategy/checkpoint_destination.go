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
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
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

// pushedDestinationLockName serializes the read-decide-write, pairing with the
// state file exactly as checkpoint_sync_capture.go and checkpoint/pushqueue.go
// pair theirs in this same directory.
const pushedDestinationLockName = "entire-checkpoint-destination.lock"

// pushedDestinationFile stores a FINGERPRINT rather than the destination: the
// file only answers "same place as last time", and where a developer sends
// transcripts is worth not recording in the clear.
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
	release, err := flock.AcquireIn(root, pushedDestinationLockName)
	if err != nil {
		logging.Debug(ctx, "checkpoint destination: cannot acquire lock to record",
			slog.String("error", err.Error()))
		return
	}
	defer release()
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
// git-branch needs no equivalent: its push sends the whole
// entire/checkpoints/v1 branch. That is a property of pushRefIfNeeded rather
// than an assumption, so TestV1BranchCarriesItsWholeHistoryToANewDestination
// pins it.
//
// Two suppressions. Nothing recorded means a first push, not a change, so a
// fresh clone does not offer its whole history unasked. And the key is the push
// target as the caller resolved it, which is what makes the check free on an
// ordinary push — so repointing an elected remote's URL reads as the same
// destination.
//
// Reports whether the destination may now be recorded. A re-sync that did not
// finish must NOT be followed by a record: the stored fingerprint would equal
// the current destination and the refs that never reached the queue would never
// be noticed again.
func resyncCheckpointRefsOnDestinationChange(ctx context.Context, repo *git.Repository, target string) (mayRecord bool) {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return true
	}
	release, err := flock.AcquireIn(root, pushedDestinationLockName)
	if err != nil {
		logging.Debug(ctx, "checkpoint destination: cannot acquire lock to re-sync",
			slog.String("error", err.Error()))
		return true
	}
	defer release()

	previous := loadPushedDestination(ctx)
	if previous == "" || previous == destinationFingerprint(target) {
		return true
	}
	queue, qErr := checkpoint.PushQueueForRepo(ctx, repo)
	if qErr != nil {
		logging.Warn(ctx, "checkpoint destination: resolve push queue for re-sync failed",
			slog.String("error", qErr.Error()))
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
