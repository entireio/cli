// Package bgupload runs uploads in one detached worker per repository and kind
// of work, so a hook can return before the upload finishes. Each kind has its
// own lock and state file in the git common dir, shared by every worktree; a
// kind adds its own job state (S) beside the generic run record.
//
// The worker's stdio is the null device, so what it would have printed is kept
// in the run record (Result) and shown by the next process that reports it.
package bgupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// WorkerCommand is the hidden command a spawned worker runs, followed by its kind.
const WorkerCommand = "__upload"

// Result is a worker's record of its last run.
type Result struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Done       int       `json:"done"`
	Remaining  int       `json:"remaining"`
	// Error says in one line, ready for display, why work was left; Detail is
	// the actionable text behind it. Both are replaced by every run.
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Announcement is a line the worker could not print itself.
	Announcement string `json:"announcement,omitempty"`
}

// Running reports whether the run had not finished as of now. A record older
// than maxRun is stale (the worker was killed), not running.
func (r *Result) Running(now time.Time, maxRun time.Duration) bool {
	return r != nil && r.FinishedAt.IsZero() && now.Sub(r.StartedAt) < maxRun
}

// State is a kind's state file: its job state and its last run.
type State[S any] struct {
	Job  S       `json:"job"`
	Last *Result `json:"last,omitempty"`
}

// Coordinator owns one kind's lock and state file in a git common dir.
type Coordinator[S any] struct {
	dir    string
	kind   string
	maxRun time.Duration // a worker's run deadline; an older unfinished record is stale
}

// New returns kind's coordinator rooted at gitCommonDir. maxRun is how long a
// worker of this kind may run.
func New[S any](gitCommonDir, kind string, maxRun time.Duration) *Coordinator[S] {
	return &Coordinator[S]{dir: gitCommonDir, kind: kind, maxRun: maxRun}
}

func (c *Coordinator[S]) root() (*os.Root, error) {
	root, err := gitdir.OpenAt(c.dir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	return root, nil
}

func (c *Coordinator[S]) name(suffix string) string { return "entire-upload-" + c.kind + suffix }

// TryLock takes the worker lock if it is free right now. ok is false, with a
// nil error, when another process holds it. Every flush of the kind's work
// holds this lock, foreground or background.
func (c *Coordinator[S]) TryLock() (release func(), ok bool, err error) {
	root, err := c.root()
	if err != nil {
		return nil, false, err
	}
	release, err = flock.TryAcquireIn(root, c.name(".lock"))
	if errors.Is(err, flock.ErrLocked) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock %s upload: %w", c.kind, err)
	}
	return release, true, nil
}

// Lock waits for the worker lock until ctx is done.
func (c *Coordinator[S]) Lock(ctx context.Context) (func(), error) {
	root, err := c.root()
	if err != nil {
		return nil, err
	}
	release, err := flock.AcquireContextIn(ctx, root, c.name(".lock"))
	if err != nil {
		return nil, fmt.Errorf("lock %s upload: %w", c.kind, err)
	}
	return release, nil
}

// State reads the state file. A missing or corrupt file is the zero state.
func (c *Coordinator[S]) State() (State[S], error) {
	var st State[S]
	err := c.withStateLock(func(root *os.Root) error {
		var readErr error
		st, readErr = c.read(root)
		return readErr
	})
	return st, err
}

// Update applies mutate to the state file under its lock.
func (c *Coordinator[S]) Update(mutate func(*State[S])) error {
	return c.withStateLock(func(root *os.Root) error {
		st, err := c.read(root)
		if err != nil {
			return err
		}
		mutate(&st)
		data, err := jsonutil.MarshalIndentWithNewline(st, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s upload state: %w", c.kind, err)
		}
		if err := jsonutil.WriteFileAtomicIn(root, c.name("-state.json"), data, 0o600); err != nil {
			return fmt.Errorf("write %s upload state: %w", c.kind, err)
		}
		return nil
	})
}

func (c *Coordinator[S]) withStateLock(fn func(*os.Root) error) error {
	root, err := c.root()
	if err != nil {
		return err
	}
	release, err := flock.AcquireIn(root, c.name("-state.lock"))
	if err != nil {
		return fmt.Errorf("lock %s upload state: %w", c.kind, err)
	}
	defer release()
	return fn(root)
}

func (c *Coordinator[S]) read(root *os.Root) (State[S], error) {
	var st State[S]
	data, err := root.ReadFile(c.name("-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read %s upload state: %w", c.kind, err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		// A corrupt file must not wedge uploads: start over.
		return State[S]{}, nil //nolint:nilerr // deliberate reset of unreadable state
	}
	return st, nil
}

// StartRun records a run starting at started, keeping any announcement no one
// has shown yet.
func (c *Coordinator[S]) StartRun(started time.Time) error {
	return c.Update(func(st *State[S]) {
		res := Result{StartedAt: started}
		if st.Last != nil {
			res.Announcement = st.Last.Announcement
		}
		st.Last = &res
	})
}

// FinishRun records the outcome of the run StartRun began, appending its
// announcement to any still unshown.
func (c *Coordinator[S]) FinishRun(res Result) error {
	return c.Update(func(st *State[S]) {
		if st.Last != nil && st.Last.Announcement != "" {
			res.Announcement = strings.TrimSpace(st.Last.Announcement + "\n" + res.Announcement)
		}
		st.Last = &res
	})
}

// Report prints, once, what the last finished run could not: its announcement
// and why it left work. label names the work, e.g. "checkpoint upload".
func (c *Coordinator[S]) Report(w io.Writer, label string) error {
	st, err := c.State()
	if err != nil || st.Last == nil || st.Last.Running(time.Now(), c.maxRun) {
		return err
	}
	last := st.Last
	if last.Announcement == "" && last.Error == "" {
		return nil
	}
	if last.Announcement != "" {
		fmt.Fprintln(w, last.Announcement)
	}
	if last.Error != "" {
		fmt.Fprintf(w, "[entire] Last background %s stopped: %s.\n", label, last.Error)
		if last.Detail != "" {
			fmt.Fprintln(w, last.Detail)
		}
	}
	return c.Update(func(st *State[S]) {
		if st.Last != nil && st.Last.StartedAt.Equal(last.StartedAt) {
			st.Last.Error, st.Last.Detail, st.Last.Announcement = "", "", ""
		}
	})
}

// Spawn starts a detached worker for kind in worktreeRoot and reports whether
// it started. A no-op under go test.
func Spawn(worktreeRoot, kind string) bool {
	return execx.SpawnDetached(worktreeRoot, WorkerCommand, kind)
}

// RunPasses is a worker's body: while it can take the lock and has work, it
// runs pass, at most maxPasses times. pending reports work waiting in the
// state. It returns true when work is still waiting at the end, so the caller
// should start a fresh worker. A held lock ends the run: its holder starts a
// worker for anything left when it releases.
func (c *Coordinator[S]) RunPasses(ctx context.Context, maxPasses int, pending func(State[S]) bool, pass func(context.Context)) (leftover bool) {
	hasWork := func() bool {
		st, err := c.State()
		return err == nil && pending(st)
	}
	for range maxPasses {
		if ctx.Err() != nil {
			break
		}
		release, ok, err := c.TryLock()
		if err != nil || !ok {
			return false
		}
		pass(ctx)
		release()
		if !hasWork() {
			return false
		}
	}
	return hasWork()
}
