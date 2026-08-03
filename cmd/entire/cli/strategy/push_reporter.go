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
// only when styled (ANSI-capable per interactive.ShouldStyle — respects
// NO_COLOR and TERM=cygwin, not merely "stderr is a terminal"), and only once
// the push has run long enough to feel stuck.
func pushShouldReveal(elapsed, threshold time.Duration, styled bool) bool {
	return styled && elapsed >= threshold
}

// pushReporter is a threshold-gated, single-in-place-line progress reporter
// for the pre-push checkpoint sync. It always logs to file via logging.Debug,
// but only writes to its terminal writer once the push has run long enough to
// feel stuck (and never at all when styled is false) — agents and CI must see
// zero bytes.
type pushReporter struct {
	ctx       context.Context
	w         io.Writer
	styled    bool
	threshold time.Duration
	start     time.Time

	mu       sync.Mutex
	revealed bool
	prefix   string
	detail   string

	done    chan struct{}
	stopped chan struct{}
}

// newPushReporter creates a pushReporter and starts its background reveal
// goroutine. Callers must call finish to stop the goroutine and print the
// persistent summary line.
func newPushReporter(ctx context.Context, w io.Writer, styled bool, threshold time.Duration) *pushReporter {
	r := &pushReporter{
		ctx:       ctx,
		w:         w,
		styled:    styled,
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
	if !pushShouldReveal(time.Since(r.start), r.threshold, r.styled) {
		return
	}
	r.revealed = true
	r.redrawLocked()
}

// redrawLocked writes the current in-place line. Callers must hold r.mu.
func (r *pushReporter) redrawLocked() {
	if !r.styled {
		return
	}
	elapsed := int(time.Since(r.start).Seconds())
	if r.detail != "" {
		fmt.Fprintf(r.w, "\r[entire] %s… %s (%ds)\033[K", r.prefix, r.detail, elapsed)
	} else {
		fmt.Fprintf(r.w, "\r[entire] %s… (%ds)\033[K", r.prefix, elapsed)
	}
}

// phase sets the current prefix text (e.g. "syncing 12 checkpoints"). It
// always logs to file; if the line is already revealed, it redraws
// immediately.
func (r *pushReporter) phase(prefix string) {
	logging.Debug(r.ctx, "checkpoint push progress", slog.String("phase", prefix))

	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefix = prefix
	if r.revealed {
		r.redrawLocked()
	}
}

// setDetail updates the live transfer detail (e.g. "writing 40/47 objects")
// shown alongside the prefix, redrawing immediately if the line is already
// revealed. Deliberately does not log to file on every call — it is driven by
// git's --progress stream, which ticks far more often than is useful in the
// debug log; logGitProgress records the transfer detail to file once, after
// the push completes.
func (r *pushReporter) setDetail(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detail = detail
	if r.revealed {
		r.redrawLocked()
	}
}

// finish stops the reveal goroutine and, if the line was ever revealed,
// prints a PERSISTENT final summary line (trailing newline, so it stays in
// the user's scrollback rather than being erased) and logs the final summary
// to file. Stays completely silent when the line was never revealed —
// agents/CI/fast pushes must see zero bytes.
func (r *pushReporter) finish(summary string) {
	close(r.done)
	<-r.stopped

	r.mu.Lock()
	if r.revealed && r.styled {
		elapsed := int(time.Since(r.start).Seconds())
		fmt.Fprintf(r.w, "\r[entire] %s (%ds)\033[K\n", summary, elapsed)
	}
	r.mu.Unlock()

	logging.Debug(r.ctx, "checkpoint push finished", slog.String("summary", summary))
}
