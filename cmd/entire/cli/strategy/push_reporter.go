package strategy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// pushShouldReveal reports whether the pre-push progress line should be shown:
// only on a terminal, and only once the push has run long enough to feel stuck.
func pushShouldReveal(elapsed, threshold time.Duration, isTTY bool) bool {
	return isTTY && elapsed >= threshold
}

// pushReporter is a threshold-gated, single-in-place-line progress reporter
// for the pre-push checkpoint sync. It always logs to file via logging.Debug,
// but only writes to its terminal writer once the push has run long enough to
// feel stuck (and never at all when isTTY is false) — agents and CI must see
// zero bytes.
type pushReporter struct {
	ctx       context.Context
	w         io.Writer
	isTTY     bool
	threshold time.Duration
	start     time.Time

	mu       sync.Mutex
	revealed bool
	text     string

	done    chan struct{}
	stopped chan struct{}
}

// newPushReporter creates a pushReporter and starts its background reveal
// goroutine. Callers must call finish to stop the goroutine and clear the
// line.
func newPushReporter(ctx context.Context, w io.Writer, isTTY bool, threshold time.Duration) *pushReporter {
	r := &pushReporter{
		ctx:       ctx,
		w:         w,
		isTTY:     isTTY,
		threshold: threshold,
		start:     time.Now(),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go r.run()
	return r
}

// run waits until the threshold elapses, then ticks ~1s, redrawing the
// in-place line while the reporter is not yet done.
func (r *pushReporter) run() {
	defer close(r.stopped)

	timer := time.NewTimer(r.threshold)
	defer timer.Stop()

	select {
	case <-r.done:
		return
	case <-timer.C:
	}

	r.mu.Lock()
	r.maybeReveal()
	r.mu.Unlock()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.mu.Lock()
			r.maybeReveal()
			r.mu.Unlock()
		}
	}
}

// maybeReveal writes the current in-place line when reveal conditions are
// met. Callers must hold r.mu.
func (r *pushReporter) maybeReveal() {
	if !pushShouldReveal(time.Since(r.start), r.threshold, r.isTTY) {
		return
	}
	r.revealed = true
	r.redrawLocked()
}

// redrawLocked writes the current in-place line. Callers must hold r.mu.
func (r *pushReporter) redrawLocked() {
	if !r.isTTY {
		return
	}
	elapsed := int(time.Since(r.start).Seconds())
	fmt.Fprintf(r.w, "\r[entire] %s (%ds)\033[K", r.text, elapsed)
}

// phase sets the current phase text. It always logs to file; if the line is
// already revealed, it redraws immediately.
func (r *pushReporter) phase(text string) {
	logging.Debug(r.ctx, "checkpoint push progress", slog.String("phase", text))

	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = text
	if r.revealed {
		r.redrawLocked()
	}
}

// finish stops the reveal goroutine, clears the in-place line if it was
// revealed, and logs the final summary to file.
func (r *pushReporter) finish(summary string) {
	close(r.done)
	<-r.stopped

	r.mu.Lock()
	if r.revealed && r.isTTY {
		fmt.Fprint(r.w, "\r\033[K")
	}
	r.mu.Unlock()

	logging.Debug(r.ctx, "checkpoint push finished", slog.String("summary", summary))
}
