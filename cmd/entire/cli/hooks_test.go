package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePreTaskHookInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *TaskHookInput
		wantErr bool
	}{
		{
			name:  "valid input",
			input: `{"session_id":"abc123","transcript_path":"/path/to/transcript.jsonl","tool_use_id":"tool_xyz"}`,
			want: &TaskHookInput{
				SessionID:      "abc123",
				TranscriptPath: "/path/to/transcript.jsonl",
				ToolUseID:      "tool_xyz",
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
			name:  "missing fields uses defaults",
			input: `{"session_id":"abc123"}`,
			want: &TaskHookInput{
				SessionID:      "abc123",
				TranscriptPath: "",
				ToolUseID:      "",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.input)
			got, err := parseTaskHookInput(reader)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseTaskHookInput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want != nil {
				if got.SessionID != tt.want.SessionID {
					t.Errorf("SessionID = %v, want %v", got.SessionID, tt.want.SessionID)
				}
				if got.TranscriptPath != tt.want.TranscriptPath {
					t.Errorf("TranscriptPath = %v, want %v", got.TranscriptPath, tt.want.TranscriptPath)
				}
				if got.ToolUseID != tt.want.ToolUseID {
					t.Errorf("ToolUseID = %v, want %v", got.ToolUseID, tt.want.ToolUseID)
				}
			}
		})
	}
}

func TestParsePostTaskHookInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *PostTaskHookInput
		wantErr bool
	}{
		{
			name: "valid input with agent",
			input: `{
				"session_id": "abc123",
				"transcript_path": "/path/to/transcript.jsonl",
				"tool_use_id": "tool_xyz",
				"tool_input": {"prompt": "do something"},
				"tool_response": {"agentId": "agent_456"}
			}`,
			want: &PostTaskHookInput{
				TaskHookInput: TaskHookInput{
					SessionID:      "abc123",
					TranscriptPath: "/path/to/transcript.jsonl",
					ToolUseID:      "tool_xyz",
				},
				AgentID: "agent_456",
			},
			wantErr: false,
		},
		{
			name: "valid input without agent",
			input: `{
				"session_id": "abc123",
				"transcript_path": "/path/to/transcript.jsonl",
				"tool_use_id": "tool_xyz",
				"tool_input": {},
				"tool_response": {}
			}`,
			want: &PostTaskHookInput{
				TaskHookInput: TaskHookInput{
					SessionID:      "abc123",
					TranscriptPath: "/path/to/transcript.jsonl",
					ToolUseID:      "tool_xyz",
				},
				AgentID: "",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.input)
			got, err := parsePostTaskHookInput(reader)

			if (err != nil) != tt.wantErr {
				t.Errorf("parsePostTaskHookInput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want != nil {
				if got.SessionID != tt.want.SessionID {
					t.Errorf("SessionID = %v, want %v", got.SessionID, tt.want.SessionID)
				}
				if got.TranscriptPath != tt.want.TranscriptPath {
					t.Errorf("TranscriptPath = %v, want %v", got.TranscriptPath, tt.want.TranscriptPath)
				}
				if got.ToolUseID != tt.want.ToolUseID {
					t.Errorf("ToolUseID = %v, want %v", got.ToolUseID, tt.want.ToolUseID)
				}
				if got.AgentID != tt.want.AgentID {
					t.Errorf("AgentID = %v, want %v", got.AgentID, tt.want.AgentID)
				}
			}
		})
	}
}

func TestLogPreTaskHookContext(t *testing.T) {
	input := &TaskHookInput{
		SessionID:      "test-session-123",
		TranscriptPath: "/home/user/.claude/projects/myproject/transcript.jsonl",
		ToolUseID:      "toolu_abc123",
	}

	var buf bytes.Buffer
	logPreTaskHookContext(&buf, input)

	output := buf.String()

	// Check that all expected fields are present
	if !strings.Contains(output, "[entire] PreToolUse[Task] hook invoked") {
		t.Error("Missing hook header")
	}
	if !strings.Contains(output, "Session ID: test-session-123") {
		t.Error("Missing session ID")
	}
	if !strings.Contains(output, "Tool Use ID: toolu_abc123") {
		t.Error("Missing tool use ID")
	}
	if !strings.Contains(output, "Transcript:") {
		t.Error("Missing transcript path")
	}
}

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

