package agent

import (
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

func TestFileEditFields(t *testing.T) {
	t.Parallel()
	now := time.Now()
	edit := FileEdit{
		FilePath:     "cmd/main.go",
		Action:       FileEditActionEdit,
		ToolName:     "Edit",
		LinesAdded:   5,
		LinesRemoved: 2,
		Timestamp:    now,
	}
	if edit.FilePath != "cmd/main.go" {
		t.Errorf("FilePath = %q, want %q", edit.FilePath, "cmd/main.go")
	}
	if edit.Action != FileEditActionEdit {
		t.Errorf("Action = %q, want %q", edit.Action, FileEditActionEdit)
	}
	if edit.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want %q", edit.ToolName, "Edit")
	}
	if edit.LinesAdded != 5 {
		t.Errorf("LinesAdded = %d, want %d", edit.LinesAdded, 5)
	}
	if edit.LinesRemoved != 2 {
		t.Errorf("LinesRemoved = %d, want %d", edit.LinesRemoved, 2)
	}
	if !edit.Timestamp.Equal(now) {
		t.Errorf("Timestamp = %v, want %v", edit.Timestamp, now)
	}
}
