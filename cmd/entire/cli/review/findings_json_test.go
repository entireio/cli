package review_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	cli "github.com/entireio/cli/cmd/entire/cli"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// A saved run's findings print as JSON through the production wiring, as a
// trail post would send them: a reviewer's structured comment as given,
// with its severity normalized and its path cleaned, and another reviewer's
// prose split into one finding per item, with the severity and location
// its text gives.
func TestReviewFindingsJSON_PrintsWhatATrailPostWouldSend(t *testing.T) {
	setupCmdTestRepo(t)
	contract := `{"summary":"1 issue","comments":[{"severity":"P2","confidence":0.7,"body":"Retry leaks the timer.","location":{"granularity":"range","file_path":"./a.go","start_line":3,"end_line":5}}]}`
	summary := reviewtypes.RunSummary{AgentRuns: []reviewtypes.AgentRun{
		{
			Name:   "codex",
			Status: reviewtypes.AgentStatusSucceeded,
			Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "[medium] Retry leaks the timer.\n" + contract}},
		},
		{
			Name:   "claude-code",
			Status: reviewtypes.AgentStatusSucceeded,
			Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "- [high] retry.go:42 — loop ignores ctx cancel\n- nit: rename `n`"}},
		},
	}}
	created := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	if err := review.ExposedSaveReviewRun(context.Background(), created, "abc123", summary, "", true); err != nil {
		t.Fatalf("save review run: %v", err)
	}

	doc := runFindingsJSON(t, "codex-session")
	if doc.Handle != "20260507T100000" || doc.HeadSHA != "abc123" {
		t.Fatalf("handle, head_sha = %q, %q; want 20260507T100000, abc123", doc.Handle, doc.HeadSHA)
	}
	want := []map[string]any{
		{"severity": "medium", "confidence": 0.7, "file": "a.go", "line": 3.0, "end_line": 5.0, "body": "Retry leaks the timer."},
		{"severity": "high", "file": "retry.go", "line": 42.0, "body": "[high] retry.go:42 — loop ignores ctx cancel"},
		{"severity": "low", "body": "nit: rename `n`"},
	}
	if !reflect.DeepEqual(doc.Findings, want) {
		t.Fatalf("findings = %#v\nwant %#v", doc.Findings, want)
	}
}

// A local profile's clean review, which has no trail JSON line to say so,
// prints an empty list rather than its verdict, or the exploration
// narration before it, as findings, whether a reviewer's verdict or a
// judge's decides.
func TestReviewFindingsJSON_PrintsNoFindingsForACleanLocalReview(t *testing.T) {
	tests := []struct {
		name      string
		runs      []reviewtypes.AgentRun
		aggregate string
	}{
		{
			name: "reviewer verdicts",
			runs: []reviewtypes.AgentRun{
				{Name: "codex", Status: reviewtypes.AgentStatusSucceeded, Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "approve — no actionable findings"}}},
				{Name: "claude-code", Status: reviewtypes.AgentStatusSucceeded, Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "**Verdict:** approve. The change is small and tested."}}},
			},
		},
		{
			name: "narration before the verdict",
			runs: []reviewtypes.AgentRun{
				{Name: "codex", Status: reviewtypes.AgentStatusSucceeded, Buffer: []reviewtypes.Event{
					reviewtypes.AssistantText{Text: "I'll check:\n- retry.go\n- main.go"},
					reviewtypes.ToolCall{Name: "read_file"},
					reviewtypes.AssistantText{Text: "approve — no actionable findings"},
				}},
			},
		},
		{
			name: "judge verdict",
			runs: []reviewtypes.AgentRun{
				{Name: "codex", Status: reviewtypes.AgentStatusSucceeded, Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "- [low] a.go:3 — maybe rename"}}},
			},
			aggregate: "approve — the reported rename is stylistic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupCmdTestRepo(t)
			created := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
			summary := reviewtypes.RunSummary{AgentRuns: tt.runs}
			if err := review.ExposedSaveReviewRun(context.Background(), created, "abc123", summary, tt.aggregate, false); err != nil {
				t.Fatalf("save review run: %v", err)
			}

			doc := runFindingsJSON(t, "codex-session")
			if doc.Findings == nil || len(doc.Findings) != 0 {
				t.Fatalf("findings = %#v, want an empty list", doc.Findings)
			}
		})
	}
}

type findingsJSONDoc struct {
	Handle   string           `json:"handle"`
	HeadSHA  string           `json:"head_sha"`
	Findings []map[string]any `json:"findings"`
}

// runFindingsJSON runs `entire review --findings <handle> --json` through the
// root command and decodes what it printed.
func runFindingsJSON(t *testing.T, handle string) findingsJSONDoc {
	t.Helper()
	rootCmd := cli.NewRootCmd()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs([]string{"review", "--findings", handle, "--json"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("review --findings --json: %v\nstderr:\n%s", err, errOut.String())
	}
	var doc findingsJSONDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	return doc
}
