package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleRunner = `{
  "id": "trail-risk",
  "display_name": "Risk Eval",
  "enabled": true,
  "runtime": {
    "kind": "prompt_runner",
    "model": "haiku"
  },
  "prompt": {
    "template": "Old template with a \"quote\" and <angle> & ampersand — and an em-dash."
  },
  "output": {
    "trail_monitor": {
      "key": "risk",
      "polarity": "lower_is_better"
    }
  }
}
`

func TestReplaceRunnerTemplate_SurgicalPreservesOtherFields(t *testing.T) {
	t.Parallel()

	const newTemplate = "Brand new template with <angle>, & ampersand, \"quotes\", and — em-dash."
	out, err := replaceRunnerTemplate([]byte(sampleRunner), newTemplate)
	if err != nil {
		t.Fatalf("replaceRunnerTemplate: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON:\n%s", out)
	}

	// Every non-template field must survive byte-for-byte.
	for _, want := range []string{
		`"id": "trail-risk"`,
		`"display_name": "Risk Eval"`,
		`"model": "haiku"`,
		`"polarity": "lower_is_better"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected output to preserve %q, got:\n%s", want, out)
		}
	}

	// And the template must now be the new one (decoded), with special chars literal.
	var doc struct {
		Prompt struct {
			Template string `json:"template"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if doc.Prompt.Template != newTemplate {
		t.Errorf("template = %q, want %q", doc.Prompt.Template, newTemplate)
	}
	// Literal <angle> present (rather than <angle>) proves
	// SetEscapeHTML(false) kept special chars unescaped.
	if !strings.Contains(string(out), `<angle>`) {
		t.Errorf("expected literal <angle> (not HTML-escaped) in output:\n%s", out)
	}
}

func TestReplaceRunnerTemplate_NoChangeReturnsIdentical(t *testing.T) {
	t.Parallel()

	const same = "Old template with a \"quote\" and <angle> & ampersand — and an em-dash."
	out, err := replaceRunnerTemplate([]byte(sampleRunner), same)
	if err != nil {
		t.Fatalf("replaceRunnerTemplate: %v", err)
	}
	if string(out) != sampleRunner {
		t.Errorf("expected identical bytes when template unchanged")
	}
}

func TestReplaceRunnerTemplate_Errors(t *testing.T) {
	t.Parallel()

	if _, err := replaceRunnerTemplate([]byte(`{"prompt": {}}`), "x"); err == nil {
		t.Error("expected error when prompt.template is missing")
	}
	if _, err := replaceRunnerTemplate([]byte(`{}`), "x"); err == nil {
		t.Error("expected error when prompt object is missing")
	}
	if _, err := replaceRunnerTemplate([]byte(`not json`), "x"); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestValidateNewTemplate(t *testing.T) {
	t.Parallel()

	const old = "Analyze {{branch}} vs {{base_branch}}. Use {{previous_findings}}. Output JSON."

	tests := []struct {
		name        string
		newTemplate string
		wantErr     bool
	}{
		{name: "all placeholders preserved", newTemplate: "New text {{branch}} {{base_branch}} {{previous_findings}} done", wantErr: false},
		{name: "empty", newTemplate: "   ", wantErr: true},
		{name: "dropped placeholder is allowed", newTemplate: "New text {{base_branch}} {{previous_findings}} done", wantErr: false},
		{name: "invented placeholder", newTemplate: "New {{branch}} {{base_branch}} {{previous_findings}} {{secrets}}", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateNewTemplate(old, tc.newTemplate)
			if tc.wantErr != (err != nil) {
				t.Errorf("validateNewTemplate err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestDroppedPlaceholders(t *testing.T) {
	t.Parallel()

	const old = "Analyze {{branch}} vs {{base_branch}}. Use {{previous_findings}}."
	got := droppedPlaceholders(old, "Analyze HEAD vs {{base_branch}}. Use {{previous_findings}}.")
	if len(got) != 1 || got[0] != "{{branch}}" {
		t.Errorf("dropped = %v, want [{{branch}}]", got)
	}
	if d := droppedPlaceholders(old, old); len(d) != 0 {
		t.Errorf("expected no drops for identical template, got %v", d)
	}
}

func TestUntailoredRunners(t *testing.T) {
	t.Parallel()

	created := []string{"trail-risk", "trail-drift", "trail-review"}
	tailored := map[string]bool{"risk": true} // normalized IDs (no "trail-" prefix)

	got := untailoredRunners(created, tailored)
	want := []string{"trail-drift", "trail-review"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("untailoredRunners = %v, want %v", got, want)
	}

	// Nothing created → nothing untailored, even with no tailoring recorded.
	if u := untailoredRunners(nil, map[string]bool{}); len(u) != 0 {
		t.Errorf("expected empty, got %v", u)
	}
}

func TestParseTuneOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "plain object",
			in:   `{"trail-risk": "new risk", "trail-drift": "new drift"}`,
			want: map[string]string{"trail-risk": "new risk", "trail-drift": "new drift"},
		},
		{
			name: "fenced",
			in:   "```json\n{\"trail-risk\": \"new risk\"}\n```",
			want: map[string]string{"trail-risk": "new risk"},
		},
		{
			name: "prose wrapped",
			in:   "Here are the changes:\n{\"trail-risk\": \"new risk\"}\nDone.",
			want: map[string]string{"trail-risk": "new risk"},
		},
		{name: "no json", in: "no object here", wantErr: true},
		{name: "empty object is a valid no-op", in: "{}", want: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTuneOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTuneOutput: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
