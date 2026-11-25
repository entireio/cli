package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/bgupload"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

// UploadKind is the background upload kind that drains the checkpoint ref push queue.
const UploadKind = "checkpoint-refs"

// UploadWorkerDeadline bounds a checkpoint upload worker's run.
const UploadWorkerDeadline = 30 * time.Minute

// OPF decisions a pre-push carries to the background worker. Unset means the
// pushing hook never resolved one; the worker then applies the non-interactive
// rules, never a skip.
const (
	UploadOPFUnset = ""
	UploadOPFRun   = "run"
	UploadOPFSkip  = "skip"
)

// UploadRequest is what a pre-push decided in the foreground, where it can
// still ask the user, so the worker acts on it rather than re-deriving it.
type UploadRequest struct {
	Remote      string `json:"remote"`
	OPFDecision string `json:"opf_decision,omitempty"`
	// PendingCapture is the checkpoint sync remote this push would elect,
	// persisted only once the worker's delivery lands.
	PendingCapture string `json:"pending_capture,omitempty"`
	// QueueDigest identifies the queue the OPF decision was made for. A skip
	// covers only that queue: checkpoints queued later get the non-interactive
	// rules (see QueueDigest).
	QueueDigest string `json:"queue_digest,omitempty"`
}

// QueueDigest identifies a set of queue entries, in any order.
func QueueDigest(entries []PushQueueEntry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Ref.String()+" "+e.Hash.String())
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UserPush marks a user's `git push` the worker waits for before it pushes:
// GitHub rejects a push landing within about a second of another push to the
// same repository finishing. Process carries a start-time fingerprint, so a
// reused PID is not mistaken for the push; it is zero when the hook could not
// find the process.
type UserPush struct {
	Process proclive.Identity `json:"process,omitzero"`
	At      time.Time         `json:"at"`
}

// userPushMarkTTL drops marks no worker consumed.
const userPushMarkTTL = 10 * time.Minute

// UploadJob is the checkpoint upload's job state.
type UploadJob struct {
	Request    *UploadRequest `json:"request,omitempty"`
	UserPushes []UserPush     `json:"user_pushes,omitempty"`
}

// UploadCoordinator is the checkpoint upload's background coordinator.
type UploadCoordinator struct {
	*bgupload.Coordinator[UploadJob]
}

// NewUploadCoordinator returns the coordinator rooted at gitCommonDir.
func NewUploadCoordinator(gitCommonDir string) *UploadCoordinator {
	return &UploadCoordinator{bgupload.New[UploadJob](gitCommonDir, UploadKind, UploadWorkerDeadline)}
}

// UploadCoordinatorForRepo resolves the git common dir for repo.
func UploadCoordinatorForRepo(repo *git.Repository) (*UploadCoordinator, error) {
	_, dir, err := repositoryDirs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for checkpoint upload: %w", err)
	}
	return NewUploadCoordinator(dir), nil
}

func (c *UploadCoordinator) update(mutate func(*bgupload.State[UploadJob])) error {
	if err := c.Update(mutate); err != nil {
		return fmt.Errorf("checkpoint upload: %w", err)
	}
	return nil
}

// Request records req, merged with any request not yet taken: the latest
// push's destination wins, and so does a decision to run OPF, because the
// queue still holds the earlier push's refs.
func (c *UploadCoordinator) Request(req UploadRequest) error {
	return c.update(func(st *bgupload.State[UploadJob]) {
		if prev := st.Job.Request; prev != nil {
			if prev.OPFDecision == UploadOPFRun {
				req.OPFDecision = UploadOPFRun
			}
			if req.PendingCapture == "" && prev.Remote == req.Remote {
				req.PendingCapture = prev.PendingCapture
			}
		}
		st.Job.Request = &req
	})
}

// TakeRequest removes and returns the pending request, or nil.
func (c *UploadCoordinator) TakeRequest() (*UploadRequest, error) {
	var req *UploadRequest
	err := c.update(func(st *bgupload.State[UploadJob]) {
		req, st.Job.Request = st.Job.Request, nil
	})
	return req, err
}

// HasRequest reports whether a request is waiting for a worker.
func (c *UploadCoordinator) HasRequest() bool {
	st, err := c.State()
	return err == nil && HasUploadRequest(st)
}

// HasUploadRequest reports whether st holds a request.
func HasUploadRequest(st bgupload.State[UploadJob]) bool { return st.Job.Request != nil }

// NoteUserPush records a user's git push for the worker to wait on, dropping
// marks older than userPushMarkTTL.
func (c *UploadCoordinator) NoteUserPush(push UserPush) error {
	return c.update(func(st *bgupload.State[UploadJob]) {
		kept := make([]UserPush, 0, len(st.Job.UserPushes)+1)
		for _, p := range st.Job.UserPushes {
			if push.At.Sub(p.At) < userPushMarkTTL {
				kept = append(kept, p)
			}
		}
		st.Job.UserPushes = append(kept, push) //nolint:gocritic // kept is a fresh slice by construction
	})
}

// TakeUserPushes returns and clears the recorded user pushes.
func (c *UploadCoordinator) TakeUserPushes() ([]UserPush, error) {
	var pushes []UserPush
	err := c.update(func(st *bgupload.State[UploadJob]) {
		pushes, st.Job.UserPushes = st.Job.UserPushes, nil
	})
	return pushes, err
}
