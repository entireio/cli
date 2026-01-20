package cli

import (
	"context"
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

// Tests for AI-powered summarization functions

func TestFormatTranscriptForAI(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Add a logout button"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "I'll add the button now."}]}`)},
		{Type: "user", Message: json.RawMessage(`{"content": "Also add a login button"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Done with both buttons."}]}`)},
	}

	result := formatTranscriptForAI(transcript)

	// Should contain user messages prefixed with "User: "
	if !strings.Contains(result, "User: Add a logout button") {
		t.Errorf("Expected 'User: Add a logout button' in output, got: %s", result)
	}

	// Should contain assistant messages prefixed with "Assistant: "
	if !strings.Contains(result, "Assistant: I'll add the button now.") {
		t.Errorf("Expected 'Assistant: I'll add the button now.' in output, got: %s", result)
	}

	// Should preserve order
	userIdx := strings.Index(result, "User: Add a logout button")
	assistantIdx := strings.Index(result, "Assistant: I'll add the button now.")
	if userIdx > assistantIdx {
		t.Error("User message should come before assistant message")
	}
}

func TestFormatTranscriptForAI_EmptyTranscript(t *testing.T) {
	result := formatTranscriptForAI(nil)
	if result != "" {
		t.Errorf("Expected empty string for nil transcript, got: %s", result)
	}

	result = formatTranscriptForAI([]transcriptLine{})
	if result != "" {
		t.Errorf("Expected empty string for empty transcript, got: %s", result)
	}
}

func TestTruncateTranscriptForAI(t *testing.T) {
	// Create a transcript with 10 messages
	var transcript []transcriptLine
	for i := range 10 {
		transcript = append(transcript, transcriptLine{
			Type:    "user",
			Message: json.RawMessage(`{"content": "Message ` + string(rune('0'+i)) + `"}`),
		})
		transcript = append(transcript, transcriptLine{
			Type:    "assistant",
			Message: json.RawMessage(`{"content": [{"type": "text", "text": "Response ` + string(rune('0'+i)) + `"}]}`),
		})
	}

	// Should keep only last 4 messages
	result := truncateTranscriptForAI(transcript, 4)

	// Should contain last messages but not first
	if strings.Contains(result, "Message 0") {
		t.Error("Should not contain first message after truncation")
	}

	// Should contain recent messages
	if !strings.Contains(result, "Message 8") && !strings.Contains(result, "Message 9") {
		t.Error("Should contain recent messages")
	}
}

