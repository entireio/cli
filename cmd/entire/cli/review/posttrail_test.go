package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputLocal, "general", TrailScope{}, "", postTrailSummary("a finding"), ""); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
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
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "", postTrailSummary("real finding"), "the verdict"); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
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
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "", postTrailSummary(""), ""); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
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
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", reviewTrailScope(t.Context(), io.Discard, ReviewOutputTrail, dir, defaultBranchName), "", postTrailSummary("a finding"), "the verdict"); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
		requireFileSet(t, got.Scope.Quotable, []string{"feat.go"}, []string{"root.go"})
		requireFileSet(t, got.Scope.Tracked, []string{"root.go"}, nil)
	})

	t.Run("trail mode surfaces a posting error", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		deps := Deps{PostReviewToTrail: func(context.Context, io.Writer, string, TrailReport) error {
			return errors.New("boom")
		}}
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "", postTrailSummary("a finding"), ""); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
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
		if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "", postTrailSummary("a finding"), ""); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
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
		if err := maybePostReviewToTrail(context.Background(), &out, Deps{}, ReviewOutputTrail, "general", TrailScope{}, "", summary, "verdict"); err != nil {
			t.Fatalf("maybePostReviewToTrail: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("cancelled run should print nothing, got %q", out.String())
		}
	})
}

// A trail-mode run that cannot anchor findings to lines says so once, so a
// finding posted against its file or the whole change is not a surprise.
// A --trail run fails whenever its report is not posted, including when it
// was cancelled, while a profile's output: trail keeps the run's result.
func TestMaybePostReviewToTrailRequiredByTrailFlag(t *testing.T) {
	t.Parallel()
	silent := func(err error) error { return postTrailAlreadyPrintedError{err: err} }
	failing := func(context.Context, io.Writer, string, TrailReport) error { return errors.New("head moved on") }
	cancelled := postTrailSummary("a finding")
	cancelled.Cancelled = true
	for _, tt := range []struct {
		name    string
		deps    Deps
		summary reviewtypes.RunSummary
		wantErr string
		printed bool
	}{
		{"post fails", Deps{PostReviewToTrail: failing}, postTrailSummary("a finding"), "head moved on", true},
		{"no poster", Deps{}, postTrailSummary("a finding"), "trail output is not available", true},
		{"cancelled", Deps{PostReviewToTrail: failing}, cancelled, "the review was cancelled", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			deps := tt.deps
			deps.NewSilentError = silent
			if err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "abc", tt.summary, ""); err != nil {
				t.Fatalf("a profile's output: trail failed the run: %v", err)
			}
			deps.trail = &explicitTrailRun{}
			err := maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", TrailScope{}, "abc", tt.summary, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got := userMessageAlreadyPrinted(err); got != tt.printed {
				t.Errorf("already printed = %v, want %v (%v)", got, tt.printed, err)
			}
		})
	}
}

// A --trail run that ends before settling its report, as at a declined
// prompt, fails; one that posted or had nothing to report keeps its result.
func TestTrailRunResult(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	failed := errors.New("review run failed")
	if err := trailRunResult(cmd, &explicitTrailRun{}, nil); err == nil || !strings.Contains(err.Error(), "the review did not run") {
		t.Errorf("unsettled run: err = %v", err)
	}
	if err := trailRunResult(cmd, &explicitTrailRun{settled: true}, nil); err != nil {
		t.Errorf("settled run: err = %v", err)
	}
	if err := trailRunResult(cmd, &explicitTrailRun{}, failed); !errors.Is(err, failed) {
		t.Errorf("failed run: err = %v, want its own error", err)
	}
}

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
