package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateSummary_ExtractsIntent(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Add a logout button to the navbar"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "I'll add a logout button to the navbar."}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Intent != "Add a logout button to the navbar" {
		t.Errorf("Intent = %q, want %q", summary.Intent, "Add a logout button to the navbar")
	}
}

func TestGenerateSummary_ExtractsOutcome(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Fix the bug"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Done! I fixed the authentication bug by updating the token validation logic."}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Outcome == "" {
		t.Error("Outcome should not be empty")
	}
	if summary.Outcome != "Done! I fixed the authentication bug by updating the token validation logic." {
		t.Errorf("Outcome = %q, want %q", summary.Outcome, "Done! I fixed the authentication bug by updating the token validation logic.")
	}
}

func TestGenerateSummary_TruncatesLongIntent(t *testing.T) {
	longPrompt := strings.Repeat("a", 300)

	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "` + longPrompt + `"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "OK"}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if len(summary.Intent) > 200 {
		t.Errorf("Intent should be truncated to 200 chars, got %d", len(summary.Intent))
	}
	if !strings.HasSuffix(summary.Intent, "...") {
		t.Error("Truncated intent should end with ...")
	}
}

func TestGenerateSummary_TruncatesLongOutcome(t *testing.T) {
	longOutcome := strings.Repeat("b", 300)

	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Do something"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "` + longOutcome + `"}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if len(summary.Outcome) > 200 {
		t.Errorf("Outcome should be truncated to 200 chars, got %d", len(summary.Outcome))
	}
	if !strings.HasSuffix(summary.Outcome, "...") {
		t.Error("Truncated outcome should end with ...")
	}
}

func TestGenerateSummary_EmptyTranscript(t *testing.T) {
	summary, err := GenerateSummary(nil)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Intent != "" {
		t.Errorf("Intent should be empty for nil transcript, got %q", summary.Intent)
	}
	if summary.Outcome != "" {
		t.Errorf("Outcome should be empty for nil transcript, got %q", summary.Outcome)
	}
}

func TestGenerateSummary_LearningsAndFrictionEmpty(t *testing.T) {
	// For now, these should always be empty (requires AI)
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Do something"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Done"}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if len(summary.Learnings) != 0 {
		t.Errorf("Learnings should be empty, got %v", summary.Learnings)
	}
	if len(summary.FrictionPoints) != 0 {
		t.Errorf("FrictionPoints should be empty, got %v", summary.FrictionPoints)
	}
}

func TestGenerateSummary_MultipleUserPrompts_UsesFirst(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "First prompt - this is the intent"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Response 1"}]}`)},
		{Type: "user", Message: json.RawMessage(`{"content": "Second prompt - not the intent"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Final response - this is the outcome"}]}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Intent != "First prompt - this is the intent" {
		t.Errorf("Intent = %q, want %q", summary.Intent, "First prompt - this is the intent")
	}
	if summary.Outcome != "Final response - this is the outcome" {
		t.Errorf("Outcome = %q, want %q", summary.Outcome, "Final response - this is the outcome")
	}
}

func TestGenerateSummary_NoAssistantMessages(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Some prompt"}`)},
	}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Intent != "Some prompt" {
		t.Errorf("Intent = %q, want %q", summary.Intent, "Some prompt")
	}
	if summary.Outcome != "" {
		t.Errorf("Outcome should be empty when no assistant messages, got %q", summary.Outcome)
	}
}

func TestGenerateSummary_EmptySlice(t *testing.T) {
	transcript := []transcriptLine{}

	summary, err := GenerateSummary(transcript)
	if err != nil {
		t.Fatalf("GenerateSummary() error = %v", err)
	}

	if summary.Intent != "" {
		t.Errorf("Intent should be empty for empty transcript, got %q", summary.Intent)
	}
	if summary.Outcome != "" {
		t.Errorf("Outcome should be empty for empty transcript, got %q", summary.Outcome)
	}
}

// Note: truncation is now handled by stringutil.TruncateRunes which has its own tests.
// The TestGenerateSummary_TruncatesLongIntent and TestGenerateSummary_TruncatesLongOutcome
// tests above verify that truncation is correctly integrated into GenerateSummary.
