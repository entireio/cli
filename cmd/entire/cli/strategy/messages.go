package strategy

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/redact"
)

// MaxDescriptionLength is the maximum length for descriptions in commit messages
// before truncation occurs.
const MaxDescriptionLength = 60

// maxSubjectRedactionInput bounds how much agent-supplied text is fed to the
// redaction pipeline before truncation. Sources like Codex's
// last_assistant_message are whole model replies, so redacting them in full
// would put an unbounded cost on a hook path. The bound is far above
// MaxDescriptionLength, so any secret that could still be visible after
// truncation is inside the window; everything past it is dropped by
// TruncateDescription regardless.
const maxSubjectRedactionInput = 4096

// omittedSubjectLabel is returned when agent-supplied text exceeds the redaction
// window. Truncating before redact would let a secret straddling the boundary
// leak its prefix; failing closed is safer than a partial scan.
const omittedSubjectLabel = "[agent description omitted]"

// SanitizeSubjectContent prepares agent-supplied text for use inside a
// single-line Git commit subject.
//
// Model output reaches commit subjects verbatim (a subagent's task description,
// a TodoWrite item), so this is the boundary where two guarantees have to hold:
//
//  1. Detected secrets are replaced before anything is written to Git. A reply
//     that opens with a credential copied out of tool output would otherwise
//     become an unredacted Git object.
//  2. The subject renders as inert text. JSON strings may legally carry NUL,
//     ESC, C1/DEL, and Unicode bidi/format controls, which survive whitespace
//     collapsing and can make `git log`, rewind output, and terminal UIs
//     display something other than what was committed.
//
// Redaction runs on inputs up to maxSubjectRedactionInput runes. Inputs larger
// than that fail closed to omittedSubjectLabel so a secret straddling the
// boundary cannot leak a prefix that truncation would have cut mid-match.
func SanitizeSubjectContent(s string) string {
	if utf8.RuneCountInString(s) > maxSubjectRedactionInput {
		return omittedSubjectLabel
	}
	s = stripSubjectControls(s)
	if s == "" {
		return ""
	}
	s = redact.String(s)
	if utf8.RuneCountInString(s) > MaxDescriptionLength {
		return omittedSubjectLabel
	}
	// Redaction placeholders are plain text, but a custom rule's replacement is
	// caller-supplied, so re-run the control strip rather than trust it.
	return stripSubjectControls(s)
}

// SanitizeTaskDescription redacts secrets and strips unsafe controls from task
// descriptions persisted in checkpoint metadata. Unlike SanitizeSubjectContent
// it does not apply commit-subject length limits — task.json is not a subject.
func SanitizeTaskDescription(s string) string {
	if utf8.RuneCountInString(s) > maxSubjectRedactionInput {
		return omittedSubjectLabel
	}
	s = stripSubjectControls(s)
	if s == "" {
		return ""
	}
	s = redact.String(s)
	return stripSubjectControls(s)
}

// stripSubjectControls removes characters that must never reach a commit
// subject and folds every whitespace run into a single space, preserving normal
// Unicode (including non-Latin scripts and emoji) otherwise.
//
// Dropped: C0 controls, DEL, C1 controls, and Unicode format characters
// (category Cf — the bidi overrides and isolates, plus zero-width joiners).
// Invalid UTF-8 is dropped too, since Git subjects must be well-formed text.
func stripSubjectControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			// Either invalid UTF-8 or a literal U+FFFD; neither belongs here.
			continue
		case unicode.IsSpace(r):
			// Covers tab/newline plus U+2028/U+2029, which render as breaks.
			// Leading whitespace is dropped by never arming before the first rune.
			pendingSpace = b.Len() > 0
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		default:
			if pendingSpace {
				b.WriteRune(' ')
				pendingSpace = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TruncateDescription truncates a string to maxLen runes, adding "..." if truncated.
// Uses rune-based slicing to avoid splitting multi-byte UTF-8 characters.
// If maxLen is less than 3, truncates without ellipsis.
func TruncateDescription(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return stringutil.TruncateRunes(s, maxLen, "")
	}
	return stringutil.TruncateRunes(s, maxLen, "...")
}

