package strategy

import (
	"strings"
	"testing"
)

// TestSanitizeSubjectContent_StripsUnsafeControls covers the characters a JSON
// string may legally carry that must never reach a commit subject: NUL, ESC,
// C1/DEL, and the Unicode bidi controls that can make `git log` render
// something other than what was committed.
func TestSanitizeSubjectContent_StripsUnsafeControls(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"nul", "before\x00after", "beforeafter"},
		{"escape", "before\x1b[31mred\x1b[0m", "before[31mred[0m"},
		{"del", "before\x7fafter", "beforeafter"},
		{"c1", "before\x9bafter", "beforeafter"},
		{"bidi override", "safe.txt\u202e" + "gnp.exe", "safe.txtgnp.exe"},
		{"bidi isolate", "a\u2066b\u2069c", "abc"},
		{"zero width joiner", "a\u200db", "ab"},
		{"newlines collapse to one space", "line one\nline two", "line one line two"},
		{"tabs collapse", "a\t\tb", "a b"},
		{"line separator collapses", "a\u2028b", "a b"},
		{"surrounding whitespace trimmed", "  padded  ", "padded"},
		{"normal unicode preserved", "café 日本語 🎉", "café 日本語 🎉"},
		{"only controls becomes empty", "\x00\x1b\u202e", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeSubjectContent(tt.input); got != tt.want {
				t.Errorf("SanitizeSubjectContent(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestFormatSubagentEndMessage_RedactsSecretBeforeGit is the regression test for
// model output becoming Git metadata. Codex forwards the whole
// last_assistant_message into TaskDescription, so a reply that opens with a
// credential copied out of tool output would otherwise be committed verbatim.
func TestFormatSubagentEndMessage_RedactsSecretBeforeGit(t *testing.T) {
	const secret = "ghp_1234567890abcdefghijklmnopqrstuvwx"

	got := FormatSubagentEndMessage("dev", secret+" is the key I used", "toolu_019t1c")
	if strings.Contains(got, secret) {
		t.Errorf("commit subject must not contain the raw secret, got %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("commit subject should carry a redaction placeholder, got %q", got)
	}
}

// TestSanitizeSubjectContent_RedactsBeforeTruncating pins that secrets inside
// the redaction window are removed even when the raw input exceeds it.
func TestSanitizeSubjectContent_RedactsBeforeTruncating(t *testing.T) {
	const secret = "ghp_1234567890abcdefghijklmnopqrstuvwx"
	description := strings.Repeat("a", 100) + " " + secret + " " + strings.Repeat("b", maxSubjectRedactionInput)

	got := SanitizeSubjectContent(description)
	if strings.Contains(got, secret[:6]) {
		t.Errorf("secret inside the bounded redaction window must be redacted, got %q", got)
	}
}

// TestFormatSubagentEndMessage_RedactsBeforeTruncating pins the ordering.
// Truncating first can cut a secret short of any rule's match, leaving the
// surviving prefix in the subject.
func TestFormatSubagentEndMessage_RedactsBeforeTruncating(t *testing.T) {
	const secret = "ghp_1234567890abcdefghijklmnopqrstuvwx"
	// Push the secret across the MaxDescriptionLength boundary so truncation
	// would keep only its first few characters.
	description := strings.Repeat("a", MaxDescriptionLength-10) + " " + secret

	got := FormatSubagentEndMessage("dev", description, "toolu_019t1c")
	if strings.Contains(got, secret[:6]) {
		t.Errorf("a secret straddling the truncation boundary must be redacted before truncation, got %q", got)
	}
}

// TestFormatIncrementalMessage_SanitizesTodoContent confirms the same boundary
// guards TodoWrite content, which is model output on the same path.
func TestFormatIncrementalMessage_SanitizesTodoContent(t *testing.T) {
	got := FormatIncrementalMessage("wire\x1b[31m the\nparser", 1, "toolu_01CJhrr")
	want := "wire[31m the parser (toolu_01CJhrr)"
	if got != want {
		t.Errorf("FormatIncrementalMessage() = %q, want %q", got, want)
	}

	// All-control content is empty after sanitizing, so it must take the
	// no-content fallback rather than emit a blank subject.
	if got := FormatIncrementalMessage("\x00\x1b", 3, "toolu_01CJhrr"); got != "Checkpoint #3: toolu_01CJhrr" {
		t.Errorf("all-control todo content should fall back to the checkpoint format, got %q", got)
	}
}

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

func TestFormatIncrementalMessage(t *testing.T) {
	t.Parallel()

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
			t.Parallel()
			got := FormatIncrementalMessage(tt.todoContent, tt.sequence, tt.toolUseID)
			if got != tt.want {
				t.Errorf("FormatIncrementalMessage(%q, %d, %q) = %q, want %q",
					tt.todoContent, tt.sequence, tt.toolUseID, got, tt.want)
			}
		})
	}
}

func TestExtractLastCompletedTodo(t *testing.T) {
	t.Parallel()

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
			t.Parallel()
			got := ExtractLastCompletedTodo([]byte(tt.todosJSON))
			if got != tt.want {
				t.Errorf("ExtractLastCompletedTodo(%s) = %q, want %q", tt.todosJSON, got, tt.want)
			}
		})
	}
}

func TestCountTodos(t *testing.T) {
	t.Parallel()

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
			t.Parallel()
			got := CountTodos([]byte(tt.todosJSON))
			if got != tt.want {
				t.Errorf("CountTodos(%s) = %d, want %d", tt.todosJSON, got, tt.want)
			}
		})
	}
}
