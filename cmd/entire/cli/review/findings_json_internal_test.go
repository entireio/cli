package review

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const findingsJSONTestHandle = "claude-session"

// The manifest keeps what a trail post would read from the whole run, and
// why a run left no report, so --findings --json can tell a review that
// found nothing from one that left nothing to read. A run that did not ask
// for the trail JSON line keeps each reviewer's final report instead, with
// a clean verdict counted as nothing to report and a trailing JSON block
// kept as the reviewer's text.
func TestBuildLocalReviewManifestFromSummary_KeepsTrailReport(t *testing.T) {
	t.Parallel()
	failed := reviewtypes.RunSummary{AgentRuns: []reviewtypes.AgentRun{{Name: "claude-code", Status: reviewtypes.AgentStatusFailed}}}
	narrated := func(events ...reviewtypes.Event) reviewtypes.RunSummary {
		return trailRunsSummary(trailRun{name: manifestTestCodexAgent, events: events})
	}
	tool := reviewtypes.ToolCall{Name: "read_file"}
	const finding = "- [high] retry.go:42 — loop ignores ctx cancel"
	tests := []struct {
		name      string
		summary   reviewtypes.RunSummary
		aggregate string
		local     bool
		want      ManifestReport
	}{
		{
			name:    "structured findings",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "[high] Loop ignores ctx cancel.\n"+oneHighComment)),
			want: ManifestReport{Comments: []TrailComment{{
				Severity: "high",
				Body:     "Loop ignores ctx cancel.",
				Location: TrailLocation{Granularity: "line", FilePath: "retry.go", StartLine: 42},
			}}},
		},
		{
			name:    "nothing to report",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "approve\n"+emptyTrailJSON)),
			want:    ManifestReport{Comments: []TrailComment{}},
		},
		{
			name:    "prose without a contract",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, finding)),
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\n" + finding},
		},
		{
			name:    "a trail run's clean verdict without its line",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "approve — no actionable findings")),
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\napprove — no actionable findings"},
		},
		{
			name:    "no output",
			summary: failed,
			want:    ManifestReport{Comments: []TrailComment{}, Unreadable: "no reviewer finished with output"},
		},
		{
			name:      "no readable report",
			summary:   failed,
			aggregate: `{"comments":"none"}`,
			want:      ManifestReport{Comments: []TrailComment{}, Unreadable: "the review produced no readable report"},
		},
		{
			name:    "local clean verdict",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "**Verdict:** approve. The change is small and tested.")),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}},
		},
		{
			name:    "local narration before a clean verdict",
			summary: narrated(reviewtypes.AssistantText{Text: "I'll check:\n- retry.go"}, tool, reviewtypes.AssistantText{Text: "approve — no actionable findings"}),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}},
		},
		{
			name:    "local narration before findings",
			summary: narrated(reviewtypes.AssistantText{Text: "I'll check:\n- retry.go"}, tool, reviewtypes.AssistantText{Text: "request changes\n\n" + finding}),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\nrequest changes\n\n" + finding},
		},
		{
			name:    "local findings before a last tool call",
			summary: narrated(reviewtypes.AssistantText{Text: finding}, tool, reviewtypes.AssistantText{Text: "Done."}),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\n" + finding + "\nDone."},
		},
		{
			name:    "local plain-paragraph finding before a last tool call",
			summary: narrated(reviewtypes.AssistantText{Text: "The retry loop ignores ctx cancel."}, tool, reviewtypes.AssistantText{Text: "Done."}),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\nThe retry loop ignores ctx cancel.\nDone."},
		},
		{
			name:    "local approval followed by a plain-paragraph finding",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "approve\n\nThe retry loop in retry.go:42 can hang shutdown.")),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\napprove\n\nThe retry loop in retry.go:42 can hang shutdown."},
		},
		{
			name:    "local approval with nits inside emphasis",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "Verdict: **approve** with nits — rename the flag")),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\nVerdict: **approve** with nits — rename the flag"},
		},
		{
			name:    "local finding as a plain paragraph",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, "The retry loop ignores ctx cancel.")),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\nThe retry loop ignores ctx cancel."},
		},
		{
			name:    "local trailing JSON is the reviewer's text",
			summary: trailRunsSummary(textRun(manifestTestCodexAgent, finding+"\n"+emptyTrailJSON)),
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Prose: "## codex\n\n" + finding + "\n" + emptyTrailJSON},
		},
		{
			name: "local clean reviewer beside one with findings",
			summary: trailRunsSummary(
				textRun(manifestTestCodexAgent, "approve"),
				textRun("claude-code", finding),
			),
			local: true,
			want:  ManifestReport{Comments: []TrailComment{}, Prose: "## claude-code\n\n" + finding},
		},
		{
			name:      "local judge's clean verdict",
			summary:   trailRunsSummary(textRun(manifestTestCodexAgent, finding)),
			aggregate: "approve — the reported issue is unsupported",
			local:     true,
			want:      ManifestReport{Comments: []TrailComment{}},
		},
		{
			name:    "local no output",
			summary: failed,
			local:   true,
			want:    ManifestReport{Comments: []TrailComment{}, Unreadable: "no reviewer finished with output"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", tt.summary, nil, tt.aggregate, !tt.local)
			if manifest.Report == nil {
				t.Fatal("manifest kept no report")
			}
			if manifest.TrailFindingsJSON == tt.local {
				t.Fatalf("TrailFindingsJSON = %v, want %v", manifest.TrailFindingsJSON, !tt.local)
			}
			if !reflect.DeepEqual(*manifest.Report, tt.want) {
				t.Fatalf("report = %#v, want %#v", *manifest.Report, tt.want)
			}
		})
	}
}

