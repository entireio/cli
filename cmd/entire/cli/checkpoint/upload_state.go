package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// Background checkpoint upload coordination, kept in the git common dir next to
// the push queue: the queue is shared by every worktree, so is the one worker
// that drains it.
const (
	// uploadWorkerLockName is held for a whole flush of the queue — by the
	// background worker for its run, and by every foreground flush — so two
	// processes never push (and fetch+replay) the same refs at once.
	uploadWorkerLockName = "entire-checkpoint-upload.lock"
	uploadStateFileName  = "entire-checkpoint-upload-state.json"
	uploadStateLockName  = "entire-checkpoint-upload-state.lock"
)

// uploadWorkerTryWait turns the blocking flock into a try-lock: a caller that
// cannot take it almost at once treats the queue as being uploaded by its holder.
const uploadWorkerTryWait = 100 * time.Millisecond

// OPF decisions a pre-push carries to the background worker. Unset means the
// pushing hook never resolved one; the worker then applies the non-interactive
// rules, never a skip.
const (
	UploadOPFUnset = ""
	UploadOPFRun   = "run"
	UploadOPFSkip  = "skip"
)

// UploadRequest is a pre-push's hand-off to the background worker: what the
// hook decided in the foreground, where it can still ask the user, so the worker
// acts on that decision rather than re-deriving it later from changed state.
type UploadRequest struct {
	Remote      string `json:"remote"`
	OPFDecision string `json:"opf_decision,omitempty"`
	// PendingCapture is the checkpoint sync remote this push would elect. The
	// worker persists it only once its own delivery lands, as the hook would.
	PendingCapture string    `json:"pending_capture,omitempty"`
	RequestedAt    time.Time `json:"requested_at"`
}

// UploadResult is the background worker's record of its last run.
type UploadResult struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Pushed     int       `json:"pushed"`
	Remaining  int       `json:"remaining"`
	// Error is a one-line account of why refs stayed queued, already
	// redacted for display; "" when the run left nothing behind.
	Error string `json:"error,omitempty"`
	// Announcement is a user-facing line the worker could not print itself
	// (its stdio is the null device), shown by the next push and status.
	Announcement string `json:"announcement,omitempty"`
}

// Running reports whether the run had not finished as of now. A record older
// than maxRun is stale (the worker was killed), not running.
func (r *UploadResult) Running(now time.Time, maxRun time.Duration) bool {
	return r != nil && r.FinishedAt.IsZero() && now.Sub(r.StartedAt) < maxRun
}

// UploadState is the coordination file's contents.
type UploadState struct {
	Request *UploadRequest `json:"request,omitempty"`
	Last    *UploadResult  `json:"last,omitempty"`
}

// UploadCoordinator owns the background upload's lock and state file for one
// git common dir.
type UploadCoordinator struct {
	dir string
}

// NewUploadCoordinator returns the coordinator rooted at gitCommonDir.
func NewUploadCoordinator(gitCommonDir string) *UploadCoordinator {
	return &UploadCoordinator{dir: gitCommonDir}
}

// UploadCoordinatorForRepo resolves the git common dir for repo.
func UploadCoordinatorForRepo(repo *git.Repository) (*UploadCoordinator, error) {
	_, dir, err := repositoryDirs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for checkpoint upload: %w", err)
	}
	return NewUploadCoordinator(dir), nil
}

func (c *UploadCoordinator) root() (*os.Root, error) {
	return gitdir.OpenAt(c.dir) //nolint:wrapcheck // gitdir names the directory; callers add context
}

