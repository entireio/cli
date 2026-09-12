package binding

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// RecordRetention is how long a session record outlives its last update before
// the sweep removes it.
//
// The store grows without bound otherwise. Every session with a turn-end writes
// a record — including one that never touches a repo, because the no-repo scan
// advances its cursor on a successful scan to keep repeat scans cheap, leaving a
// cursor-only record behind. Nothing else deletes them: they live under the user
// config directory rather than in any repo, so neither `entire clean` nor a
// repo's own cleanup reaches them.
//
// 30 days is deliberately generous against the failure that would actually hurt:
// removing a record whose session is still live. Agent sessions run for days (a
// week is not unusual), and a record removed underneath one loses its cross-repo
// evidence and re-scans its transcript from zero. Reclaiming a few kilobytes a
// month sooner is worth nothing next to that.
const RecordRetention = 30 * 24 * time.Hour

// retentionInterval is how often the prune actually runs, and retentionMarker
// is the file in the store recording when it last did.
//
// Retention has to nominate its own sweep. The detached sweep it rides on is
// spawned only when a session-state file looks like a zombie, and the machines
// whose record store grows are exactly the ones with no zombies to find: a
// session that never touches a repo leaves a cursor-only record and no session
// state at all. Without this, retention would run only on machines that did not
// need it.
//
// The marker is written by the prune rather than by the check, so a check that
// nominates a sweep which never runs does not consume the window.
const (
	retentionInterval = 24 * time.Hour
	retentionMarker   = "last-prune"
)

// RetentionDue reports whether the record store is due for a prune, cheaply
// enough for the session-start hook that decides whether to spawn a sweep: one
// small read, no listing.
//
// It answers false when the store does not exist — a machine that has never
// recorded a session has nothing to prune, and spawning a process to discover
// that is pure cost. It answers true when the marker cannot be read or parsed:
// running an unnecessary prune costs a directory listing in a detached process,
// while skipping a necessary one lets the store grow forever. A marker in the
// future (a clock moved back) is also due, so a bad clock cannot wedge
// retention permanently.
func RetentionDue(_ context.Context, now time.Time) bool {
	root, err := sessionsRoot(false)
	if err != nil {
		return false
	}
	data, err := osroot.ReadFileNoFollow(root, retentionMarker)
	if err != nil {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	return now.Before(last) || !now.Before(last.Add(retentionInterval))
}

// PruneStaleRecords removes session records whose UpdatedAt is older than
// RecordRetention, and reports how many it removed.
//
// Best-effort, and called from the detached sweep: a record that cannot be read
// or removed is logged and skipped, never fatal. It removes only files it
// recognizes as records — this directory sits in the user's config dir, and a
// sweep that deleted anything it did not understand would be a footgun — so a
// file that will not parse as a record is left where it is.
//
// It takes no record lock. Locking would buy nothing a 30-day window has not
// already bought: the only race is a session that has been dormant for a month
// writing a record in the microseconds between this read and this remove, and
// its evidence is rebuilt by the next turn's scan. Blocking is the outcome the
// sweep must not have.
//
// now is a parameter so the boundary is testable; callers pass time.Now().
func PruneStaleRecords(ctx context.Context, now time.Time) (int, error) {
	// sessionsRoot hands back a registry-owned shared root — never close it.
	root, err := sessionsRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // nothing has ever been recorded on this machine
	}
	if err != nil {
		return 0, err
	}

	entries, err := osroot.ReadDir(root, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list session records: %w", err)
	}

	cutoff := now.Add(-RecordRetention)
	pruned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, recordSuffix) {
			continue
		}
		rec, loadErr := loadRecord(root, name, name)
		if loadErr != nil {
			logging.Debug(ctx, "retention: unreadable session record, leaving it in place",
				slog.String("record", name), slog.String("error", loadErr.Error()))
			continue
		}
		if rec == nil || rec.SessionID == "" || rec.UpdatedAt.IsZero() {
			// Vanished mid-sweep, or not a record we wrote. Leaving a file we
			// do not recognize costs a few bytes; deleting someone else's is
			// not recoverable.
			continue
		}
		if !rec.UpdatedAt.Before(cutoff) {
			continue
		}
		if !removeRecordFile(ctx, root, name) {
			continue
		}
		pruned++
	}

	// Record the pass, so RetentionDue stops nominating a sweep until the next
	// interval. Best-effort like the rest: a failed marker write costs one
	// extra prune, which is a directory listing in a detached process.
	if err := jsonutil.WriteFileAtomicIn(root, retentionMarker,
		[]byte(now.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		logging.Debug(ctx, "retention: could not record the prune marker",
			slog.String("error", err.Error()))
	}
	return pruned, nil
}

// removeRecordFile removes a record and the lock file mutateRecord keeps beside
// it, and reports whether the record is gone. Both have to go, or the directory
// still grows without bound — the locks are created one per session and nothing
// else reclaims them.
//
// The lock is removed only after the record it guards, and its failure is not
// the caller's business: an orphaned lock file is inert, while a record left
// behind is re-examined on every sweep.
func removeRecordFile(ctx context.Context, root *os.Root, name string) bool {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.Debug(ctx, "retention: could not remove stale session record",
			slog.String("record", name), slog.String("error", err.Error()))
		return false
	}
	if err := root.Remove(name + lockSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.Debug(ctx, "retention: stale session record removed, lock file left behind",
			slog.String("record", name), slog.String("error", err.Error()))
	}
	return true
}