func TestTruncateTranscriptForAI_ShortTranscript(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", Message: json.RawMessage(`{"content": "Hello"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content": [{"type": "text", "text": "Hi there!"}]}`)},
	}

	// Requesting more messages than exist should return all
	result := truncateTranscriptForAI(transcript, 10)

	if !strings.Contains(result, "User: Hello") {
		t.Errorf("Should contain all messages when transcript is shorter than limit, got: %s", result)
	}
}

func TestParseSummaryResponse(t *testing.T) {
	tests := []struct {
		name          string
		response      string
		wantIntent    string
		wantOutcome   string
		wantLearnings []string
		wantFriction  []string
		wantErr       bool
	}{
		{
			name:          "valid response with all fields",
			response:      `{"intent": "Add a logout button", "outcome": "Button added successfully", "learnings": ["Use React hooks"], "friction_points": ["API was slow"]}`,
			wantIntent:    "Add a logout button",
			wantOutcome:   "Button added successfully",
			wantLearnings: []string{"Use React hooks"},
			wantFriction:  []string{"API was slow"},
		},
		{
			name:          "valid response with empty arrays",
			response:      `{"intent": "Fix bug", "outcome": "Bug fixed", "learnings": [], "friction_points": []}`,
			wantIntent:    "Fix bug",
			wantOutcome:   "Bug fixed",
			wantLearnings: []string{},
			wantFriction:  []string{},
		},
		{
			name:          "response with null arrays",
			response:      `{"intent": "Test", "outcome": "Done", "learnings": null, "friction_points": null}`,
			wantIntent:    "Test",
			wantOutcome:   "Done",
			wantLearnings: nil,
			wantFriction:  nil,
		},
		{
			name:     "invalid JSON",
			response: `not json`,
			wantErr:  true,
		},
		{
			name:     "empty string",
			response: ``,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, err := parseSummaryResponse(tt.response)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSummaryResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if summary.Intent != tt.wantIntent {
				t.Errorf("Intent = %q, want %q", summary.Intent, tt.wantIntent)
			}
			if summary.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q, want %q", summary.Outcome, tt.wantOutcome)
			}
			if len(summary.Learnings) != len(tt.wantLearnings) {
				t.Errorf("Learnings = %v, want %v", summary.Learnings, tt.wantLearnings)
			}
			if len(summary.FrictionPoints) != len(tt.wantFriction) {
				t.Errorf("FrictionPoints = %v, want %v", summary.FrictionPoints, tt.wantFriction)
			}
		})
	}
}

func TestParseSummaryResponse_ExtractsJSONFromText(t *testing.T) {
	// Claude sometimes wraps JSON in text
	response := `Here's the summary:
{"intent": "Add feature", "outcome": "Feature added", "learnings": [], "friction_points": []}
Hope that helps!`

	summary, err := parseSummaryResponse(response)
	if err != nil {
		t.Fatalf("parseSummaryResponse() error = %v", err)
	}

	if summary.Intent != "Add feature" {
		t.Errorf("Intent = %q, want %q", summary.Intent, "Add feature")
	}
}

func TestBuildCheckpointSummaryPrompt(t *testing.T) {
	transcriptText := "User: Add a button\n\nAssistant: Done!"

	prompt := buildCheckpointSummaryPrompt(transcriptText)

	// Should contain the transcript
	if !strings.Contains(prompt, transcriptText) {
		t.Error("Prompt should contain the transcript text")
	}

	// Should ask for JSON output
	if !strings.Contains(prompt, "JSON") {
		t.Error("Prompt should mention JSON format")
	}

	// Should ask for intent, outcome, learnings, friction_points
	for _, field := range []string{"intent", "outcome", "learnings", "friction_points"} {
		if !strings.Contains(strings.ToLower(prompt), field) {
			t.Errorf("Prompt should mention %s", field)
		}
	}
}

func TestBuildBranchSummaryPrompt(t *testing.T) {
	checkpointSummaries := `- 2026-01-15: Added login feature
- 2026-01-16: Fixed authentication bug
- 2026-01-17: Added logout button`

	prompt := buildBranchSummaryPrompt(checkpointSummaries)

	// Should contain the checkpoint summaries
	if !strings.Contains(prompt, checkpointSummaries) {
		t.Error("Prompt should contain the checkpoint summaries")
	}

	// Should ask for JSON output
	if !strings.Contains(prompt, "JSON") {
		t.Error("Prompt should mention JSON format")
	}

	// Should ask for overall intent and outcome
	if !strings.Contains(strings.ToLower(prompt), "intent") || !strings.Contains(strings.ToLower(prompt), "outcome") {
		t.Error("Prompt should ask for intent and outcome")
	}
}

func TestIsSummaryEmpty(t *testing.T) {
	tests := []struct {
		name    string
		summary *Summary
		want    bool
	}{
		{
			name:    "nil summary",
			summary: nil,
			want:    true,
		},
		{
			name:    "all fields empty",
			summary: &Summary{},
			want:    true,
		},
		{
			name:    "only intent set",
			summary: &Summary{Intent: "test"},
			want:    false,
		},
		{
			name:    "only outcome set",
			summary: &Summary{Outcome: "test"},
			want:    false,
		},
		{
			name:    "only learnings set",
			summary: &Summary{Learnings: []string{"test"}},
			want:    false,
		},
		{
			name:    "only friction points set",
			summary: &Summary{FrictionPoints: []string{"test"}},
			want:    false,
		},
		{
			name:    "empty slices",
			summary: &Summary{Learnings: []string{}, FrictionPoints: []string{}},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSummaryEmpty(tt.summary); got != tt.want {
				t.Errorf("isSummaryEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGenerateBranchSummary_EmptyIntents tests that empty intents returns nil
func TestGenerateBranchSummary_EmptyIntents(t *testing.T) {
	tests := []struct {
		name    string
		intents []string
	}{
		{"nil intents", nil},
		{"empty slice", []string{}},
		{"all empty strings", []string{"", "", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, err := GenerateBranchSummary(context.Background(), tt.intents)
			if err != nil {
				t.Fatalf("GenerateBranchSummary() unexpected error = %v", err)
			}
			if summary != nil {
				t.Errorf("GenerateBranchSummary() = %v, want nil", summary)
			}
		})
	}
}
