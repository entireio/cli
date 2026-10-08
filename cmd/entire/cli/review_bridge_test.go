package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const testWholeChangeGranularity = "whole_change"

func TestReviewTrailFindingInput(t *testing.T) {
	// Regression: a review verdict is not tied to a file/line, so the finding
	// must use whole_change granularity. An empty granularity is rejected by
	// the API with a 400.
	in := reviewTrailFindingInputWithKind("general", "  the verdict  ", "verdict")
	if in.Location.Granularity != testWholeChangeGranularity {
		t.Errorf("granularity = %q, want whole_change", in.Location.Granularity)
	}
	if in.ClientID == "" {
		t.Error("client id (idempotency key) should be set")
	}
	if in.Body == nil || !strings.Contains(*in.Body, "the verdict") || !strings.Contains(*in.Body, "general") {
		t.Errorf("body = %v, want it to include the profile and (trimmed) verdict", in.Body)
	}

	// No profile: the body is exactly the trimmed verdict.
	bare := reviewTrailFindingInputWithKind("", "  bare verdict  ", "verdict")
	if bare.Body == nil || *bare.Body != "bare verdict" {
		t.Errorf("body = %v, want exactly %q", bare.Body, "bare verdict")
	}
	if bare.Location.Granularity != testWholeChangeGranularity {
		t.Errorf("granularity = %q, want whole_change", bare.Location.Granularity)
	}
}

func TestReviewTrailFindingInputsSplitsTopLevelBullets(t *testing.T) {
	verdict := `REQUEST CHANGES - multiple issues.

- **[P1] First issue:** fix sandbox-runs-queue.ts:1144
  with continuation detail
- **[P2] Second issue:** fix daytona.ts:919
  - nested detail stays with second
- **[Low] Third issue:** remove the note from daytona-command-native-plan.md:1`

	inputs := reviewTrailFindingInputs("general", verdict)
	if len(inputs) != 3 {
		t.Fatalf("inputs = %d, want 3", len(inputs))
	}
	bodies := make([]string, len(inputs))
	for i, in := range inputs {
		if in.Location.Granularity != reviewTrailGranularityLine {
			t.Fatalf("input %d granularity = %q, want line", i, in.Location.Granularity)
		}
		if in.Location.FilePath == nil || in.Location.StartLine == nil {
			t.Fatalf("input %d missing inferred line location: %+v", i, in.Location)
		}
		if in.Severity == nil {
			t.Fatalf("input %d missing inferred severity", i)
		}
		if in.ClientID == "" {
			t.Fatalf("input %d missing client id", i)
		}
		if in.Body == nil {
			t.Fatalf("input %d body is nil", i)
		}
		bodies[i] = *in.Body
		if !strings.Contains(bodies[i], "Review finding (profile: general)") {
			t.Fatalf("body %d missing finding/profile header: %q", i, bodies[i])
		}
	}
	if !strings.Contains(bodies[0], "First issue") || !strings.Contains(bodies[0], "with continuation detail") {
		t.Fatalf("first body did not preserve first finding: %q", bodies[0])
	}
	if !strings.Contains(bodies[1], "Second issue") || !strings.Contains(bodies[1], "nested detail stays with second") {
		t.Fatalf("second body did not preserve nested detail: %q", bodies[1])
	}
	if strings.Contains(bodies[0], "Second issue") || strings.Contains(bodies[1], "Third issue") {
		t.Fatalf("bodies were not split cleanly: %#v", bodies)
	}
}

func TestReviewTrailFindingInputsSplitsTopLevelMarkedFindings(t *testing.T) {
	verdict := "request changes\n\n" +
		"**[HIGH] Lifecycle events silently dropped** — `api/src/lib/planetscale/trails.ts:657–668`. Fix it.\n\n" +
		"Additional detail for the first issue.\n\n" +
		"**[MEDIUM] PATCH thread route missing requestBody** — `api/src/routes/trails.ts:2802`. Generated clients cannot send updates.\n"

	inputs := reviewTrailFindingInputs("general", verdict)
	if len(inputs) != 2 {
		t.Fatalf("inputs = %d, want 2", len(inputs))
	}
	bodies := []string{*inputs[0].Body, *inputs[1].Body}
	if !strings.Contains(bodies[0], "Review finding (profile: general)") || !strings.Contains(bodies[0], "Lifecycle events") || !strings.Contains(bodies[0], "Additional detail") {
		t.Fatalf("first body did not preserve first marked finding: %q", bodies[0])
	}
	if strings.Contains(bodies[0], "PATCH thread route") || !strings.Contains(bodies[1], "PATCH thread route") {
		t.Fatalf("marked findings were not split cleanly: %#v", bodies)
	}
	if inputs[0].Severity == nil || *inputs[0].Severity != "high" {
		t.Fatalf("severity[0] = %v, want high", inputs[0].Severity)
	}
	if inputs[1].Severity == nil || *inputs[1].Severity != "medium" {
		t.Fatalf("severity[1] = %v, want medium", inputs[1].Severity)
	}
	if inputs[0].Location.Granularity != reviewTrailGranularityLine || inputs[0].Location.FilePath == nil || *inputs[0].Location.FilePath != "api/src/lib/planetscale/trails.ts" || inputs[0].Location.StartLine == nil || *inputs[0].Location.StartLine != 657 {
		t.Fatalf("location[0] = %+v, want trails.ts:657", inputs[0].Location)
	}
}

