package checkpoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

// Push-discovery queue file names, kept in the git common dir so every worktree
// sharing the object store enqueues into one queue. The git-refs backend cannot
// push every local checkpoint ref at pre-push time (reads fetch refs too, and
// deleting them after push would hurt local workflows), so each write records
// the ref it touched here and pre-push drains + batch-pushes exactly those.
const (
	pushQueueFileName = "entire-checkpoint-push-queue.jsonl"
	pushQueueLockName = "entire-checkpoint-push-queue.lock"
)

// pushQueueEntry is one JSONL record: a checkpoint ref awaiting push.
type pushQueueEntry struct {
	Ref string `json:"ref"`
}

// PushQueue is a flock-protected JSONL list of checkpoint refs awaiting push,
// stored in the git common dir. Entries are removed only after a confirmed push
// (Remove), so an interrupted or failed push leaves them for the next pre-push.
// Duplicates are tolerated on disk and collapsed by Drain.
type PushQueue struct {
	dir string
}

// NewPushQueue returns the push queue rooted at gitCommonDir.
func NewPushQueue(gitCommonDir string) *PushQueue {
	return &PushQueue{dir: gitCommonDir}
}

// PushQueueForRepo resolves the git common dir for repo and returns its queue.
func PushQueueForRepo(ctx context.Context, repo *git.Repository) (*PushQueue, error) {
	dir, err := resolveGitCommonDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	return NewPushQueue(dir), nil
}

func (q *PushQueue) queuePath() string { return filepath.Join(q.dir, pushQueueFileName) }
func (q *PushQueue) lockPath() string  { return filepath.Join(q.dir, pushQueueLockName) }

// Enqueue appends a ref to the queue. It is safe to enqueue a ref already
// present (or already pushed): Drain collapses duplicates and the batch push is
// idempotent. Enqueue takes the lock so concurrent writers never interleave a
// partial line.
func (q *PushQueue) Enqueue(ref plumbing.ReferenceName) error {
	release, err := flock.Acquire(q.lockPath())
	if err != nil {
		return fmt.Errorf("lock push queue: %w", err)
	}
	defer release()

	line, err := json.Marshal(pushQueueEntry{Ref: ref.String()})
	if err != nil {
		return fmt.Errorf("encode push queue entry: %w", err)
	}
	f, err := os.OpenFile(q.queuePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open push queue: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append push queue entry: %w", err)
	}
	return nil
}

// Drain returns the de-duplicated refs currently queued, in first-seen order. It
// does NOT remove them; call Remove after a confirmed push so a failed push
// retries next time. A missing queue file yields no refs.
//
// It compacts the file in place: when the on-disk queue held redundant lines
// (duplicate enqueues of the same ref, or malformed/blank lines), Drain rewrites
// it to the de-duplicated set. Enqueue only ever appends, so without this the
// file would grow unboundedly between the Removes that are otherwise the sole
// compaction point (e.g. a long-lived session that keeps re-enqueuing the same
// checkpoint ref but never pushes).
func (q *PushQueue) Drain() ([]plumbing.ReferenceName, error) {
	release, err := flock.Acquire(q.lockPath())
	if err != nil {
		return nil, fmt.Errorf("lock push queue: %w", err)
	}
	defer release()

	refs, rawLines, err := q.readLocked()
	if err != nil {
		return nil, err
	}
	if rawLines > len(refs) {
		if err := q.rewriteLocked(refs); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// Remove deletes the given refs from the queue, preserving any entries appended
// after a Drain (e.g. a write that landed during the push). Called after a
// confirmed push.
func (q *PushQueue) Remove(refs []plumbing.ReferenceName) error {
	if len(refs) == 0 {
		return nil
	}
	release, err := flock.Acquire(q.lockPath())
	if err != nil {
		return fmt.Errorf("lock push queue: %w", err)
	}
	defer release()

	current, _, err := q.readLocked()
	if err != nil {
		return err
	}
	removed := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		removed[r.String()] = struct{}{}
	}
	kept := make([]plumbing.ReferenceName, 0, len(current))
	for _, r := range current {
		if _, drop := removed[r.String()]; drop {
			continue
		}
		kept = append(kept, r)
	}
	return q.rewriteLocked(kept)
}

// rewriteLocked replaces the queue file with exactly refs (de-duplicated, one
// line each), or removes the file when refs is empty so a clean repo has no
// stray queue. The caller must hold the lock. The write is atomic (temp file +
// rename) so a concurrent reader never sees a half-written queue.
func (q *PushQueue) rewriteLocked(refs []plumbing.ReferenceName) error {
	if len(refs) == 0 {
		if err := os.Remove(q.queuePath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove empty push queue: %w", err)
		}
		return nil
	}
	var buf bytes.Buffer
	for _, r := range refs {
		line, err := json.Marshal(pushQueueEntry{Ref: r.String()})
		if err != nil {
			return fmt.Errorf("encode push queue entry: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := writeFileAtomicInDir(q.dir, q.queuePath(), buf.Bytes()); err != nil {
		return fmt.Errorf("rewrite push queue: %w", err)
	}
	return nil
}

// readLocked parses the queue file into de-duplicated refs, preserving first-seen
// order. The caller must hold the lock. Malformed lines are skipped rather than
// failing the whole drain — a single bad record must not strand every queued ref.
//
// rawLines is the number of non-empty lines seen (including duplicates and
// malformed records), so callers can detect when the file holds more than the
// de-duplicated set and is worth compacting: rawLines > len(refs) exactly when
// there were redundant lines.
func (q *PushQueue) readLocked() (refs []plumbing.ReferenceName, rawLines int, err error) {
	f, err := os.Open(q.queuePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open push queue: %w", err)
	}
	defer f.Close()

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		rawLines++
		var entry pushQueueEntry
		if err := json.Unmarshal(line, &entry); err != nil || entry.Ref == "" {
			continue
		}
		if _, dup := seen[entry.Ref]; dup {
			continue
		}
		seen[entry.Ref] = struct{}{}
		refs = append(refs, plumbing.ReferenceName(entry.Ref))
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("read push queue: %w", err)
	}
	return refs, rawLines, nil
}

// writeFileAtomicInDir writes data to a temp file in dir and renames it over
// path, so a reader (under the lock) never sees a half-written queue.
func writeFileAtomicInDir(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, pushQueueFileName+".*")
	if err != nil {
		return fmt.Errorf("create temp push queue: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp push queue: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp push queue: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp push queue: %w", err)
	}
	return nil
}
