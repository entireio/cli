package cli

import (
	"fmt"
	"io"
	"strings"
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

type progressBar struct {
	w       io.Writer
	label   string
	total   int
	current int
	width   int
	enabled bool
}

func startProgressBar(w io.Writer, label string, total int) *progressBar {
	p := &progressBar{
		w:       w,
		label:   label,
		total:   total,
		width:   24,
		enabled: total > 0 && interactive.IsTerminalWriter(w),
	}
	if !p.enabled {
		return p
	}

	counter := fmt.Sprintf(" %d/%d (100%%)", total, total)
	available := getTerminalWidth(w) - len(label) - len(counter) - len(" []")
	p.width = min(max(available, 10), 32)
	p.render()
	return p
}

func (p *progressBar) Increment() {
	p.current++
	if p.current > p.total {
		p.current = p.total
	}
	p.render()
}

func (p *progressBar) Finish() {
	if !p.enabled {
		return
	}
	fmt.Fprint(p.w, "\r\033[K")
}

func (p *progressBar) render() {
	if !p.enabled {
		return
	}

	filled := 0
	percent := 0
	if p.total > 0 {
		filled = p.current * p.width / p.total
		percent = p.current * 100 / p.total
	}
	if p.current >= p.total {
		filled = p.width
		percent = 100
	}

	bar := strings.Repeat("#", filled) + strings.Repeat("-", p.width-filled)
	fmt.Fprintf(p.w, "\r%s [%s] %d/%d (%d%%)", p.label, bar, p.current, p.total, percent)
}