// writeFindingsJSONTestManifest saves one review run with report in a new
// repository and changes into it.
func writeFindingsJSONTestManifest(t *testing.T, report *ManifestReport) {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "init")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	if err := writeLocalReviewManifest(context.Background(), LocalReviewManifest{
		CreatedAt:   time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		StartingSHA: "abc123",
		Sources:     []ManifestSource{{SessionID: findingsJSONTestHandle, Label: "Claude Code", Output: manifestTestFinding}},
		Report:      report,
	}); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}
}

func runReviewCommand(deps Deps, args ...string) (stdout, stderr string, err error) {
	cmd := NewCommand(deps)
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestReviewFindingsCommand_JSONPrintsTheSavedReportsFindings(t *testing.T) {
	saved := &ManifestReport{
		Comments: []TrailComment{{Severity: "high", Body: "Loop ignores ctx cancel.", Location: TrailLocation{Granularity: "line", FilePath: "retry.go", StartLine: 42}}},
		Prose:    "- nit: rename n",
	}
	writeFindingsJSONTestManifest(t, saved)
	confidence := 0.9
	want := []Finding{{Severity: "high", Confidence: &confidence, File: "retry.go", Line: 42, Body: "Loop ignores ctx cancel."}}
	var split TrailReport
	deps := Deps{TrailFindings: func(report TrailReport) []Finding {
		split = report
		return want
	}}

	stdout, stderr, err := runReviewCommand(deps, manifestFindingsFlag, findingsJSONTestHandle, "--json")
	if err != nil {
		t.Fatalf("review --findings --json: %v\nstderr:\n%s", err, stderr)
	}
	if !reflect.DeepEqual(split, TrailReport{Comments: saved.Comments, Prose: saved.Prose}) {
		t.Fatalf("split report = %#v, want the saved one", split)
	}
	var doc reviewFindingsJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	wantDoc := reviewFindingsJSON{
		Handle:    "20260507T100000",
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		HeadSHA:   "abc123",
		Findings:  want,
	}
	if !reflect.DeepEqual(doc, wantDoc) {
		t.Fatalf("printed %#v, want %#v", doc, wantDoc)
	}
}

// A run with nothing to report prints an empty list, not null, so a script
// can range over it.
func TestReviewFindingsCommand_JSONPrintsAnEmptyListForACleanReview(t *testing.T) {
	writeFindingsJSONTestManifest(t, &ManifestReport{Comments: []TrailComment{}})
	deps := Deps{TrailFindings: func(TrailReport) []Finding { return nil }}

	stdout, stderr, err := runReviewCommand(deps, manifestFindingsFlag, findingsJSONTestHandle, "--json")
	if err != nil {
		t.Fatalf("review --findings --json: %v\nstderr:\n%s", err, stderr)
	}
	var doc reviewFindingsJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if doc.Handle != "20260507T100000" || doc.Findings == nil || len(doc.Findings) != 0 {
		t.Fatalf("printed %#v, want handle 20260507T100000 and an empty findings list", doc)
	}
	if !strings.Contains(stdout, `"findings": []`) {
		t.Fatalf("findings should print as an empty list, not null:\n%s", stdout)
	}
}

// Two runs that started in the same second share their time handle, so
// the printed handle is one that selects only the printed run.
func TestReviewFindingsCommand_JSONPrintsAHandleThatSelectsOnlyThatRun(t *testing.T) {
	writeFindingsJSONTestManifest(t, &ManifestReport{Comments: []TrailComment{}})
	if err := writeLocalReviewManifest(context.Background(), LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources:   []ManifestSource{{SessionID: "codex-session", Label: "Codex", Output: manifestTestFinding}},
		Report:    &ManifestReport{Comments: []TrailComment{}},
	}); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}
	deps := Deps{TrailFindings: func(TrailReport) []Finding { return nil }}

	stdout, stderr, err := runReviewCommand(deps, manifestFindingsFlag, findingsJSONTestHandle, "--json")
	if err != nil {
		t.Fatalf("review --findings --json: %v\nstderr:\n%s", err, stderr)
	}
	var doc reviewFindingsJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if doc.Handle != findingsJSONTestHandle {
		t.Fatalf("handle = %q, want %q: the shared time handle selects both runs", doc.Handle, findingsJSONTestHandle)
	}
}

