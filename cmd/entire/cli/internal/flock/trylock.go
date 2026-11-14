package flock

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrLocked reports that TryAcquireIn found the lock held by someone else.
var ErrLocked = errors.New("flock: lock is held")

// TryAcquireIn takes the lock named inside root if it is free right now, and
// returns ErrLocked if another holder has it. Use it to tell a caller that a
// wait is about to begin, then wait with AcquireContextIn.
//
// It is AcquireContextIn with a deadline already past: both platforms make one
// non-blocking attempt before they first consult the context, so an expired
// one means exactly one attempt.
func TryAcquireIn(root *os.Root, name string) (release func(), err error) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	release, err = AcquireContextIn(ctx, root, name)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrLocked
	}
	return release, err
}
