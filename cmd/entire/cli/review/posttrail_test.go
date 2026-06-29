package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

type postTrailAlreadyPrintedError struct {
	err error
}

func (e postTrailAlreadyPrintedError) Error() string {
	return e.err.Error()
}

func (e postTrailAlreadyPrintedError) Unwrap() error {
	return e.err
}

func (e postTrailAlreadyPrintedError) AlreadyPrinted() bool {
	return true
}

func postTrailSummary(narrative string) reviewtypes.RunSummary {
	var buf []reviewtypes.Event
	if narrative != "" {
		buf = []reviewtypes.Event{reviewtypes.AssistantText{Text: narrative}}
	}
	return reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{{
			Name:   "claude-code",
			Status: reviewtypes.AgentStatusSucceeded,
			Buffer: buf,
		}},
	}
}

func TestMaybePostReviewToTrail(t *testing.T) {
	t.Parallel()

	t.Run("local mode never posts and stays silent about the trail", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		called := false
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, string) error {
			called = true
			return nil
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputLocal, "general", postTrailSummary("a finding"), "")
		if called {
			t.Error("local mode must not post to the trail")
		}
		if out.Len() != 0 {
			t.Errorf("local mode should print nothing about the trail, got %q", out.String())
		}
	})

	t.Run("trail mode with output posts the verdict via the hook", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		gotVerdict := ""
		deps := Deps{PostReviewToTrail: func(_ context.Context, w io.Writer, _, verdict string) error {
			gotVerdict = verdict
			fmt.Fprintln(w, "Posted the review verdict to trail #1 as a finding.")
			fmt.Fprintln(w, "View the trail: https://entire.io/gh/o/r/trails/1/b")
			return nil
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", postTrailSummary("real finding"), "the verdict")
		if gotVerdict != "the verdict" {
			t.Errorf("verdict passed to hook = %q, want %q", gotVerdict, "the verdict")
		}
		if !strings.Contains(out.String(), "Posted the review verdict to trail #1") ||
			!strings.Contains(out.String(), "View the trail:") {
			t.Errorf("expected posted confirmation + link, got %q", out.String())
		}
	})

	t.Run("trail mode with nothing to report confirms and skips posting", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		called := false
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, string) error {
			called = true
			return nil
		}}
		// Empty aggregate and a reviewer that produced no narrative => nothing to report.
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", postTrailSummary(""), "")
		if called {
			t.Error("must not post when there is nothing to report")
		}
		if !strings.Contains(out.String(), "Nothing to report") {
			t.Errorf("expected a 'nothing to report' confirmation, got %q", out.String())
		}
	})

	t.Run("trail mode surfaces a posting error", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, string) error {
			return errors.New("boom")
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", postTrailSummary("a finding"), "")
		if !strings.Contains(out.String(), "Could not post the review to the trail") {
			t.Errorf("expected an error confirmation, got %q", out.String())
		}
	})

	t.Run("trail mode does not double print already-rendered auth errors", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		deps := Deps{PostReviewToTrail: func(_ context.Context, w io.Writer, _, _ string) error {
			fmt.Fprintln(w, "Not logged in. Run 'entire login' to authenticate.")
			return postTrailAlreadyPrintedError{err: errors.New("not logged in")}
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", postTrailSummary("a finding"), "")
		got := out.String()
		if strings.Count(got, "Not logged in") != 1 {
			t.Fatalf("login hint count in output = %d, want 1; output: %q", strings.Count(got, "Not logged in"), got)
		}
		if strings.Contains(got, "Could not post the review to the trail") {
			t.Fatalf("already-rendered auth error was double printed: %q", got)
		}
	})

	t.Run("cancelled run stays silent", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		summary := postTrailSummary("a finding")
		summary.Cancelled = true
		maybePostReviewToTrail(context.Background(), &out, Deps{}, ReviewOutputTrail, "general", summary, "verdict")
		if out.Len() != 0 {
			t.Errorf("cancelled run should print nothing, got %q", out.String())
		}
	})
}
