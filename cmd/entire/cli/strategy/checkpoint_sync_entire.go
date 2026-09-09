package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// The entire tier of the checkpoint sync election (SyncRemoteSourceEntire)
// changes what a pre-push does. Under every other tier a push carries
// checkpoints only when it names the elected remote; under this one the
// elected remote is Entire's own store, so EVERY push — to any remote, or to a
// raw URL — carries the checkpoints to the Entire remote, the way the dedicated
// checkpoint_remote URL mode already bypasses the single-remote gate. The gate
// exists to keep transcripts off remotes the user did not choose for them; an
// Entire remote is not that surface. Without the redirect, a user whose habit
// is `git push origin` (GitHub) would strand checkpoints locally forever once
// the tier flipped the election.
//
// This file holds the redirect, and the per-clone state that makes the first
// delivery announce itself exactly once and records whether the checkpoint
// migration command has moved the backlog over.

// redirectToEntireSyncRemote applies the entire-tier push rule to ps: when no
// dedicated checkpoint URL is in play and the election elected an entire://
// remote other than the one being pushed, the checkpoint target becomes that
// remote (ps.syncRemote). It returns the election and entireTier=true whenever
// the tier is in force — redirected or not (a push straight to the Entire
// remote needs no redirect) — so the caller skips the single-remote gate and
// announces delivery. entireTier=false leaves ps untouched and the ordinary
// gate applies.
//
// A raw-URL push is redirected too. The gate blocks raw-URL pushes because git
// hands the hook the URL verbatim and no election can vouch for it; here the
// destination is the elected Entire remote regardless of where the code went,
// so there is nothing to vouch for.
func redirectToEntireSyncRemote(ctx context.Context, ps *pushSettings) (elected CheckpointSyncRemote, entireTier bool) {
	if ps.hasCheckpointURL() {
		return CheckpointSyncRemote{}, false
	}
	elected, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil || elected.Source != SyncRemoteSourceEntire {
		return CheckpointSyncRemote{}, false
	}
	if elected.Name != ps.remote {
		ps.syncRemote = elected.Name
		logging.Debug(ctx, "checkpoint push redirected to the Entire remote",
			slog.String("push_remote", ps.remote),
			slog.String("checkpoint_sync_remote", elected.Name))
	}
	return elected, true
}

// entireSyncStateFileName is the per-clone entire-tier state, in the git
// common dir (worktree-shared, like the captured election and the push queue).
const entireSyncStateFileName = "entire-checkpoint-sync-entire.json"

// entireSyncStateLockName serializes the read-modify-write of the state file:
// two pre-push hooks in different worktrees must not both observe "not yet
// announced" and announce twice.
const entireSyncStateLockName = "entire-checkpoint-sync-entire.lock"

// EntireSyncMigration records whether the checkpoint migration command has dealt
// with the checkpoints that predate the Entire remote.
type EntireSyncMigration string

const (
	// EntireSyncMigrationNone: not yet run (or nothing recorded).
	EntireSyncMigrationNone EntireSyncMigration = ""
	// EntireSyncMigrationDone: the backlog was moved to the Entire remote.
	EntireSyncMigrationDone EntireSyncMigration = "done"
	// EntireSyncMigrationDeclined: the user chose to leave the old copies where
	// they are; reminders stop.
	EntireSyncMigrationDeclined EntireSyncMigration = "declined"
)

// EntireSyncState is the per-clone state of the entire-tier election.
type EntireSyncState struct {
	// Remote is the Entire remote checkpoints were first delivered to.
	Remote string `json:"remote"`
	// AnnouncedAt is when the first delivery was announced; zero until then.
	AnnouncedAt time.Time `json:"announced_at,omitempty"`
	// Migration is the checkpoint migration command's verdict on the pre-Entire backlog.
	Migration EntireSyncMigration `json:"migration,omitempty"`
	// MigratedFrom names the remote the backlog was moved from (or left on).
	MigratedFrom string `json:"migrated_from,omitempty"`
	// MigratedAt is when Migration was recorded.
	MigratedAt time.Time `json:"migrated_at,omitempty"`
}

