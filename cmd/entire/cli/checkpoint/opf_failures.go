package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// OPF-failure bookkeeping, kept in the git common dir beside the push queue so
// every worktree sharing the object store agrees on one answer.
//
// Deliberately a SEPARATE file from the push queue rather than an extra field
// on pushQueueEntry. The queue's contract is that a ref leaves it only after a
// confirmed push (Remove) and that reads never mutate membership (Peek); its
// file is also deleted outright once empty. Failure counts have none of those
// lifetimes — they must survive a successful push, and they must be writable by
// a background worker that never pushes anything — so folding them into the
// queue would mean giving Drain/Peek/Remove a second, conflicting set of rules
// about when a record may disappear. That is the contract the pre-push path
// fails closed on, and it is not worth loosening to save one file.
const (
	opfFailureFileName = "entire-checkpoint-opf-failures.json"
	opfFailureLockName = "entire-checkpoint-opf-failures.lock"
)

// StuckOPFFailureThreshold is the number of CONSECUTIVE failed OPF rewrite
// attempts after which a checkpoint ref is reported stuck.
//
// The count exists to separate two states that look identical from outside —
// a ref sitting in the queue because the background worker has not reached it
// yet, and a ref the worker reaches every time and can never rewrite (over the
// per-ref inference cap, or content a broken OPF runtime keeps refusing).
// Three is the first count that cannot be explained by one unlucky pass plus a
// retry, so it is the first count worth telling a human about.
const StuckOPFFailureThreshold = 3

// opfFailureRecord is one ref's consecutive-failure tally.
//
// Nothing removes a record from the on-disk file when its ref is deleted
// out-of-band (checkpoint pruned, branch deleted, `entire clean`) — this file
// has no reference to the queue's or the repo's own ref lifetime, so stale
// records can accumulate here indefinitely. StuckOPFRefs filters its result
// against the repo's live refs so a deleted ref never surfaces as stuck, but
// that only hides the symptom at read time; the record itself is never
// pruned from disk.
type opfFailureRecord struct {
	Count int `json:"count"`
	// LastFailedAt lets a later visibility surface say how long a ref has been
	// stuck without having to guess from the file's mtime, which any unrelated
	// ref's update would move.
	LastFailedAt time.Time `json:"last_failed_at"`
}

// opfFailureFile is the on-disk shape: ref name -> tally.
type opfFailureFile struct {
	Refs map[string]opfFailureRecord `json:"refs"`
}

// OPFFailureLog is a flock-protected per-ref count of consecutive failed OPF
// rewrite attempts, stored in the git common dir. A ref's count is incremented
// when a rewrite pass leaves it still needing OPF, and cleared the moment a
// pass succeeds for it, so the count always means "in a row, right now".
type OPFFailureLog struct {
	dir string
}

// NewOPFFailureLog returns the failure log rooted at gitCommonDir.
func NewOPFFailureLog(gitCommonDir string) *OPFFailureLog {
	return &OPFFailureLog{dir: gitCommonDir}
}

// OPFFailureLogForRepo resolves the git common dir for repo and returns its
// failure log.
func OPFFailureLogForRepo(_ context.Context, repo *git.Repository) (*OPFFailureLog, error) {
	_, dir, err := repositoryDirs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for OPF failure log: %w", err)
	}
	return NewOPFFailureLog(dir), nil
}

