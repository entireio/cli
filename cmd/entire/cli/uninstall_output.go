package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// marvinUninstallFarewell is printed, dimmed, under the success verdict of
// `entire disable --uninstall`. Success path only: after a failed uninstall a
// joke lands as mockery.
const marvinUninstallFarewell = "Beep, boop. Entire removed. Your repository is on its own now — statistically, that rarely ends well."

// uninstallStepTickInterval is how often the live progress line is redrawn
// while a removal step is still running (external plugins answer over a
// subprocess, so a step can take long enough to look like a hang).
const uninstallStepTickInterval = time.Second

// uninstallPrinter owns the uninstall flow's presentation: styled output on
// stdout, warnings on stderr, with styling resolved per stream (stderr can be
// a TTY while stdout is piped, and vice versa). Off-TTY every helper degrades
// to plain text, so the non-interactive path stays fully readable.
type uninstallPrinter struct {
	w    io.Writer
	errW io.Writer
	out  statusStyles
	err  statusStyles
}

func newUninstallPrinter(w, errW io.Writer) *uninstallPrinter {
	return &uninstallPrinter{w: w, errW: errW, out: newStatusStyles(w), err: newStatusStyles(errW)}
}

// plain prints an unstyled line to stdout.
func (p *uninstallPrinter) plain(text string) {
	fmt.Fprintln(p.w, text)
}

// blank prints an empty line to stdout.
func (p *uninstallPrinter) blank() {
	fmt.Fprintln(p.w)
}

// step prints a completed removal: green check + text.
func (p *uninstallPrinter) step(format string, args ...any) {
	fmt.Fprintf(p.w, "  %s %s\n", p.out.render(p.out.green, "✓"), fmt.Sprintf(format, args...))
}

// stepFailed prints a failed removal: red ✗ + text. The reason and the
// remedy follow on nested warning lines (warnUnder / warnUnderDetail).
func (p *uninstallPrinter) stepFailed(format string, args ...any) {
	fmt.Fprintf(p.w, "  %s %s\n", p.out.render(p.out.red, "✗"), fmt.Sprintf(format, args...))
}

// noop prints a nothing-to-do line, dimmed so completed removals stand out.
func (p *uninstallPrinter) noop(text string) {
	fmt.Fprintf(p.w, "  %s\n", p.out.render(p.out.dim, "· "+text))
}

// warn prints a warning to stderr: yellow ⚠ + message.
func (p *uninstallPrinter) warn(format string, args ...any) {
	p.warnAt("", format, args...)
}

// warnAt prints a ⚠ warning to stderr at the given indent. A message can carry
// an external plugin's own stderr, which is arbitrary text and often several
// lines: continuation lines are indented past the marker so a multi-line
// reason still reads as one warning instead of breaking out of the block.
func (p *uninstallPrinter) warnAt(indent, format string, args ...any) {
	lines := splitMessageLines(fmt.Sprintf(format, args...))
	fmt.Fprintf(p.errW, "%s%s %s\n", indent, p.err.render(p.err.yellow, "⚠"), lines[0])
	for _, line := range lines[1:] {
		// Two spaces: the width of the marker plus its trailing space.
		fmt.Fprintf(p.errW, "%s  %s\n", indent, line)
	}
}

// splitMessageLines splits a message into printable lines, trimming trailing
// blank lines so a reason ending in a newline does not print an empty one.
func splitMessageLines(msg string) []string {
	return strings.Split(strings.TrimRight(msg, "\n \t"), "\n")
}

// warnDetail prints an indented follow-up line under a warning. Deliberately
// unstyled: these lines carry the paste-able plugin recovery command, and ANSI
// codes survive some copy paths.
func (p *uninstallPrinter) warnDetail(format string, args ...any) {
	fmt.Fprintf(p.errW, "  %s\n", fmt.Sprintf(format, args...))
}

// warnUnder prints a warning nested beneath a failed step line (stepFailed),
// indented one level deeper so the reason reads as part of that step. Still
// stderr: on a terminal it interleaves under the ✗ line, and stream-splitting
// callers keep all failure detail on stderr.
func (p *uninstallPrinter) warnUnder(format string, args ...any) {
	p.warnAt("    ", format, args...)
}

// warnUnderDetail prints an unstyled follow-up nested beneath a failed step.
// Unstyled for the same copy-paste reason as warnDetail.
func (p *uninstallPrinter) warnUnderDetail(format string, args ...any) {
	fmt.Fprintf(p.errW, "    %s\n", fmt.Sprintf(format, args...))
}

// successVerdict prints the closing success line: green ✓ + bold text.
func (p *uninstallPrinter) successVerdict(text string) {
	fmt.Fprint(p.w, p.out.successBullet(text))
}

// failureVerdict prints the closing failure line: red ✗ + bold text.
func (p *uninstallPrinter) failureVerdict(text string) {
	fmt.Fprint(p.w, p.out.failureBullet(text))
}

// farewell prints Marvin's dimmed goodbye under the success verdict.
func (p *uninstallPrinter) farewell() {
	fmt.Fprintf(p.w, "\n  %s\n", p.out.render(p.out.dim, marvinUninstallFarewell))
}

// runTimed runs fn while a live "… <label> (Ns)" line counts up every
// uninstallStepTickInterval, so a slow step (an external plugin's subprocess)
// is visibly in progress rather than hung. When fn returns, the progress line
// is erased and the caller prints the outcome line in its place.
//
// The line is redrawn in place rather than grown: erasing is a single-row \r +
// erase-line, so a line long enough to wrap would leave its earlier rows on
// screen. A counter stays a fixed width, and says how long the step has taken.
//
// Live rendering only happens when stdout is a styled terminal — the same
// condition that makes \r + erase-line safe. Off-TTY fn just runs and nothing
// is printed here, keeping piped/CI output free of partial lines.
func (p *uninstallPrinter) runTimed(label string, fn func() error) error {
	if !p.out.colorEnabled {
		return fn()
	}

	draw := func(text string) {
		fmt.Fprintf(p.w, "\r\x1b[2K  %s %s", p.out.render(p.out.dim, "…"), text)
	}
	draw(label)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(uninstallStepTickInterval)
		defer ticker.Stop()
		started := time.Now()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				draw(fmt.Sprintf("%s %s", label,
					p.out.render(p.out.dim, fmt.Sprintf("(%ds)", int(now.Sub(started).Seconds())))))
			}
		}
	}()

	// Deferred so a panic in fn cannot leave the ticker goroutine running and
	// half a progress line on screen for whatever handles it.
	defer func() {
		close(done)
		wg.Wait()
		// Erase the progress line; the outcome line replaces it.
		fmt.Fprint(p.w, "\r\x1b[2K")
	}()
	return fn()
}
