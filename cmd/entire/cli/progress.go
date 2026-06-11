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
// operation takes longer than spinnerInitialDelay. stop(true) leaves
// "✓ msg" on the line; stop(false) erases the line and writes nothing.
// On non-terminal writers the animation is omitted but stop(true) still
// prints the completion line.
func startSpinner(w io.Writer, msg string) func(success bool) {
	if !interactive.IsTerminalWriter(w) {
		return func(success bool) {
			if success {
				fmt.Fprintf(w, "✓ %s\n", msg)
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
	return func(success bool) {
		close(done)
		<-stopped
		if success {
			fmt.Fprintf(w, "\r\033[K✓ %s\n", msg)
			return
		}
		fmt.Fprint(w, "\r\033[K")
	}
}