// FormatSubagentEndMessage formats a commit message for when a subagent completes.
// Format: "Completed '<agent-type>' agent: <description> (<tool-use-id>)"
//
// Edge cases:
//   - Empty description: "Completed '<agent-type>' agent (<tool-use-id>)"
//   - Empty agentType: "Completed agent: <description> (<tool-use-id>)"
//   - Both empty: "Task: <tool-use-id>"
func FormatSubagentEndMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Completed", agentType, description, toolUseID)
}

// FormatSubagentRunningMessage is FormatSubagentEndMessage's in-flight
// counterpart, for pending-list rows whose task record is still live.
func FormatSubagentRunningMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Running", agentType, description, toolUseID)
}

// formatSubagentMessage is a shared helper for start/end messages.
func formatSubagentMessage(verb, agentType, description, toolUseID string) string {
	// Both fields are agent-supplied and land in a commit subject verbatim.
	// The description carries model output, so it also needs redaction; the
	// agent type is a label, and redacting it would mangle legitimate names.
	agentType = stripSubjectControls(agentType)
	description = SanitizeSubjectContent(description)

	// Both empty - fall back to simple format
	if agentType == "" && description == "" {
		return "Task: " + toolUseID
	}

	// Truncate description if needed
	if description != "" {
		description = TruncateDescription(description, MaxDescriptionLength)
	}

	// Build message based on what fields are present
	if agentType != "" && description != "" {
		return fmt.Sprintf("%s '%s' agent: %s (%s)", verb, agentType, description, toolUseID)
	}
	if agentType != "" {
		return fmt.Sprintf("%s '%s' agent (%s)", verb, agentType, toolUseID)
	}
	// agentType is empty, description is present
	return fmt.Sprintf("%s agent: %s (%s)", verb, description, toolUseID)
}

// FormatIncrementalMessage formats a commit message for an incremental checkpoint.
// Format: "<todo-content> (<tool-use-id>)"
//
// If todoContent is empty, falls back to: "Checkpoint #<sequence>: <tool-use-id>"
func FormatIncrementalMessage(todoContent string, sequence int, toolUseID string) string {
	// Todo content is model output on its way into a commit subject.
	todoContent = SanitizeSubjectContent(todoContent)
	if todoContent == "" {
		return fmt.Sprintf("Checkpoint #%d: %s", sequence, toolUseID)
	}

	// Truncate todo content if needed
	todoContent = TruncateDescription(todoContent, MaxDescriptionLength)
	return fmt.Sprintf("%s (%s)", todoContent, toolUseID)
}

// todoItem represents a single item in the TodoWrite tool_input.todos array.
type todoItem struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"`
}

// ExtractLastCompletedTodo extracts the content of the last completed todo item from tool_input.
// This represents the work that was just finished and is used for commit messages.
//
// When TodoWrite is called in PostToolUse, the NEW list is provided which has the
// just-completed work marked as "completed". The last completed item is the most
// recently finished task.
//
// Returns empty string if no completed items exist or JSON is invalid.
func ExtractLastCompletedTodo(todosJSON []byte) string {
	if len(todosJSON) == 0 {
		return ""
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return ""
	}

	// Find the last completed item - this is the work that was just finished
	var lastCompleted string
	for _, todo := range todos {
		if todo.Status == "completed" {
			lastCompleted = todo.Content
		}
	}
	return lastCompleted
}

// CountTodos returns the number of todo items in the JSON array.
// Returns 0 if the JSON is invalid or empty.
func CountTodos(todosJSON []byte) int {
	if len(todosJSON) == 0 {
		return 0
	}

	var todos []todoItem
	if err := json.Unmarshal(todosJSON, &todos); err != nil {
		return 0
	}

	return len(todos)
}
