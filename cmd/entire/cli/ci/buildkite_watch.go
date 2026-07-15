package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// This file is intentionally untagged (no `internal` build constraint): it is
// the provider-neutral watch renderer, ported from entiredb's
// cmd/entire-ci/cli/buildkite_watch.go. Keeping it out of the build tag lets the
// normal `go test` run cover renderBuild and runWatch. The command constructor
// and the core-backed data source that consume it live in
// buildkite_watch_internal.go, gated on `internal` alongside the other verbs.

// errNoActiveBuilds is returned by runWatch when no build number was given and
// the source has nothing in flight to watch.
var errNoActiveBuilds = errors.New("no active builds")

// These Buildkite build/step states recur across the three state-classifier
// funcs below (terminal, color, glyph); named so each literal has a single
// source. Single-use states (running, scheduled, …) stay inline.
const (
	statePassed   = "passed"
	stateFailed   = "failed"
	stateCanceled = "canceled"
	stateBlocked  = "blocked"
)

// --- neutral view model: the data-source seam ---
//
// stepView / buildView are deliberately provider-agnostic. The watcher's
// renderer consumes only these, so the data source (here: the core-mediated
// builds endpoint) can be swapped without touching the renderer.

type stepView struct {
	Label       string
	Key         string
	State       string
	Exit        *int
	Started     time.Time
	Finished    time.Time
	HasStarted  bool
	HasFinished bool
}

type buildView struct {
	Number      int
	State       string
	Branch      string
	Commit      string
	Message     string
	URL         string
	Started     time.Time
	Finished    time.Time
	HasStarted  bool
	HasFinished bool
	Steps       []stepView
}

// buildSource abstracts where build status comes from, so the poll loop and
// renderer are testable against a fake and independent of the transport. The
// only production implementation reads entire-core's mediated builds endpoint
// (see coreBuildSource in buildkite_watch_internal.go).
type buildSource interface {
	// ActiveBuilds returns in-flight builds, newest first, for picking a
	// default build when the caller didn't name one.
	ActiveBuilds(ctx context.Context) ([]buildView, error)
	// Build returns a single build with its steps.
	Build(ctx context.Context, number int) (buildView, error)
}

// runWatch polls src for build `number` and renders a live step tree until the
// build reaches a terminal state. When number <= 0 it picks the most recent
// active build. On a TTY it redraws in place; otherwise it prints a fresh frame
// only when build/step states change (so piped output is a clean transition
// log, not a per-tick flood).
func runWatch(ctx context.Context, out io.Writer, src buildSource, number int, interval time.Duration, clock func() time.Time, tty bool) error {
	if clock == nil {
		clock = time.Now
	}
	if number <= 0 {
		active, err := src.ActiveBuilds(ctx)
		if err != nil {
			return fmt.Errorf("list active builds: %w", err)
		}
		if len(active) == 0 {
			return errNoActiveBuilds
		}
		number = active[0].Number
		if len(active) > 1 {
			fmt.Fprintf(out, "watching the most recent of %d active builds (#%d); pass a build number to pick another\n\n", len(active), number)
		}
	}

	var prevLines int
	lastSig := ""
	for {
		v, err := src.Build(ctx, number)
		if err != nil {
			return fmt.Errorf("get build #%d: %w", number, err)
		}
		frame := renderBuild(v, clock(), tty)
		terminal := isTerminalBuildState(v.State)
		if tty {
			if prevLines > 0 {
				fmt.Fprintf(out, "\x1b[%dA\x1b[J", prevLines)
			}
			fmt.Fprint(out, frame)
			prevLines = strings.Count(frame, "\n")
		} else if sig := stateSignature(v); sig != lastSig || terminal {
			fmt.Fprint(out, frame)
			lastSig = stateSignature(v)
		}
		if terminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("watch interrupted: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// renderBuild produces one frame of the watch view. Pure (no I/O, no clock) so
// it's unit-testable; `now` supplies the reference time for in-progress
// durations and `color` gates ANSI on the build-state word only (kept out of
// the tabwriter block so escape codes don't break column alignment).
func renderBuild(v buildView, now time.Time, color bool) string {
	var b strings.Builder
	header := fmt.Sprintf("Build #%d  %s", v.Number, colorize(strings.ToUpper(v.State), buildColor(v.State), color))
	var meta []string
	if v.Branch != "" {
		meta = append(meta, v.Branch)
	}
	if v.Commit != "" {
		meta = append(meta, shortCommit(v.Commit))
	}
	if d, ok := elapsed(v.Started, v.HasStarted, v.Finished, v.HasFinished, now); ok {
		meta = append(meta, fmtDuration(d))
	}
	if len(meta) > 0 {
		header += "  " + strings.Join(meta, " · ")
	}
	b.WriteString(header + "\n")
	if v.Message != "" {
		b.WriteString("  " + v.Message + "\n")
	}
	if v.URL != "" {
		b.WriteString("  " + v.URL + "\n")
	}
	if len(v.Steps) == 0 {
		b.WriteString("  (no steps yet)\n")
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, s := range v.Steps {
		dur := ""
		if d, ok := elapsed(s.Started, s.HasStarted, s.Finished, s.HasFinished, now); ok {
			dur = fmtDuration(d)
		}
		exit := ""
		if s.Exit != nil && *s.Exit != 0 {
			exit = fmt.Sprintf("exit %d", *s.Exit)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", stepGlyph(s.State), s.Label, s.State, dur, exit)
	}
	_ = tw.Flush()
	return b.String()
}

// isTerminalBuildState reports whether a build has stopped progressing (so the
// watcher should exit). "blocked" counts: the build is paused on a block step
// and won't move without human action.
func isTerminalBuildState(s string) bool {
	switch s {
	case statePassed, stateFailed, stateCanceled, "skipped", "not_run", stateBlocked, "finished":
		return true
	}
	return false
}

func stepGlyph(state string) string {
	switch state {
	case statePassed:
		return "✓"
	case stateFailed, "timed_out", "broken":
		return "✗"
	case "running":
		return "▶"
	case stateCanceled, "canceling":
		return "⊘"
	case stateBlocked, "unblocked":
		return "▮"
	case "skipped", "not_run":
		return "–"
	default: // scheduled, waiting, assigned, accepted, limited, ...
		return "·"
	}
}

func buildColor(state string) string {
	switch state {
	case statePassed:
		return "32" // green
	case stateFailed, stateCanceled, stateBlocked:
		return "31" // red
	case "running", "scheduled", "failing", "canceling":
		return "33" // yellow
	}
	return ""
}

func colorize(s, code string, on bool) string {
	if !on || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func stateSignature(v buildView) string {
	var b strings.Builder
	b.WriteString(v.State)
	for _, s := range v.Steps {
		b.WriteByte('|')
		b.WriteString(s.State)
	}
	return b.String()
}

func elapsed(start time.Time, hasStart bool, end time.Time, hasEnd bool, now time.Time) (time.Duration, bool) {
	if !hasStart {
		return 0, false
	}
	stop := now
	if hasEnd {
		stop = end
	}
	if d := stop.Sub(start); d > 0 {
		return d, true
	}
	return 0, true
}

func fmtDuration(d time.Duration) string {
	secs := int(d.Round(time.Second).Seconds())
	if secs < 60 {
		return strconv.Itoa(secs) + "s"
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

func shortCommit(c string) string {
	if len(c) >= 7 {
		return c[:7]
	}
	return c
}