func TestReviewTrailFindingInputsAcceptsRunnerStyleJSONLastLine(t *testing.T) {
	verdict := `Intermediate prose that should be ignored for posting.
{"summary":"","comments":[{"severity":"high","confidence":0.92,"body":"` + "`" + `daytona.ts` + "`" + ` rejects public repos without a token; allow public clones or mint a token.","location":{"granularity":"line","file_path":"daytona.ts","start_line":901,"selected_text":"return err"}},{"severity":"P2","confidence":0.7,"body":"Delete failures are swallowed, orphaning provider snapshots.","location":{"granularity":"range","file_path":"daytona.ts","start_line":966,"end_line":970}}]}`

	inputs := reviewTrailFindingInputs("general", verdict)
	if len(inputs) != 2 {
		t.Fatalf("inputs = %d, want 2", len(inputs))
	}
	if inputs[0].Body == nil || strings.Contains(*inputs[0].Body, "Review finding") {
		t.Fatalf("structured JSON body should be the native comment body, got %v", inputs[0].Body)
	}
	if inputs[0].Severity == nil || *inputs[0].Severity != "high" {
		t.Fatalf("severity[0] = %v, want high", inputs[0].Severity)
	}
	if inputs[0].Confidence == nil || *inputs[0].Confidence != 0.92 {
		t.Fatalf("confidence[0] = %v, want 0.92", inputs[0].Confidence)
	}
	if inputs[0].Location.Granularity != reviewTrailGranularityLine || inputs[0].Location.FilePath == nil || *inputs[0].Location.FilePath != "daytona.ts" || inputs[0].Location.StartLine == nil || *inputs[0].Location.StartLine != 901 {
		t.Fatalf("location[0] = %+v, want daytona.ts:901", inputs[0].Location)
	}
	if inputs[0].Location.SelectedText == nil || *inputs[0].Location.SelectedText != "return err" {
		t.Fatalf("selected_text[0] = %v, want preserved JSON selected_text", inputs[0].Location.SelectedText)
	}
	if inputs[1].Severity == nil || *inputs[1].Severity != "medium" {
		t.Fatalf("severity[1] = %v, want normalized medium", inputs[1].Severity)
	}
	if inputs[1].Location.Granularity != reviewTrailGranularityRange || inputs[1].Location.EndLine == nil || *inputs[1].Location.EndLine != 970 {
		t.Fatalf("location[1] = %+v, want range ending 970", inputs[1].Location)
	}
}

func TestReviewTrailFindingInputsSingleVerdictUnchanged(t *testing.T) {
	inputs := reviewTrailFindingInputs("general", "APPROVE - no actionable findings.")
	if len(inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(inputs))
	}
	if inputs[0].Body == nil || !strings.Contains(*inputs[0].Body, "Review verdict (profile: general)") {
		t.Fatalf("single body = %v, want verdict/profile header", inputs[0].Body)
	}
}

func TestReviewTrailLocationFromJSON_SingleLineRangeBecomesLine(t *testing.T) {
	loc := reviewTrailLocationFromJSON(reviewTrailJSONLocation{
		Granularity: reviewTrailGranularityRange,
		FilePath:    "src/app.ts",
		StartLine:   42,
		EndLine:     42,
	})
	if loc.Granularity != reviewTrailGranularityLine {
		t.Fatalf("granularity = %q, want line", loc.Granularity)
	}
	if loc.FilePath == nil || *loc.FilePath != "src/app.ts" || loc.StartLine == nil || *loc.StartLine != 42 || loc.EndLine != nil {
		t.Fatalf("location = %+v, want single-line location at src/app.ts:42", loc)
	}
}

func TestReviewTrailLocationFromJSON_InvalidRangeKeepsStartLineAnchor(t *testing.T) {
	loc := reviewTrailLocationFromJSON(reviewTrailJSONLocation{
		Granularity: reviewTrailGranularityRange,
		FilePath:    "src/app.ts",
		StartLine:   42,
		EndLine:     40,
	})
	if loc.Granularity != reviewTrailGranularityLine {
		t.Fatalf("granularity = %q, want line", loc.Granularity)
	}
	if loc.FilePath == nil || *loc.FilePath != "src/app.ts" || loc.StartLine == nil || *loc.StartLine != 42 || loc.EndLine != nil {
		t.Fatalf("location = %+v, want line location preserving start line", loc)
	}
}

func TestSplitReviewVerdictFindingsNumberedList(t *testing.T) {
	items := splitReviewVerdictFindings("Verdict\n\n1. First\n2. Second")
	if len(items) != 2 || items[0] != "First" || items[1] != "Second" {
		t.Fatalf("items = %#v, want numbered findings", items)
	}
}

func TestTrailReviewWebURL(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "https://entire.io")

	cases := []struct {
		name   string
		target trailReviewTarget
		want   string
	}{
		{
			name: "full target",
			target: trailReviewTarget{
				Host:  "gh",
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Number: 466, Branch: "review-profiles"},
			},
			want: "https://entire.io/gh/entireio/cli/trails/466/review-profiles",
		},
		{
			name: "no trail number yields no link",
			target: trailReviewTarget{
				Host:  "gh",
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Branch: "review-profiles"},
			},
			want: "",
		},
		{
			name: "missing forge yields no link",
			target: trailReviewTarget{
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Number: 1, Branch: "main"},
			},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trailReviewWebURL(c.target); got != c.want {
				t.Errorf("trailReviewWebURL() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTrailReviewWebURL_HonorsCustomBase(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "https://entire.example.com/")
	target := trailReviewTarget{
		Host:  "gh",
		Owner: "acme",
		Repo:  "app",
		Trail: api.TrailResource{Number: 7, Branch: "feat/x"},
	}
	want := "https://entire.example.com/gh/acme/app/trails/7/feat/x"
	if got := trailReviewWebURL(target); got != want {
		t.Errorf("trailReviewWebURL() = %q, want %q", got, want)
	}
}
