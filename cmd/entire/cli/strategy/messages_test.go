package strategy

import "testing"

func TestTruncateDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "short string unchanged",
			input:  "Short",
			maxLen: 60,
			want:   "Short",
		},
		{
			name:   "exactly max length unchanged",
			input:  "123456",
			maxLen: 6,
			want:   "123456",
		},
		{
			name:   "long string truncated with ellipsis",
			input:  "This is a very long description that exceeds the maximum length",
			maxLen: 30,
			want:   "This is a very long descrip...",
		},
		{
			name:   "empty string",
			input:  "",
			maxLen: 60,
			want:   "",
		},
		{
			name:   "max length less than ellipsis",
			input:  "Hello",
			maxLen: 2,
			want:   "He",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateDescription(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateDescription(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestFormatSubagentEndMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		agentType   string
		description string
		toolUseID   string
		want        string
	}{
		{
			name:        "full message with all fields",
			agentType:   "dev",
			description: "Implement user authentication",
			toolUseID:   "toolu_019t1c",
			want:        "Completed 'dev' agent: Implement user authentication (toolu_019t1c)",
		},
		{
			name:        "empty description",
			agentType:   "dev",
			description: "",
			toolUseID:   "toolu_019t1c",
			want:        "Completed 'dev' agent (toolu_019t1c)",
		},
		{
			name:        "empty agent type",
			agentType:   "",
			description: "Implement user authentication",
			toolUseID:   "toolu_019t1c",
			want:        "Completed agent: Implement user authentication (toolu_019t1c)",
		},
		{
			name:        "both empty",
			agentType:   "",
			description: "",
			toolUseID:   "toolu_019t1c",
			want:        "Task: toolu_019t1c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FormatSubagentEndMessage(tt.agentType, tt.description, tt.toolUseID)
			if got != tt.want {
				t.Errorf("FormatSubagentEndMessage(%q, %q, %q) = %q, want %q",
					tt.agentType, tt.description, tt.toolUseID, got, tt.want)
			}
		})
	}
}