func TestLogPostTaskHookContext(t *testing.T) {
	tests := []struct {
		name             string
		input            *PostTaskHookInput
		subagentPath     string
		wantAgentID      string
		wantSubagentPath string
	}{
		{
			name: "with agent",
			input: &PostTaskHookInput{
				TaskHookInput: TaskHookInput{
					SessionID:      "test-session-456",
					TranscriptPath: "/path/to/transcript.jsonl",
					ToolUseID:      "toolu_xyz789",
				},
				AgentID: "agent_subagent_001",
			},
			subagentPath:     "/path/to/agent-agent_subagent_001.jsonl",
			wantAgentID:      "Agent ID: agent_subagent_001",
			wantSubagentPath: "Subagent Transcript: /path/to/agent-agent_subagent_001.jsonl",
		},
		{
			name: "without agent",
			input: &PostTaskHookInput{
				TaskHookInput: TaskHookInput{
					SessionID:      "test-session-789",
					TranscriptPath: "/path/to/transcript.jsonl",
					ToolUseID:      "toolu_def456",
				},
				AgentID: "",
			},
			subagentPath:     "",
			wantAgentID:      "Agent ID: (none)",
			wantSubagentPath: "Subagent Transcript: (none)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logPostTaskHookContext(&buf, tt.input, tt.subagentPath)

			output := buf.String()

			if !strings.Contains(output, "[entire] PostToolUse[Task] hook invoked") {
				t.Error("Missing hook header")
			}
			if !strings.Contains(output, tt.wantAgentID) {
				t.Errorf("Missing or wrong agent ID, got:\n%s", output)
			}
			if !strings.Contains(output, tt.wantSubagentPath) {
				t.Errorf("Missing or wrong subagent path, got:\n%s", output)
			}
		})
	}
}

func TestHookResponse_SessionStart(t *testing.T) {
	t.Parallel()

	resp := hookResponse{
		SystemMessage: "Powered by Entire",
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: "Powered by Entire",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Verify the nested structure
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// systemMessage should be present (same as additionalContext for user visibility)
	if _, ok := raw["systemMessage"]; !ok {
		t.Error("systemMessage should be present for SessionStart")
	}

	// hookSpecificOutput should be present
	hsoRaw, ok := raw["hookSpecificOutput"]
	if !ok {
		t.Fatal("hookSpecificOutput missing from response")
	}

	var hso map[string]string
	if err := json.Unmarshal(hsoRaw, &hso); err != nil {
		t.Fatalf("failed to unmarshal hookSpecificOutput: %v", err)
	}

	if hso["hookEventName"] != "SessionStart" {
		t.Errorf("hookEventName = %q, want %q", hso["hookEventName"], "SessionStart")
	}
	if hso["additionalContext"] != "Powered by Entire" {
		t.Errorf("additionalContext = %q, want %q", hso["additionalContext"], "Powered by Entire")
	}
}

func TestHookResponse_UserPromptSubmit(t *testing.T) {
	t.Parallel()

	resp := hookResponse{
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "UserPromptSubmit",
			AdditionalContext: "Review instructions here",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// systemMessage should be absent
	if _, ok := raw["systemMessage"]; ok {
		t.Error("systemMessage should be omitted when empty")
	}

	hsoRaw, ok := raw["hookSpecificOutput"]
	if !ok {
		t.Fatal("hookSpecificOutput missing from response")
	}

	var hso map[string]string
	if err := json.Unmarshal(hsoRaw, &hso); err != nil {
		t.Fatalf("failed to unmarshal hookSpecificOutput: %v", err)
	}

	if hso["hookEventName"] != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want %q", hso["hookEventName"], "UserPromptSubmit")
	}
	if hso["additionalContext"] != "Review instructions here" {
		t.Errorf("additionalContext = %q, want %q", hso["additionalContext"], "Review instructions here")
	}
}

func TestHookResponse_WithContextAndMessage(t *testing.T) {
	t.Parallel()

	resp := hookResponse{
		SystemMessage: "[Wingman] A code review is pending.",
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "UserPromptSubmit",
			AdditionalContext: "Apply the review",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// systemMessage should be present
	var sysMsg string
	if err := json.Unmarshal(raw["systemMessage"], &sysMsg); err != nil {
		t.Fatalf("failed to unmarshal systemMessage: %v", err)
	}
	if sysMsg != "[Wingman] A code review is pending." {
		t.Errorf("systemMessage = %q, want %q", sysMsg, "[Wingman] A code review is pending.")
	}

	// hookSpecificOutput should also be present
	hsoRaw, ok := raw["hookSpecificOutput"]
	if !ok {
		t.Fatal("hookSpecificOutput missing from response")
	}

	var hso map[string]string
	if err := json.Unmarshal(hsoRaw, &hso); err != nil {
		t.Fatalf("failed to unmarshal hookSpecificOutput: %v", err)
	}

	if hso["hookEventName"] != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want %q", hso["hookEventName"], "UserPromptSubmit")
	}
	if hso["additionalContext"] != "Apply the review" {
		t.Errorf("additionalContext = %q, want %q", hso["additionalContext"], "Apply the review")
	}
}

func TestHookResponse_NilHookSpecificOutput(t *testing.T) {
	t.Parallel()

	resp := hookResponse{
		SystemMessage: "Just a message",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// hookSpecificOutput should be absent (omitempty on pointer)
	if _, ok := raw["hookSpecificOutput"]; ok {
		t.Error("hookSpecificOutput should be omitted when nil")
	}

	var sysMsg string
	if err := json.Unmarshal(raw["systemMessage"], &sysMsg); err != nil {
		t.Fatalf("failed to unmarshal systemMessage: %v", err)
	}
	if sysMsg != "Just a message" {
		t.Errorf("systemMessage = %q, want %q", sysMsg, "Just a message")
	}
}
