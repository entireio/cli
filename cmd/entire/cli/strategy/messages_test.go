package strategy

import "testing"

func TestTruncateDescription(t *testing.T) {
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
			got := TruncateDescription(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateDescription(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestFormatSubagentEndMessage(t *testing.T) {
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
			got := FormatSubagentEndMessage(tt.agentType, tt.description, tt.toolUseID)
			if got != tt.want {
				t.Errorf("FormatSubagentEndMessage(%q, %q, %q) = %q, want %q",
					tt.agentType, tt.description, tt.toolUseID, got, tt.want)
			}
		})
	}
}

func TestFormatIncrementalMessage(t *testing.T) {
	tests := []struct {
		name        string
		todoContent string
		sequence    int
		toolUseID   string
		want        string
	}{
		{
			name:        "with todo content",
			todoContent: "Set up Node.js project with package.json",
			sequence:    1,
			toolUseID:   "toolu_01CJhrr",
			want:        "Set up Node.js project with package.json (toolu_01CJhrr)",
		},
		{
			name:        "empty todo content falls back to checkpoint format",
			todoContent: "",
			sequence:    3,
			toolUseID:   "toolu_01CJhrr",
			want:        "Checkpoint #3: toolu_01CJhrr",
		},
		{
			name:        "long todo content truncated",
			todoContent: "This is a very long todo item that describes in detail what needs to be done for this step of the implementation process",
			sequence:    2,
			toolUseID:   "toolu_01CJhrr",
			want:        "This is a very long todo item that describes in detail wh... (toolu_01CJhrr)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatIncrementalMessage(tt.todoContent, tt.sequence, tt.toolUseID)
			if got != tt.want {
				t.Errorf("FormatIncrementalMessage(%q, %d, %q) = %q, want %q",
					tt.todoContent, tt.sequence, tt.toolUseID, got, tt.want)
			}
		})
	}
}

func TestExtractLastCompletedTodo(t *testing.T) {
	tests := []struct {
		name      string
		todosJSON string
		want      string
	}{
		{
			name:      "typical case - last completed is the work just finished",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "completed"}, {"content": "Third task", "status": "in_progress"}]`,
			want:      "Second task",
		},
		{
			name:      "single completed item",
			todosJSON: `[{"content": "First task", "status": "completed"}]`,
			want:      "First task",
		},
		{
			name:      "multiple completed - returns last one",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "completed"}, {"content": "Third task", "status": "completed"}]`,
			want:      "Third task",
		},
		{
			name:      "no completed items - empty string",
			todosJSON: `[{"content": "First task", "status": "in_progress"}, {"content": "Second task", "status": "pending"}]`,
			want:      "",
		},
		{
			name:      "empty array",
			todosJSON: `[]`,
			want:      "",
		},
		{
			name:      "invalid JSON",
			todosJSON: `not valid json`,
			want:      "",
		},
		{
			name:      "null",
			todosJSON: `null`,
			want:      "",
		},
		{
			name:      "completed items mixed with pending",
			todosJSON: `[{"content": "Done 1", "status": "completed"}, {"content": "Pending 1", "status": "pending"}, {"content": "Done 2", "status": "completed"}, {"content": "Pending 2", "status": "pending"}]`,
			want:      "Done 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractLastCompletedTodo([]byte(tt.todosJSON))
			if got != tt.want {
				t.Errorf("ExtractLastCompletedTodo(%s) = %q, want %q", tt.todosJSON, got, tt.want)
			}
		})
	}
}

func TestCountTodos(t *testing.T) {
	tests := []struct {
		name      string
		todosJSON string
		want      int
	}{
		{
			name:      "typical list with multiple items",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "in_progress"}, {"content": "Third task", "status": "pending"}]`,
			want:      3,
		},
		{
			name:      "single item",
			todosJSON: `[{"content": "Only task", "status": "pending"}]`,
			want:      1,
		},
		{
			name:      "empty array",
			todosJSON: `[]`,
			want:      0,
		},
		{
			name:      "invalid JSON",
			todosJSON: `not valid json`,
			want:      0,
		},
		{
			name:      "null",
			todosJSON: `null`,
			want:      0,
		},
		{
			name:      "six items - planning scenario",
			todosJSON: `[{"content": "Task 1", "status": "pending"}, {"content": "Task 2", "status": "pending"}, {"content": "Task 3", "status": "pending"}, {"content": "Task 4", "status": "pending"}, {"content": "Task 5", "status": "pending"}, {"content": "Task 6", "status": "in_progress"}]`,
			want:      6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountTodos([]byte(tt.todosJSON))
			if got != tt.want {
				t.Errorf("CountTodos(%s) = %d, want %d", tt.todosJSON, got, tt.want)
			}
		})
	}
}

func TestFormatIncrementalSubject(t *testing.T) {
	tests := []struct {
		name                string
		incrementalType     string
		subagentType        string
		taskDescription     string
		todoContent         string
		incrementalSequence int
		shortToolUseID      string
		want                string
	}{
		{
			name:                "incremental with todo content",
			incrementalType:     "TodoWrite",
			todoContent:         "Set up Node.js project",
			incrementalSequence: 1,
			shortToolUseID:      "toolu_01CJhrr",
			want:                "Set up Node.js project (toolu_01CJhrr)",
		},
		{
			name:                "incremental without todo content",
			incrementalType:     "TodoWrite",
			todoContent:         "",
			incrementalSequence: 3,
			shortToolUseID:      "toolu_01CJhrr",
			want:                "Checkpoint #3: toolu_01CJhrr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatIncrementalSubject(
				tt.incrementalType,
				tt.subagentType,
				tt.taskDescription,
				tt.todoContent,
				tt.incrementalSequence,
				tt.shortToolUseID,
			)
			if got != tt.want {
				t.Errorf("FormatIncrementalSubject() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractInProgressTodo(t *testing.T) {
	tests := []struct {
		name      string
		todosJSON string
		want      string
	}{
		{
			name:      "single in_progress item",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "in_progress"}, {"content": "Third task", "status": "pending"}]`,
			want:      "Second task",
		},
		{
			name:      "no in_progress - fallback to first pending",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "pending"}, {"content": "Third task", "status": "pending"}]`,
			want:      "Second task",
		},
		{
			name:      "no in_progress or pending - single completed returns last completed",
			todosJSON: `[{"content": "First task", "status": "completed"}]`,
			want:      "First task",
		},
		{
			name:      "all completed - returns last completed item",
			todosJSON: `[{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "completed"}, {"content": "Third task", "status": "completed"}]`,
			want:      "Third task",
		},
		{
			name:      "empty array",
			todosJSON: `[]`,
			want:      "",
		},
		{
			name:      "invalid JSON",
			todosJSON: `not valid json`,
			want:      "",
		},
		{
			name:      "null",
			todosJSON: `null`,
			want:      "",
		},
		{
			name:      "activeForm field present - use content",
			todosJSON: `[{"content": "Run tests", "activeForm": "Running tests", "status": "in_progress"}]`,
			want:      "Run tests",
		},
		{
			name:      "unknown status - fallback to first item content",
			todosJSON: `[{"content": "First task", "status": "unknown"}, {"content": "Second task", "status": "other"}]`,
			want:      "First task",
		},
		{
			name:      "empty status - fallback to first item content",
			todosJSON: `[{"content": "First task", "status": ""}, {"content": "Second task", "status": ""}]`,
			want:      "First task",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractInProgressTodo([]byte(tt.todosJSON))
			if got != tt.want {
				t.Errorf("ExtractInProgressTodo(%s) = %q, want %q", tt.todosJSON, got, tt.want)
			}
		})
	}
}
