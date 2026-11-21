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
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, TrailReport) error {
			called = true
			return nil
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputLocal, "general", TrailScope{}, postTrailSummary("a finding"), "")
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
		deps := Deps{PostReviewToTrail: func(_ context.Context, w io.Writer, _ string, report TrailReport) error {
			gotVerdict = report.Prose
			fmt.Fprintln(w, "Posted the review verdict to trail #1 as a finding.")
			fmt.Fprintln(w, "View the trail: https://entire.io/gh/o/r/trails/1/b")
			return nil
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, postTrailSummary("real finding"), "the verdict")
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
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, TrailReport) error {
			called = true
			return nil
		}}
		// Empty aggregate and a reviewer that produced no narrative => nothing to report.
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, postTrailSummary(""), "")
		if called {
			t.Error("must not post when there is nothing to report")
		}
		if !strings.Contains(out.String(), "Nothing to report") {
			t.Errorf("expected a 'nothing to report' confirmation, got %q", out.String())
		}
	})

	t.Run("trail mode hands the hook the files the review covered", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		initRepoOnMain(t, dir)
		commitFile(t, dir, "root.go", "package main", "init")
		testutil.GitCheckoutNewBranch(t, dir, "feat/x")
		commitFile(t, dir, "feat.go", "package main", "feat")

		var out bytes.Buffer
		var got TrailReport
		deps := Deps{PostReviewToTrail: func(_ context.Context, _ io.Writer, _ string, report TrailReport) error {
			got = report
			return nil
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", reviewTrailScope(t.Context(), io.Discard, ReviewOutputTrail, dir, defaultBranchName), postTrailSummary("a finding"), "the verdict")
		requireFileSet(t, got.Scope.Quotable, []string{"feat.go"}, []string{"root.go"})
		requireFileSet(t, got.Scope.Tracked, []string{"root.go"}, nil)
	})

	t.Run("trail mode surfaces a posting error", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, TrailReport) error {
			return errors.New("boom")
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, postTrailSummary("a finding"), "")
		if !strings.Contains(out.String(), "Could not post the review to the trail") {
			t.Errorf("expected an error confirmation, got %q", out.String())
		}
	})

	t.Run("trail mode does not double print already-rendered auth errors", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		deps := Deps{PostReviewToTrail: func(_ context.Context, w io.Writer, _ string, _ TrailReport) error {
			fmt.Fprintln(w, "Not logged in. Run 'entire login' to authenticate.")
			return postTrailAlreadyPrintedError{err: errors.New("not logged in")}
		}}
		maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, postTrailSummary("a finding"), "")
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
		maybePostReviewToTrail(context.Background(), &out, Deps{}, ReviewOutputTrail, "general", TrailScope{}, summary, "verdict")
		if out.Len() != 0 {
			t.Errorf("cancelled run should print nothing, got %q", out.String())
		}
	})
}

// A trail-mode run that cannot anchor findings to lines says so once, so a
// finding posted against its file or the whole change is not a surprise.
func TestReviewTrailScopeSaysWhenLineAnchorsAreLimited(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepoOnMain(t, dir)
	commitFile(t, dir, "root.go", "package main", "init")

	tests := []struct {
		name, outputMode, root, baseRef, wantOut string
	}{
		{name: "base found", outputMode: ReviewOutputTrail, root: dir, baseRef: defaultBranchName},
		{name: "no base", outputMode: ReviewOutputTrail, root: dir, wantOut: "No review base found"},
		{name: "git fails", outputMode: ReviewOutputTrail, root: t.TempDir(), baseRef: defaultBranchName, wantOut: "Could not list the files under review"},
		{name: "local output", outputMode: ReviewOutputLocal, root: t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			reviewTrailScope(t.Context(), &out, tt.outputMode, tt.root, tt.baseRef)
			if tt.wantOut == "" && out.Len() != 0 {
				t.Errorf("output = %q, want none", out.String())
			}
			if tt.wantOut != "" && (!strings.Contains(out.String(), tt.wantOut) || strings.Count(out.String(), "\n") != 1) {
				t.Errorf("output = %q, want one line containing %q", out.String(), tt.wantOut)
			}
		})
	}
}
