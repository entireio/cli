package cli

import "testing"

// TestFormatModel mirrors entire.io's frontend model.test.ts so the CLI's
// friendly model labels stay identical to the web Overview page.
func TestFormatModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		// Current Claude identifiers.
		{"claude-opus-4-6", "Opus 4.6"},
		{"claude-sonnet-4-6", "Sonnet 4.6"},
		{"claude-haiku-4-5", "Haiku 4.5"},
		// Legacy date suffixes are stripped.
		{"claude-sonnet-4-20250514", "Sonnet 4"},
		{"claude-opus-4-1-20250805", "Opus 4.1"},
		// Case-insensitive family, normalized to Title case.
		{"CLAUDE-OPUS-4-6", "Opus 4.6"},
		// GPT models.
		{"gpt-4o", "GPT-4o"},
		{"gpt-4-turbo", "GPT-4-turbo"},
		{"GPT-4o", "GPT-4o"},
		// Gemini models: upper-first each dash-part.
		{"gemini-2.0-flash", "Gemini 2.0 Flash"},
		// Empty / whitespace.
		{"", ""},
		{"   ", ""},
		// Unknown formats pass through unchanged.
		{"custom-model-123", "custom-model-123"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			if got := formatModel(tt.input); got != tt.want {
				t.Errorf("formatModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