// TryLockWorker takes the upload lock if it is free. ok is false, with a nil
// error, when another process holds it.
func (c *UploadCoordinator) TryLockWorker(ctx context.Context) (release func(), ok bool, err error) {
	lockCtx, cancel := context.WithTimeout(ctx, uploadWorkerTryWait)
	defer cancel()
	release, err = c.LockWorker(lockCtx)
	if err != nil {
		if errors.Is(lockCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return release, true, nil
}

// LockWorker waits for the upload lock until ctx is done.
func (c *UploadCoordinator) LockWorker(ctx context.Context) (func(), error) {
	root, err := c.root()
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireContextIn(ctx, root, uploadWorkerLockName)
	if err != nil {
		return nil, fmt.Errorf("lock checkpoint upload: %w", err)
	}
	return release, nil
}

// Request records req for the worker, merged with any request not yet taken:
// the latest push's destination wins, and so does a decision to run OPF. The
// queue holds the earlier push's refs too, so a later push that resolved skip
// or nothing must not let them upload unscanned.
func (c *UploadCoordinator) Request(req UploadRequest) error {
	return c.update(func(st *UploadState) {
		if st.Request != nil && st.Request.OPFDecision == UploadOPFRun {
			req.OPFDecision = UploadOPFRun
		}
		if req.PendingCapture == "" && st.Request != nil && st.Request.Remote == req.Remote {
			req.PendingCapture = st.Request.PendingCapture
		}
		st.Request = &req
	})
}

// TakeRequest removes and returns the pending request, or nil.
func (c *UploadCoordinator) TakeRequest() (*UploadRequest, error) {
	var req *UploadRequest
	err := c.update(func(st *UploadState) {
		req, st.Request = st.Request, nil
	})
	return req, err
}

// StartRun records a run starting at started. An announcement no push has
// shown yet is carried into the new record rather than lost with the old one.
func (c *UploadCoordinator) StartRun(started time.Time) error {
	return c.update(func(st *UploadState) {
		res := UploadResult{StartedAt: started}
		if st.Last != nil {
			res.Announcement = st.Last.Announcement
		}
		st.Last = &res
	})
}

// FinishRun records the outcome of the run StartRun began, appending its
// announcement to any still unshown.
func (c *UploadCoordinator) FinishRun(res UploadResult) error {
	return c.update(func(st *UploadState) {
		if st.Last != nil && st.Last.Announcement != "" {
			res.Announcement = strings.TrimSpace(st.Last.Announcement + "\n" + res.Announcement)
		}
		st.Last = &res
	})
}

// ClearReported drops the failure and announcement of the run that started at
// startedAt, once a push has shown them. A newer run's record is left alone.
func (c *UploadCoordinator) ClearReported(startedAt time.Time) error {
	return c.update(func(st *UploadState) {
		if st.Last != nil && st.Last.StartedAt.Equal(startedAt) {
			st.Last.Error = ""
			st.Last.Announcement = ""
		}
	})
}

// HasRequest reports whether a request is waiting for a worker.
func (c *UploadCoordinator) HasRequest() bool {
	st, err := c.State()
	return err == nil && st.Request != nil
}

// State reads the coordination file. A missing file is the zero state.
func (c *UploadCoordinator) State() (UploadState, error) {
	root, err := c.root()
	if err != nil {
		return UploadState{}, fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireIn(root, uploadStateLockName)
	if err != nil {
		return UploadState{}, fmt.Errorf("lock checkpoint upload state: %w", err)
	}
	defer release()
	return readUploadState(root)
}

func (c *UploadCoordinator) update(mutate func(*UploadState)) error {
	root, err := c.root()
	if err != nil {
		return fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireIn(root, uploadStateLockName)
	if err != nil {
		return fmt.Errorf("lock checkpoint upload state: %w", err)
	}
	defer release()
	st, err := readUploadState(root)
	if err != nil {
		return err
	}
	mutate(&st)
	data, err := jsonutil.MarshalIndentWithNewline(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint upload state: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, uploadStateFileName, data, 0o600); err != nil {
		return fmt.Errorf("write checkpoint upload state: %w", err)
	}
	return nil
}

func readUploadState(root *os.Root) (UploadState, error) {
	var st UploadState
	data, err := root.ReadFile(uploadStateFileName)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read checkpoint upload state: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		// A corrupt file must not wedge uploads: start over.
		return UploadState{}, nil //nolint:nilerr // deliberate reset of unreadable state
	}
	return st, nil
}
