package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCountLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty string", "", 0},
		{"single line no newline", "hello", 1},
		{"single line with newline", "hello\n", 1},
		{"two lines", "hello\nworld", 2},
		{"two lines trailing newline", "hello\nworld\n", 2},
		{"multiple lines", "a\nb\nc\nd", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CountLines(tt.input)
			if got != tt.want {
				t.Errorf("CountLines(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestFileEdit_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Millisecond) // JSON loses sub-ms precision
	original := FileEdit{
		FilePath:     "cmd/main.go",
		Action:       FileEditActionEdit,
		ToolName:     "Edit",
		LinesAdded:   5,
		LinesRemoved: 2,
		Timestamp:    now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded FileEdit
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded.FilePath != original.FilePath {
		t.Errorf("FilePath = %q, want %q", decoded.FilePath, original.FilePath)
	}
	if decoded.Action != original.Action {
		t.Errorf("Action = %q, want %q", decoded.Action, original.Action)
	}
	if decoded.ToolName != original.ToolName {
		t.Errorf("ToolName = %q, want %q", decoded.ToolName, original.ToolName)
	}
	if decoded.LinesAdded != original.LinesAdded {
		t.Errorf("LinesAdded = %d, want %d", decoded.LinesAdded, original.LinesAdded)
	}
	if decoded.LinesRemoved != original.LinesRemoved {
		t.Errorf("LinesRemoved = %d, want %d", decoded.LinesRemoved, original.LinesRemoved)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, original.Timestamp)
	}
}