// LoadEntireSyncState reads the state. Fail-soft: a missing, unreadable, or
// corrupt file reads as the zero state with ok=false — this is automatic
// bookkeeping, and nothing that reads it may fail closed on it.
func LoadEntireSyncState(ctx context.Context) (EntireSyncState, bool) {
	root, err := capturedSyncRemotesRoot(ctx)
	if err != nil {
		return EntireSyncState{}, false
	}
	data, err := osroot.ReadFileNoFollow(root, entireSyncStateFileName)
	if err != nil {
		return EntireSyncState{}, false
	}
	var st EntireSyncState
	if err := json.Unmarshal(data, &st); err != nil {
		logging.Debug(ctx, "entire sync state file unreadable; ignoring",
			slog.String("error", err.Error()))
		return EntireSyncState{}, false
	}
	return st, true
}

// SaveEntireSyncState writes the whole state atomically. Callers that
// read-modify-write hold the lock (see MarkEntireSyncMigration).
func SaveEntireSyncState(ctx context.Context, st EntireSyncState) error {
	root, err := capturedSyncRemotesRoot(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode entire sync state: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, entireSyncStateFileName, data, 0o600); err != nil {
		return fmt.Errorf("write entire sync state: %w", err)
	}
	return nil
}

// MarkEntireSyncMigration records the checkpoint migration command's verdict on the
// pre-Entire backlog — done or declined — and the remote it concerned, keeping
// the announcement fields intact. Read-modify-write under the state lock.
func MarkEntireSyncMigration(ctx context.Context, status EntireSyncMigration, from string) error {
	release, err := lockEntireSyncState(ctx)
	if err != nil {
		return err
	}
	defer release()
	st, _ := LoadEntireSyncState(ctx)
	st.Migration = status
	st.MigratedFrom = from
	st.MigratedAt = time.Now().UTC()
	return SaveEntireSyncState(ctx, st)
}

// lockEntireSyncState takes the state lock. The returned release is a no-op
// safe to call when err != nil.
func lockEntireSyncState(ctx context.Context) (func(), error) {
	root, err := capturedSyncRemotesRoot(ctx)
	if err != nil {
		return func() {}, err
	}
	release, err := flock.AcquireIn(root, entireSyncStateLockName)
	if err != nil {
		return func() {}, fmt.Errorf("lock entire sync state: %w", err)
	}
	return release, nil
}

// announceEntireSyncRemoteOnce tells the user, the first time checkpoints land
// on the Entire remote, that this is where they now sync — and, while the
// pre-Entire backlog has not been dealt with, where the older ones still are.
// Persisted rather than a sync.Once: the pre-push hook is a fresh process per
// push, so only the state file can make "once" mean once per clone.
//
// Call it only after a delivery actually succeeded (same rule as the capture
// announcement): the message must not claim a destination that received
// nothing. Re-announces when the Entire remote changed since the last one.
func announceEntireSyncRemoteOnce(ctx context.Context, remoteName string) {
	release, err := lockEntireSyncState(ctx)
	if err != nil {
		logging.Debug(ctx, "entire sync announcement skipped: cannot lock state",
			slog.String("error", err.Error()))
		return
	}
	defer release()

	st, _ := LoadEntireSyncState(ctx)
	if !st.AnnouncedAt.IsZero() && st.Remote == remoteName {
		return
	}
	st.Remote = remoteName
	st.AnnouncedAt = time.Now().UTC()
	if saveErr := SaveEntireSyncState(ctx, st); saveErr != nil {
		// Without the write the next push would announce again; better to stay
		// quiet this time than to nag on every push until the write succeeds.
		logging.Warn(ctx, "failed to persist entire sync announcement",
			slog.String("remote", remoteName),
			slog.String("error", saveErr.Error()))
		return
	}

	fmt.Fprintf(stderrWriter, "[entire] Checkpoints now sync to %q — your Entire remote.\n", remoteName)
	logging.Info(ctx, "entire sync remote announced",
		slog.String("remote", remoteName),
		slog.String("legacy_remote", LegacyCheckpointRemote(ctx)))
}
