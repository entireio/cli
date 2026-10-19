package review

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

const (
	emptyTrailJSON  = `{"summary":"No issues","comments":[]}`
	oneHighComment  = `{"summary":"1 issue","comments":[{"severity":"high","body":"Loop ignores ctx cancel.","location":{"granularity":"line","file_path":"retry.go","start_line":42}}]}`
	twoTrailComment = `{"summary":"2 issues","comments":[{"severity":"high","body":"A.","location":{"granularity":"line","file_path":"a.go","start_line":1}},{"severity":"low","body":"B.","location":{"granularity":"whole_change"}}]}`
)

type trailRun struct {
	name   string
	events []reviewtypes.Event
}

func textRun(name, narrative string) trailRun {
	return trailRun{name: name, events: []reviewtypes.Event{reviewtypes.AssistantText{Text: narrative}}}
}

func trailRunsSummary(runs ...trailRun) reviewtypes.RunSummary {
	summary := reviewtypes.RunSummary{}
	for _, run := range runs {
		summary.AgentRuns = append(summary.AgentRuns, reviewtypes.AgentRun{
			Name:   run.name,
			Status: reviewtypes.AgentStatusSucceeded,
			Buffer: run.events,
		})
	}
	return summary
}

// postedTrailReport runs the trail-mode posting decision and returns the
// report handed to the hook, whether the hook ran, and the printed output.
func postedTrailReport(t *testing.T, summary reviewtypes.RunSummary, aggregate string) (TrailReport, bool, string) {
	t.Helper()
	var out bytes.Buffer
	var got TrailReport
	called := false
	deps := Deps{PostReviewToTrail: func(_ context.Context, _ io.Writer, _ string, report TrailReport) error {
		called = true
		got = report
		return nil
	}}
	maybePostReviewToTrail(context.Background(), &out, deps, ReviewOutputTrail, "general", summary, aggregate)
	return got, called, out.String()
}

func requireNotPosted(t *testing.T, called bool, out string) {
	t.Helper()
	if called {
		t.Fatal("a clean review must not be posted to the trail")
	}
	if !strings.Contains(out, "Nothing to report") {
		t.Fatalf("expected a 'Nothing to report' notice, got %q", out)
	}
}

func requireUnreadableNotPosted(t *testing.T, called bool, out string) {
	t.Helper()
	if called {
		t.Fatal("an empty report must not be handed to the trail hook")
	}
	if !strings.Contains(out, "no readable report") {
		t.Fatalf("expected a 'no readable report' notice, got %q", out)
	}
}

func requirePostedProse(t *testing.T, report TrailReport, called bool, wantSubstr string) {
	t.Helper()
	if !called {
		t.Fatal("expected the review to be posted")
	}
	if len(report.Comments) != 0 {
		t.Fatalf("expected a prose report, got %d structured comments", len(report.Comments))
	}
	if !strings.Contains(report.Prose, wantSubstr) {
		t.Fatalf("prose %q does not contain %q", report.Prose, wantSubstr)
	}
	if strings.Contains(report.Prose, `"comments"`) {
		t.Fatalf("the JSON contract line must be removed before posting prose, got %q", report.Prose)
	}
}

