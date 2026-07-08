package cli

import (
	"strings"
	"testing"
)

func TestParseSubagentCheckpointHookInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *SubagentCheckpointHookInput
		wantErr bool
	}{
		{
			name: "valid TodoWrite input",
			input: `{
				"session_id": "abc123",
				"tool_name": "TodoWrite",
				"tool_use_id": "toolu_xyz",
				"tool_input": {"todos": [{"content": "Task 1", "status": "pending"}]},
				"tool_response": {"success": true}
			}`,
			want: &SubagentCheckpointHookInput{
				SessionID: "abc123",
				ToolName:  "TodoWrite",
				ToolUseID: "toolu_xyz",
			},
			wantErr: false,
		},
		{
			name:    "empty input",
			input:   "",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   "not json",
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid Edit input",
			input: `{
				"session_id": "def456",
				"tool_name": "Edit",
				"tool_use_id": "toolu_edit123",
				"tool_input": {"file_path": "/path/to/file", "old_string": "foo", "new_string": "bar"},
				"tool_response": {}
			}`,
			want: &SubagentCheckpointHookInput{
				SessionID: "def456",
				ToolName:  "Edit",
				ToolUseID: "toolu_edit123",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.input)
			got, err := parseSubagentCheckpointHookInput(reader)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseSubagentCheckpointHookInput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want != nil {
				if got.SessionID != tt.want.SessionID {
					t.Errorf("SessionID = %v, want %v", got.SessionID, tt.want.SessionID)
				}
				if got.ToolName != tt.want.ToolName {
					t.Errorf("ToolName = %v, want %v", got.ToolName, tt.want.ToolName)
				}
				if got.ToolUseID != tt.want.ToolUseID {
					t.Errorf("ToolUseID = %v, want %v", got.ToolUseID, tt.want.ToolUseID)
				}
				// ToolInput and ToolResponse are json.RawMessage, just verify they're not nil
				if got.ToolInput == nil {
					t.Error("ToolInput should not be nil")
				}
			}
		})
	}
}

func TestParseSubagentTypeAndDescription(t *testing.T) {
	tests := []struct {
		name            string
		toolInput       string
		wantAgentType   string
		wantDescription string
	}{
		{
			name:            "full task input",
			toolInput:       `{"subagent_type": "dev", "description": "Implement user authentication", "prompt": "Do the work"}`,
			wantAgentType:   "dev",
			wantDescription: "Implement user authentication",
		},
		{
			name:            "only subagent_type",
			toolInput:       `{"subagent_type": "reviewer", "prompt": "Review changes"}`,
			wantAgentType:   "reviewer",
			wantDescription: "",
		},
		{
			name:            "only description",
			toolInput:       `{"description": "Fix the bug", "prompt": "Fix it"}`,
			wantAgentType:   "",
			wantDescription: "Fix the bug",
		},
		{
			name:            "neither field",
			toolInput:       `{"prompt": "Do something"}`,
			wantAgentType:   "",
			wantDescription: "",
		},
		{
			name:            "empty input",
			toolInput:       ``,
			wantAgentType:   "",
			wantDescription: "",
		},
		{
			name:            "invalid json",
			toolInput:       `not valid json`,
			wantAgentType:   "",
			wantDescription: "",
		},
		{
			name:            "null input",
			toolInput:       `null`,
			wantAgentType:   "",
			wantDescription: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAgentType, gotDescription := ParseSubagentTypeAndDescription([]byte(tt.toolInput))

			if gotAgentType != tt.wantAgentType {
				t.Errorf("agentType = %q, want %q", gotAgentType, tt.wantAgentType)
			}
			if gotDescription != tt.wantDescription {
				t.Errorf("description = %q, want %q", gotDescription, tt.wantDescription)
			}
		})
	}
}

func TestExtractTodoContentFromToolInput(t *testing.T) {
	tests := []struct {
		name      string
		toolInput string
		want      string
	}{
		{
			name:      "in_progress item present",
			toolInput: `{"todos": [{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "in_progress"}, {"content": "Third task", "status": "pending"}]}`,
			want:      "Second task",
		},
		{
			name:      "no in_progress - fallback to first pending",
			toolInput: `{"todos": [{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "pending"}, {"content": "Third task", "status": "pending"}]}`,
			want:      "Second task",
		},
		{
			name:      "all pending - first TodoWrite scenario",
			toolInput: `{"todos": [{"content": "First pending task", "status": "pending", "activeForm": "Doing first task"}, {"content": "Second pending task", "status": "pending", "activeForm": "Doing second task"}]}`,
			want:      "First pending task",
		},
		{
			name:      "no in_progress or pending - returns last completed",
			toolInput: `{"todos": [{"content": "First task", "status": "completed"}]}`,
			want:      "First task",
		},
		{
			name:      "empty todos array",
			toolInput: `{"todos": []}`,
			want:      "",
		},
		{
			name:      "no todos field",
			toolInput: `{"other_field": "value"}`,
			want:      "",
		},
		{
			name:      "null todos field",
			toolInput: `{"todos": null}`,
			want:      "",
		},
		{
			name:      "empty input",
			toolInput: ``,
			want:      "",
		},
		{
			name:      "invalid json",
			toolInput: `not valid json`,
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTodoContentFromToolInput([]byte(tt.toolInput))
			if got != tt.want {
				t.Errorf("ExtractTodoContentFromToolInput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractLastCompletedTodoFromToolInput(t *testing.T) {
	tests := []struct {
		name      string
		toolInput string
		want      string
	}{
		{
			name:      "last completed item present",
			toolInput: `{"todos": [{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "completed"}, {"content": "Third task", "status": "in_progress"}]}`,
			want:      "Second task",
		},
		{
			name:      "no completed items",
			toolInput: `{"todos": [{"content": "First task", "status": "in_progress"}, {"content": "Second task", "status": "pending"}]}`,
			want:      "",
		},
		{
			name:      "empty todos array",
			toolInput: `{"todos": []}`,
			want:      "",
		},
		{
			name:      "empty input",
			toolInput: ``,
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractLastCompletedTodoFromToolInput([]byte(tt.toolInput))
			if got != tt.want {
				t.Errorf("ExtractLastCompletedTodoFromToolInput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountTodosFromToolInput(t *testing.T) {
	tests := []struct {
		name      string
		toolInput string
		want      int
	}{
		{
			name:      "typical list with multiple items",
			toolInput: `{"todos": [{"content": "First task", "status": "completed"}, {"content": "Second task", "status": "in_progress"}, {"content": "Third task", "status": "pending"}]}`,
			want:      3,
		},
		{
			name:      "six items - planning scenario",
			toolInput: `{"todos": [{"content": "Task 1", "status": "pending"}, {"content": "Task 2", "status": "pending"}, {"content": "Task 3", "status": "pending"}, {"content": "Task 4", "status": "pending"}, {"content": "Task 5", "status": "pending"}, {"content": "Task 6", "status": "in_progress"}]}`,
			want:      6,
		},
		{
			name:      "empty todos array",
			toolInput: `{"todos": []}`,
			want:      0,
		},
		{
			name:      "no todos field",
			toolInput: `{"other_field": "value"}`,
			want:      0,
		},
		{
			name:      "empty input",
			toolInput: ``,
			want:      0,
		},
		{
			name:      "invalid json",
			toolInput: `not valid json`,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountTodosFromToolInput([]byte(tt.toolInput))
			if got != tt.want {
				t.Errorf("CountTodosFromToolInput() = %d, want %d", got, tt.want)
			}
		})
	}
}