// A run that left no report, or was saved before its report was kept, fails
// with nothing on stdout, so a script never reads it as a clean review.
func TestReviewFindingsCommand_JSONRefusesARunWithoutAReport(t *testing.T) {
	tests := []struct {
		name   string
		report *ManifestReport
		want   string
	}{
		{"saved before reports were kept", nil, "saved before --json could read them"},
		{"no output", &ManifestReport{Comments: []TrailComment{}, Unreadable: "no reviewer finished with output"}, "no findings to print: no reviewer finished with output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeFindingsJSONTestManifest(t, tt.report)
			called := false
			deps := Deps{TrailFindings: func(TrailReport) []Finding {
				called = true
				return nil
			}}

			stdout, stderr, err := runReviewCommand(deps, manifestFindingsFlag, findingsJSONTestHandle, "--json")
			if err == nil {
				t.Fatal("expected review --findings --json to fail")
			}
			if stdout != "" {
				t.Fatalf("stdout should be empty, got:\n%s", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr should say %q, got:\n%s", tt.want, stderr)
			}
			if called {
				t.Fatal("the report was split although the run left none")
			}
		})
	}
}

func TestReviewJSONFlagNeedsShowConfigOrAFindingsHandle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"alone", []string{"--json"}, "--json requires --show-config or --findings <handle>"},
		{"findings without a handle", []string{manifestFindingsFlag, "--json"}, "--findings --json needs a handle"},
		{"findings with an empty handle", []string{manifestFindingsFlag, "", "--json"}, "--findings --json needs a handle"},
		{"findings with a blank handle", []string{manifestFindingsFlag, "  ", "--json"}, "--findings --json needs a handle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := runReviewCommand(Deps{}, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
