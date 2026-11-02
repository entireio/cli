package redact

import (
	"strings"
	"testing"
)

// claudeToolUseID has the exact shape of a Claude Code tool-use id: "toolu_01"
// followed by 22 base62 characters (30 bytes in total). Its Shannon entropy is
// above entropyThreshold, so the entropy layer flags it on its own.
const claudeToolUseID = "toolu_01Xk9mQ2vL8nR5tW1yB4cF7d"

func TestClaudeToolUseIDFixtureIsHighEntropy(t *testing.T) {
	t.Parallel()
	// Guards the fixture: if it ever drops below the threshold, the tests below
	// would pass without exercising the exemption.
	if e := shannonEntropy(claudeToolUseID); e <= entropyThreshold {
		t.Fatalf("fixture entropy %.3f is not above threshold %.1f", e, entropyThreshold)
	}
}

// Claude Code task notifications carry the tool-use id in free text, not in an
// "*id" JSON key, so shouldSkipJSONLField does not protect it. Redacting it breaks
// linking a notification to its task record.
func TestString_PreservesClaudeToolUseIDInFreeText(t *testing.T) {
	t.Parallel()
	input := "<task-notification><tool-use-id>" + claudeToolUseID + "</tool-use-id><status>completed</status></task-notification>"

	if got := String(input); got != input {
		t.Fatalf("tool-use id was altered:\n got  %q\n want %q", got, input)
	}
}

func TestJSONLContent_PreservesClaudeToolUseIDInFreeText(t *testing.T) {
	t.Parallel()
	input := `{"type":"user","message":{"role":"user","content":"<task-notification>\n<tool-use-id>` + claudeToolUseID + `</tool-use-id>\n</task-notification>"}}`

	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, claudeToolUseID) {
		t.Fatalf("tool-use id was redacted: %s", got)
	}
}

// The exemption must apply only when the WHOLE entropy match is a tool-use id.
// A secret glued onto an id forms one longer match (the entropy character class
// includes '_' and '-'), and that match must still be redacted in full.
func TestString_ClaudeToolUseIDExemptionIsExact(t *testing.T) {
	t.Parallel()
	const secretTail = "Zp3Hq8Ws2Kd6Nj0Vb7Gm"

	tests := []struct {
		name  string
		input string
	}{
		{"secret appended", claudeToolUseID + secretTail},
		{"secret appended after dash", claudeToolUseID + "-" + secretTail},
		{"secret appended after underscore", claudeToolUseID + "_" + secretTail},
		{"secret prepended", secretTail + claudeToolUseID},
		{"id with one extra char", claudeToolUseID + "Q"},
		{"wrong version prefix", strings.Replace(claudeToolUseID, "toolu_01", "toolu_02", 1)},
		{"non-base62 body", strings.Replace(claudeToolUseID, "Q2v", "Q-v", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String("value: " + tt.input + " end")
			if strings.Contains(got, tt.input) {
				t.Fatalf("expected %q to be redacted, got %q", tt.input, got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("expected a REDACTED marker, got %q", got)
			}
		})
	}
}