// lock opens the common dir's root and takes the failure-log lock inside it,
// returning the root so the caller's reads and writes use the same handle.
func (l *OPFFailureLog) lock() (*os.Root, func(), error) {
	root, err := l.root()
	if err != nil {
		return nil, nil, fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireIn(root, opfFailureLockName)
	if err != nil {
		return nil, nil, fmt.Errorf("lock OPF failure log: %w", err)
	}
	return root, release, nil
}

func (l *OPFFailureLog) root() (*os.Root, error) {
	return gitdir.OpenAt(l.dir) //nolint:wrapcheck // gitdir names the directory; the caller adds the log context
}

// Record applies one rewrite pass's outcome in a single locked
// read-modify-write: every ref in failed has its consecutive count incremented,
// and every ref in succeeded is dropped from the log entirely. Passing both in
// one call is what makes the count mean "consecutive": a ref cannot be counted
// as failing and clearing in two racing halves of the same pass.
//
// Best-effort bookkeeping only. It never gates a push, so its failure is
// reported to the caller for logging and nothing else.
func (l *OPFFailureLog) Record(failed, succeeded []plumbing.ReferenceName, now time.Time) error {
	if len(failed) == 0 && len(succeeded) == 0 {
		return nil
	}
	root, release, err := l.lock()
	if err != nil {
		return err
	}
	defer release()

	current, err := l.readLocked(root)
	if err != nil {
		return err
	}
	for _, ref := range succeeded {
		delete(current, ref.String())
	}
	for _, ref := range failed {
		rec := current[ref.String()]
		rec.Count++
		rec.LastFailedAt = now.UTC()
		current[ref.String()] = rec
	}
	return l.writeLocked(root, current)
}

// Counts returns the current consecutive-failure count for every ref that has
// one. A missing log yields an empty map, not an error.
func (l *OPFFailureLog) Counts() (map[string]int, error) {
	root, release, err := l.lock()
	if err != nil {
		return nil, err
	}
	defer release()

	current, err := l.readLocked(root)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(current))
	for ref, rec := range current {
		counts[ref] = rec.Count
	}
	return counts, nil
}

// StuckRefs returns the refs that have failed OPF rewrite at least
// StuckOPFFailureThreshold times in a row, sorted by ref name so callers can
// render a stable list. Empty means "nothing needs a human", which is also what
// a missing log means.
func (l *OPFFailureLog) StuckRefs() ([]plumbing.ReferenceName, error) {
	counts, err := l.Counts()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(counts))
	for ref, count := range counts {
		if count >= StuckOPFFailureThreshold {
			names = append(names, ref)
		}
	}
	slices.Sort(names)
	stuck := make([]plumbing.ReferenceName, 0, len(names))
	for _, name := range names {
		stuck = append(stuck, plumbing.ReferenceName(name))
	}
	return stuck, nil
}

// StuckOPFRefs is the one-call reader for a status/visibility surface: the
// checkpoint refs whose OPF rewrite has failed StuckOPFFailureThreshold times
// running and so will not resolve itself no matter how many times the
// background worker retries.
func StuckOPFRefs(ctx context.Context, repo *git.Repository) ([]plumbing.ReferenceName, error) {
	log, err := OPFFailureLogForRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	stuck, err := log.StuckRefs()
	if err != nil {
		return nil, err
	}
	// The failure log has no reference to the repo's or the push queue's own
	// ref lifetime, so a ref deleted out-of-band (pruned, branch deleted,
	// `entire clean`) after failing enough to be recorded stuck would
	// otherwise surface here forever. Filter to refs that still exist.
	live := make([]plumbing.ReferenceName, 0, len(stuck))
	for _, name := range stuck {
		if _, refErr := repo.Reference(name, false); refErr == nil {
			live = append(live, name)
		}
	}
	return live, nil
}

// readLocked parses the log file. The caller must hold the lock. A missing file
// is an empty log. A corrupt file is also an empty log rather than an error:
// this is advisory bookkeeping, and refusing to read it would turn a garbled
// byte into a permanently failing background worker.
func (l *OPFFailureLog) readLocked(root *os.Root) (map[string]opfFailureRecord, error) {
	data, err := osroot.ReadFileNoFollow(root, opfFailureFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]opfFailureRecord{}, nil
		}
		return nil, fmt.Errorf("read OPF failure log: %w", err)
	}
	var file opfFailureFile
	//nolint:nilerr // a corrupt tally is advisory: see this function's doc
	if err := json.Unmarshal(data, &file); err != nil || file.Refs == nil {
		return map[string]opfFailureRecord{}, nil
	}
	return file.Refs, nil
}

// writeLocked replaces the log file with exactly refs, or removes it when refs
// is empty so a healthy repo carries no stray file. The caller must hold the
// lock.
func (l *OPFFailureLog) writeLocked(root *os.Root, refs map[string]opfFailureRecord) error {
	if len(refs) == 0 {
		if err := root.Remove(opfFailureFileName); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove empty OPF failure log: %w", err)
		}
		return nil
	}
	data, err := jsonutil.MarshalIndentWithNewline(opfFailureFile{Refs: refs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode OPF failure log: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, opfFailureFileName, data, 0o600); err != nil {
		return fmt.Errorf("write OPF failure log: %w", err)
	}
	return nil
}