func TestTrailReportFromJudge(t *testing.T) {
	t.Parallel()
	reviewers := trailRunsSummary(
		textRun("claude-code", "approve — no actionable findings\n"+emptyTrailJSON),
		textRun("codex", "approve — looks good\n"+emptyTrailJSON),
	)

	t.Run("empty comments array is clean", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, reviewers, "approve — no actionable findings\n"+emptyTrailJSON)
		requireNotPosted(t, called, out)
		if strings.Contains(out, "JSON") {
			t.Errorf("the notice must not mention the internal JSON line, got %q", out)
		}
	})

	t.Run("comments are posted as structured findings", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "request changes — two issues\n- [high] a.go:1 — A.\n- [low] B.\n"+twoTrailComment)
		if !called || len(report.Comments) != 2 {
			t.Fatalf("called=%v comments=%d, want 2 structured comments", called, len(report.Comments))
		}
		first := report.Comments[0]
		if first.Severity != "high" || first.Body != "A." || first.Location.FilePath != "a.go" || first.Location.StartLine != 1 {
			t.Errorf("first comment = %+v, want high A. at a.go:1", first)
		}
	})

	t.Run("prose without a JSON line is posted as before", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve — no actionable findings")
		requirePostedProse(t, report, called, "approve — no actionable findings")
	})

	t.Run("fenced JSON line is accepted", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, reviewers, "approve — no actionable findings\n```json\n"+emptyTrailJSON+"\n```")
		requireNotPosted(t, called, out)
	})

	t.Run("pretty-printed JSON block is accepted", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, reviewers, "approve — no actionable findings\n{\n  \"summary\": \"No issues\",\n  \"comments\": []\n}")
		requireNotPosted(t, called, out)
	})

	t.Run("a JSON-only reply with null comments posts its summary, not raw JSON", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, `{"summary":"approve — nothing found","comments":null}`)
		requirePostedProse(t, report, called, "approve — nothing found")
	})

	t.Run("a JSON-only reply with nothing readable is not handed to the hook", func(t *testing.T) {
		t.Parallel()
		jsonOnly := trailRunsSummary(textRun("claude-code", `{"comments":null}`))
		_, called, out := postedTrailReport(t, jsonOnly, `{"summary":"","comments":"none"}`)
		requireUnreadableNotPosted(t, called, out)
	})

	t.Run("comments missing a finding the prose reports post the prose", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "request changes\n- [high] a.go:1 — A.\n- [high] b.go:2 — B.\n- [high] c.go:3 — C.\n"+oneHighComment)
		requirePostedProse(t, report, called, "c.go:3")
	})

	t.Run("null comments is not clean", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve — fine\n"+`{"summary":"","comments":null}`)
		requirePostedProse(t, report, called, "approve — fine")
	})

	t.Run("missing comments key is not clean", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve — fine\n"+`{"summary":"approve, no \"comments\""}`)
		requirePostedProse(t, report, called, "approve — fine")
	})

	t.Run("a comment with an empty body is skipped, not the whole array", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "request changes\n- [high] real bug\n"+`{"summary":"","comments":[{"severity":"high","body":"real bug"},{"severity":"low","body":"  "}]}`)
		if !called || len(report.Comments) != 1 || report.Comments[0].Body != "real bug" {
			t.Fatalf("called=%v report=%+v, want only the non-blank comment posted", called, report)
		}
	})

	t.Run("only empty-body comments is clean", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, reviewers, "approve — fine\n"+`{"summary":"","comments":[{"severity":"high","body":"  "}]}`)
		requireNotPosted(t, called, out)
	})

	t.Run("empty JSON with a finding bullet in the prose is posted", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve\n- the retry loop leaks a goroutine\n"+emptyTrailJSON)
		requirePostedProse(t, report, called, "the retry loop leaks a goroutine")
	})

	t.Run("empty JSON with an indented finding bullet is posted", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve\n   * [medium] cmd.go:10 — leak\n"+emptyTrailJSON)
		requirePostedProse(t, report, called, "leak")
	})

	t.Run("empty JSON with a request changes verdict is posted", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "**Request changes** — the lock leaks on error\n"+emptyTrailJSON)
		requirePostedProse(t, report, called, "the lock leaks on error")
	})

	t.Run("empty JSON with an approve with nits verdict is posted", func(t *testing.T) {
		t.Parallel()
		report, called, _ := postedTrailReport(t, reviewers, "approve with nits — rename x\n"+emptyTrailJSON)
		requirePostedProse(t, report, called, "rename x")
	})

	for name, prose := range map[string]string{
		"labeled request changes verdict": "**Verdict:** request changes — retry ignores ctx",
		"labeled approve with nits":       "Verdict: approve with nits — rename x",
		"plural nits label":               "Nits: rename x",
		"bold suggestions label":          "**Suggestions:** rename x",
		"issues label":                    "Issues: retry ignores ctx",
		"priority severity tag":           "[p1] retry.go:42 — retry ignores ctx",
		"delimited severity marker":       "P2: retry ignores ctx",
		"dashed severity marker":          "high — retry ignores ctx",
		"needs changes verdict":           "Verdict: needs changes",
		"needs work verdict":              "needs work — retry ignores ctx",
		"blocking label":                  "Blocking: ctx ignored in retry loop",
		"findings label":                  "Findings:\nThe retry loop ignores ctx.",
		"problems label":                  "Problems:\nRetry ignores ctx.",
		"unicode bullet":                  "• foo.go:3 bug",
		"severity heading":                "### High\nfoo.go:3 bug",
		"bold severity":                   "**High** foo.go:3 bug",
	} {
		t.Run("empty JSON with "+name+" is posted", func(t *testing.T) {
			t.Parallel()
			report, called, _ := postedTrailReport(t, reviewers, prose+"\n"+emptyTrailJSON)
			requirePostedProse(t, report, called, prose)
		})
	}

	for name, prose := range map[string]string{
		"labeled approve verdict":   "**Verdict:** approve — no actionable findings",
		"no issues prose":           "approve\nNo issues found.",
		"word starting with marker": "approve\nHighlights: clean refactor.",
		"bold word after a marker":  "approve\n**Low risk** refactor.",
	} {
		t.Run("empty JSON with "+name+" is clean", func(t *testing.T) {
			t.Parallel()
			_, called, out := postedTrailReport(t, reviewers, prose+"\n"+emptyTrailJSON)
			requireNotPosted(t, called, out)
		})
	}
}

// The judge is asked to drop unsupported claims, so its verdict decides what
// is posted even when a reviewer reported a high-severity comment.
func TestTrailReportJudgeDecidesOverReviewerComments(t *testing.T) {
	t.Parallel()
	summary := trailRunsSummary(
		textRun("claude-code", "request changes\n- [high] retry.go:42 — Loop ignores ctx cancel.\n"+oneHighComment),
		textRun("codex", "approve\n"+emptyTrailJSON),
	)
	_, called, out := postedTrailReport(t, summary, "approve — no actionable findings\n"+emptyTrailJSON)
	requireNotPosted(t, called, out)
}

func TestTrailReportWithoutJudge(t *testing.T) {
	t.Parallel()

	t.Run("single clean reviewer is not posted", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, trailRunsSummary(textRun("claude-code", "approve — no actionable findings\n"+emptyTrailJSON)), "")
		requireNotPosted(t, called, out)
	})

	t.Run("a JSON-only reply with nothing readable is not handed to the hook", func(t *testing.T) {
		t.Parallel()
		_, called, out := postedTrailReport(t, trailRunsSummary(textRun("claude-code", `{"comments":null}`)), "")
		requireUnreadableNotPosted(t, called, out)
	})

	t.Run("comments missing a finding the prose reports post the prose", func(t *testing.T) {
		t.Parallel()
		run := textRun("claude-code", "request changes\n- [high] a.go:1 — A.\n- [high] b.go:2 — B.\n"+oneHighComment)
		report, called, _ := postedTrailReport(t, trailRunsSummary(run), "")
		requirePostedProse(t, report, called, "b.go:2")
	})

	t.Run("each reviewer's JSON is read, not only the last one", func(t *testing.T) {
		t.Parallel()
		summary := trailRunsSummary(
			textRun("claude-code", "request changes\n- [high] retry.go:42 — Loop ignores ctx cancel.\n"+oneHighComment),
			textRun("codex", "approve — no actionable findings\n"+emptyTrailJSON),
		)
		report, called, _ := postedTrailReport(t, summary, "")
		if !called || len(report.Comments) != 1 || report.Comments[0].Location.FilePath != "retry.go" {
			t.Fatalf("called=%v report=%+v, want the first reviewer's comment posted", called, report)
		}
	})

	t.Run("all reviewers clean is not posted", func(t *testing.T) {
		t.Parallel()
		summary := trailRunsSummary(
			textRun("claude-code", "approve\n"+emptyTrailJSON),
			textRun("codex", "approve\n"+emptyTrailJSON),
		)
		_, called, out := postedTrailReport(t, summary, "")
		requireNotPosted(t, called, out)
	})

	t.Run("a reviewer without JSON posts the combined prose", func(t *testing.T) {
		t.Parallel()
		summary := trailRunsSummary(
			textRun("claude-code", "approve\n"+emptyTrailJSON),
			textRun("codex", "approve — looks fine"),
		)
		report, called, _ := postedTrailReport(t, summary, "")
		requirePostedProse(t, report, called, "## codex")
	})

	t.Run("streamed text deltas join into one parsable JSON line", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "pi", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "approve — no actionable findings\n"},
			reviewtypes.AssistantText{Text: `{"summary":"No issues",`, Delta: true},
			reviewtypes.AssistantText{Text: `"comments":[]}`, Delta: true},
		}}
		_, called, out := postedTrailReport(t, trailRunsSummary(run), "")
		requireNotPosted(t, called, out)
	})

	t.Run("narration bullets before a tool call do not contradict a clean final message", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "claude-code", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "I'll check the changed files:\n- retry.go\n- retry_test.go"},
			reviewtypes.ToolCall{Name: "Read"},
			reviewtypes.AssistantText{Text: "approve — no actionable findings\n" + emptyTrailJSON},
		}}
		_, called, out := postedTrailReport(t, trailRunsSummary(run), "")
		requireNotPosted(t, called, out)
	})

	t.Run("a finding bullet in the report posts only the report", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "claude-code", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "I'll check the changed files:\n- retry.go"},
			reviewtypes.ToolCall{Name: "Read"},
			reviewtypes.AssistantText{Text: "approve\n- [low] retry.go:42 — the lock leaks on error.\n" + emptyTrailJSON},
			reviewtypes.ToolCall{Name: "Read"},
			reviewtypes.AssistantText{Text: "Done."},
		}}
		report, called, _ := postedTrailReport(t, trailRunsSummary(run), "")
		requirePostedProse(t, report, called, "the lock leaks on error")
		for _, excluded := range []string{"I'll check the changed files", "Done."} {
			if strings.Contains(report.Prose, excluded) {
				t.Errorf("prose %q must hold only the report, not %q", report.Prose, excluded)
			}
		}
	})

	t.Run("a report before a tool call is read with a JSON-only final message", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "claude-code", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "request changes\n- [high] retry.go:42 — loop ignores ctx."},
			reviewtypes.ToolCall{Name: "TodoWrite"},
			reviewtypes.AssistantText{Text: emptyTrailJSON},
		}}
		report, called, _ := postedTrailReport(t, trailRunsSummary(run), "")
		requirePostedProse(t, report, called, "loop ignores ctx")
	})

	t.Run("narration bullets before a JSON-only final message do not contradict it", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "codex", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "I'll check the changed files:\n- retry.go\n- retry_test.go"},
			reviewtypes.ToolCall{Name: "Read"},
			reviewtypes.AssistantText{Text: emptyTrailJSON},
		}}
		_, called, out := postedTrailReport(t, trailRunsSummary(run), "")
		requireNotPosted(t, called, out)
	})

	t.Run("a feedback verdict before a JSON-only final message contradicts it", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "codex", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "approve with nits\n- retry.go:42 could name the backoff constant."},
			reviewtypes.ToolCall{Name: "TodoWrite"},
			reviewtypes.AssistantText{Text: emptyTrailJSON},
		}}
		report, called, _ := postedTrailReport(t, trailRunsSummary(run), "")
		requirePostedProse(t, report, called, "backoff constant")
	})

	t.Run("a new message starts a new line after a tool call", func(t *testing.T) {
		t.Parallel()
		run := trailRun{name: "pi", events: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "I'll read the diff."},
			reviewtypes.ToolCall{Name: "bash"},
			reviewtypes.AssistantText{Text: "request changes — retry ignores ctx"},
		}}
		report, called, _ := postedTrailReport(t, trailRunsSummary(run), "")
		requirePostedProse(t, report, called, "I'll read the diff.\nrequest changes")
	})
}

func TestTrailFindingsJSONInstructionsExampleParses(t *testing.T) {
	t.Parallel()
	var example string
	for _, line := range strings.Split(trailFindingsJSONInstructions, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			example = line
		}
	}
	if example == "" {
		t.Fatalf("instructions carry no JSON example:\n%s", trailFindingsJSONInstructions)
	}
	_, comments, ok := splitTrailJSON("approve\n" + example)
	if !ok || len(comments) != 1 {
		t.Fatalf("the prompt's own example must parse as one comment: ok=%v comments=%d\n%s", ok, len(comments), example)
	}
}

func TestStripTrailJSONForDisplay(t *testing.T) {
	t.Parallel()
	got := stripTrailJSON("approve — no actionable findings\n"+emptyTrailJSON, true)
	if got != "approve — no actionable findings" {
		t.Errorf("stripTrailJSON = %q, want the verdict line only", got)
	}
	if prose := "plain review text"; stripTrailJSON(prose, true) != prose {
		t.Errorf("text without a JSON line must be unchanged, got %q", stripTrailJSON(prose, true))
	}
	if output := "approve\n" + emptyTrailJSON; stripTrailJSON(output, false) != output {
		t.Errorf("a run that did not ask for the line must keep it, got %q", stripTrailJSON(output, false))
	}
}

type fixedSynthesisProvider string

func (p fixedSynthesisProvider) Synthesize(context.Context, string) (string, error) {
	return string(p), nil
}

// TestTrailJSONHiddenOnlyWhenRequested verifies every display surface hides
// the contract line of a trail run and shows a local run's output unchanged,
// where a trailing JSON block is the reviewer's own text.
func TestTrailJSONHiddenOnlyWhenRequested(t *testing.T) {
	t.Parallel()
	narrative := "approve — no actionable findings"
	output := narrative + "\n" + emptyTrailJSON
	manifest := func(trail bool) LocalReviewManifest {
		return LocalReviewManifest{
			TrailFindingsJSON: trail,
			Sources:           []ManifestSource{{Label: "claude-code", Output: output}},
			AggregateOutput:   output,
		}
	}
	surfaces := map[string]func(trail bool) string{
		"dump": func(trail bool) string {
			var b bytes.Buffer
			DumpSink{W: &b, TrailFindingsJSON: trail}.RunFinished(trailRunsSummary(textRun("codex", output)))
			return b.String()
		},
		"tui detail": func(trail bool) string {
			return strings.Join(buildEventLines([]reviewtypes.Event{reviewtypes.AssistantText{Text: output}}, 200, trail), "\n")
		},
		"judge prompt": func(trail bool) string {
			summary := trailRunsSummary(textRun("codex", output), textRun("claude-code", narrative))
			return composeSynthesisPrompt(summary, synthesisPromptOptions{trailFindingsJSON: trail})
		},
		"judge verdict": func(trail bool) string {
			var b bytes.Buffer
			summary := trailRunsSummary(textRun("codex", narrative), textRun("claude-code", narrative))
			SynthesisSink{Provider: fixedSynthesisProvider(output), Writer: &b, TrailFindingsJSON: trail}.RunFinished(summary)
			return b.String()
		},
		"findings preview": func(trail bool) string {
			return reviewManifestPreview(manifest(trail))
		},
		"findings detail": func(trail bool) string {
			var b bytes.Buffer
			printReviewManifestDetail(&b, manifest(trail))
			return b.String()
		},
	}
	for name, render := range surfaces {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := render(true); strings.Contains(got, emptyTrailJSON) || !strings.Contains(got, narrative) {
				t.Errorf("trail run must show the prose without the contract line:\n%s", got)
			}
			if got := render(false); !strings.Contains(got, emptyTrailJSON) {
				t.Errorf("local run must show the reviewer's trailing JSON:\n%s", got)
			}
		})
	}
}
