package strategy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// WithSessionMetadata serializes publication of the stored transcript and
// prompts with condensation and cleanup. fn must use atomic replacement for
// writes (entiredir.WriteFile), so even identical bytes have a new file identity.
// It also works before InitializeSession: publishing a prompt must not create
// session state. The root and directory use the existing home-worktree rules.
func WithSessionMetadata(ctx context.Context, sessionID string, fn func(*os.Root) error) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	_, _, release, err := acquireSessionGate(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()
	root, err := OpenSessionEntireDir(ctx, sessionID)
	if err != nil {
		return err
	}
	name := entiredir.MustName(paths.SessionMetadataDirFromSessionID(sessionID))
	if err := osroot.MkdirAllNoSymlink(root, name, 0o750); err != nil {
		return fmt.Errorf("create session metadata directory: %w", err)
	}
	return fn(root)
}

// storedCopyRelease records the files present before condensation reads them.
// A later Stop may publish before saving its step, so pending state alone cannot
// establish ownership. Keep the whole buffer if any file was replaced/created,
// or if inspection fails; prompt and transcript are released together.
type storedCopyRelease struct {
	root         *os.Root
	worktreePath string
	turnID       string
	checkpointID id.CheckpointID
	files        map[string]fs.FileInfo
	valid        bool
}

func snapshotStoredCopy(root *os.Root, state *SessionState) *storedCopyRelease {
	r := &storedCopyRelease{root: root, worktreePath: state.WorktreePath, turnID: state.TurnID}
	if root == nil {
		return r
	}
	r.files = make(map[string]fs.FileInfo, len(stagedSessionFiles))
	for _, base := range stagedSessionFiles {
		name, err := storedSessionFileName(state.SessionID, base)
		if err != nil {
			return r
		}
		info, err := osroot.LstatNoSymlinks(root, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return r
		}
		r.files[base] = info
	}
	r.valid = true
	return r
}

func (r *storedCopyRelease) unchanged(sessionID string) bool {
	if !r.valid {
		return false
	}
	for _, base := range stagedSessionFiles {
		name, err := storedSessionFileName(sessionID, base)
		if err != nil {
			return false
		}
		current, err := osroot.LstatNoSymlinks(r.root, name)
		before := r.files[base]
		if before == nil && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || before == nil || !os.SameFile(before, current) ||
			before.Size() != current.Size() || !before.ModTime().Equal(current.ModTime()) {
			return false
		}
	}
	return true
}

// run is an onSaved effect: failed/skipped/nested-unsaved mutations never
// release recovery data. Reacquire the gate because onSaved runs unlocked, then
// recheck both state ownership and file identities before deleting anything.
func (r *storedCopyRelease) run(ctx context.Context, sessionID string) {
	err := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		if state.HasPendingWork() || state.Phase == session.PhaseActive ||
			state.TurnID != r.turnID || state.LastCheckpointID != r.checkpointID || !r.unchanged(sessionID) {
			return ErrMutationSkip
		}
		clearStagedFilesIn(ctx, r.root, sessionID, r.worktreePath)
		return ErrMutationSkip // cleanup does not change session state
	})
	if err != nil && !errors.Is(err, ErrStateNotFound) {
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "stored session copy not released after condensation",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// AcquireSessionMetadataPublication protects a Stop's staged copy until its
// step and lifecycle updates finish. OS release on exit also handles crashes;
// no persistent flag can strand a session after a killed hook. Take this lease
// before any state gate. Cleanup uses only TryAcquireIn under a state gate, so
// it skips an in-flight publisher instead of introducing a lock-order cycle.
// Never unlink the lock file: waiters must continue to lock the same inode.
func AcquireSessionMetadataPublication(ctx context.Context, sessionID string) (func(), error) {
	lock, err := stateLockForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	release, err := flock.AcquireContextIn(ctx, lock.root, lock.name+".metadata")
	if err != nil {
		return nil, fmt.Errorf("acquire staged-copy publication: %w", err)
	}
	return release, nil
}

func trySessionMetadataCleanup(ctx context.Context, sessionID string) (func(), error) {
	lock, err := stateLockForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	release, err := flock.TryAcquireIn(lock.root, lock.name+".metadata")
	if err != nil {
		if !errors.Is(err, flock.ErrLocked) {
			logging.Warn(logging.WithComponent(ctx, "checkpoint"), "stored copy retained: cleanup lock unavailable",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
		}
		return nil, fmt.Errorf("acquire staged-copy cleanup: %w", err)
	}
	return release, nil
}
