package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// spinnerFrames matches the bubbles/spinner Dot frames used by the activity
// TUI, so a CLI spinner here visually matches `entire activity`.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

const (
	spinnerInterval = 100 * time.Millisecond
	// spinnerInitialDelay is how long an operation must run before the
	// spinner appears at all. Faster operations don't get a spinner —
	// avoids flicker on warm runs that complete in under a quarter second.
	spinnerInitialDelay = 250 * time.Millisecond
)

// startSpinner prints msg followed by an animated spinner to w when the
// operation takes longer than spinnerInitialDelay. Returns a stop function
// that clears the spinner line and prints suffix (with a newline) if
// non-empty. Fast operations that call stop before the initial delay
// elapses produce no output at all.
//
// When w is not a terminal (CI, redirected output, agent subprocess), the
// spinner and the suppression message are both omitted — non-interactive
// callers get clean output without progress chatter.
func startSpinner(w io.Writer, msg string) func(suffix string) {
	if !interactive.IsTerminalWriter(w) {
		return func(suffix string) {
			if suffix != "" {
				fmt.Fprintln(w, suffix)
			}
		}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-done:
			return // operation finished before the spinner would appear
		case <-time.After(spinnerInitialDelay):
		}
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		frame := 0
		fmt.Fprintf(w, "\r%s %s", spinnerFrames[frame], msg)
		frame = (frame + 1) % len(spinnerFrames)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(w, "\r%s %s", spinnerFrames[frame], msg)
				frame = (frame + 1) % len(spinnerFrames)
			}
		}
	}()
	return func(suffix string) {
		close(done)
		<-stopped
		// \r\033[K is a no-op on a line that was never drawn; on a drawn
		// line it returns the cursor and clears it.
		fmt.Fprint(w, "\r\033[K")
		if suffix != "" {
			fmt.Fprintln(w, suffix)
		}
	}
}
