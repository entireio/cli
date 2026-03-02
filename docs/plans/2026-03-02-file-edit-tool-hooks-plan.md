# File Edit Tool Hooks Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Track agent file modifications in real time via tool-call hooks to fix orphaned checkpoints and enable future attribution.

**Architecture:** New `post-file-edit` hook command receives Write/Edit tool payloads from Claude Code, updates `FilesTouched` in session state immediately, and appends detailed edit records to an append-only JSONL log. The log is consumed at condensation time and stored in checkpoint metadata.

**Tech Stack:** Go, go-git, Claude Code postToolUse hooks with matchers

---

### Task 1: Add FileEdit types to agent package

**Files:**
- Modify: `cmd/entire/cli/agent/types.go`
- Create: `cmd/entire/cli/agent/types_test.go`

**Step 1: Write the test for FileEdit struct and line computation helpers**

```go
// cmd/entire/cli/agent/types_test.go
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

func TestNewFileEdit(t *testing.T) {
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
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/agent/ -run TestCountLines -v`
Expected: FAIL — `CountLines` and `FileEdit` not defined

**Step 3: Write minimal implementation**

Add to `cmd/entire/cli/agent/types.go`:

```go
// FileEditAction represents the type of file edit operation.
type FileEditAction string

const (
	// FileEditActionWrite represents a Write tool operation (create or overwrite).
	FileEditActionWrite FileEditAction = "write"
	// FileEditActionEdit represents an Edit tool operation (modify existing file).
	FileEditActionEdit FileEditAction = "edit"
)

// FileEdit represents a single file modification by an agent tool.
// Stored in append-only JSONL logs per session for real-time file tracking
// and future attribution computation.
type FileEdit struct {
	// FilePath is the repo-relative path to the modified file.
	FilePath string `json:"file_path"`
	// Action is the type of edit (write or edit).
	Action FileEditAction `json:"action"`
	// ToolName is the agent tool that performed the edit (e.g., "Write", "Edit").
	ToolName string `json:"tool_name"`
	// LinesAdded is the number of lines added by this edit.
	LinesAdded int `json:"lines_added"`
	// LinesRemoved is the number of lines removed by this edit.
	LinesRemoved int `json:"lines_removed"`
	// Timestamp is when the edit occurred.
	Timestamp time.Time `json:"timestamp"`
}

// CountLines counts the number of lines in a string.
// Empty string returns 0. A string with no newlines returns 1.
// Trailing newlines are not counted as an additional line.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	// If the string doesn't end with a newline, there's one more line
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
```

Add `"strings"` to imports in `types.go`.

**Step 4: Run test to verify it passes**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/agent/ -run "TestCountLines|TestNewFileEdit" -v`
Expected: PASS

**Step 5: Commit**

```bash
git add cmd/entire/cli/agent/types.go cmd/entire/cli/agent/types_test.go
git commit -m "feat: add FileEdit types and CountLines helper to agent package"
```

---

### Task 2: Add FileEdit JSONL operations to session package

**Files:**
- Modify: `cmd/entire/cli/session/state.go`
- Create: `cmd/entire/cli/session/file_edits.go`
- Create: `cmd/entire/cli/session/file_edits_test.go`

**Step 1: Write tests for AppendFileEdit, ReadFileEdits, ClearFileEdits**

```go
// cmd/entire/cli/session/file_edits_test.go
package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestAppendAndReadFileEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	sessionID := "2026-03-02-test-session"
	edit1 := agent.FileEdit{
		FilePath:   "cmd/main.go",
		Action:     agent.FileEditActionEdit,
		ToolName:   "Edit",
		LinesAdded: 5,
		LinesRemoved: 2,
		Timestamp:  time.Now(),
	}
	edit2 := agent.FileEdit{
		FilePath:   "cmd/server.go",
		Action:     agent.FileEditActionWrite,
		ToolName:   "Write",
		LinesAdded: 100,
		Timestamp:  time.Now(),
	}

	// Append two edits
	if err := store.AppendFileEdit(sessionID, edit1); err != nil {
		t.Fatalf("AppendFileEdit(1): %v", err)
	}
	if err := store.AppendFileEdit(sessionID, edit2); err != nil {
		t.Fatalf("AppendFileEdit(2): %v", err)
	}

	// Read them back
	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("ReadFileEdits returned %d edits, want 2", len(edits))
	}
	if edits[0].FilePath != "cmd/main.go" {
		t.Errorf("edits[0].FilePath = %q, want %q", edits[0].FilePath, "cmd/main.go")
	}
	if edits[1].FilePath != "cmd/server.go" {
		t.Errorf("edits[1].FilePath = %q, want %q", edits[1].FilePath, "cmd/server.go")
	}
}

func TestClearFileEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	sessionID := "2026-03-02-clear-test"
	edit := agent.FileEdit{
		FilePath: "file.go",
		Action:   agent.FileEditActionEdit,
		ToolName: "Edit",
	}

	if err := store.AppendFileEdit(sessionID, edit); err != nil {
		t.Fatalf("AppendFileEdit: %v", err)
	}

	if err := store.ClearFileEdits(sessionID); err != nil {
		t.Fatalf("ClearFileEdits: %v", err)
	}

	// Should return empty after clear
	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits after clear: %v", err)
	}
	if len(edits) != 0 {
		t.Errorf("ReadFileEdits after clear returned %d edits, want 0", len(edits))
	}
}

func TestReadFileEdits_NonExistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	edits, err := store.ReadFileEdits("nonexistent")
	if err != nil {
		t.Fatalf("ReadFileEdits for nonexistent: %v", err)
	}
	if len(edits) != 0 {
		t.Errorf("ReadFileEdits for nonexistent returned %d edits, want 0", len(edits))
	}
}

func TestFileEditsPath(t *testing.T) {
	t.Parallel()
	store := NewStateStoreWithDir("/tmp/test")
	path := store.fileEditsPath("my-session")
	expected := filepath.Join("/tmp/test", "my-session-edits.jsonl")
	if path != expected {
		t.Errorf("fileEditsPath = %q, want %q", path, expected)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/session/ -run "TestAppendAndReadFileEdits|TestClearFileEdits|TestReadFileEdits_NonExistent|TestFileEditsPath" -v`
Expected: FAIL — methods not defined

**Step 3: Write implementation**

Create `cmd/entire/cli/session/file_edits.go`:

```go
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// fileEditsPath returns the path to the file edits JSONL file for a session.
func (s *StateStore) fileEditsPath(sessionID string) string {
	return filepath.Join(s.stateDir, sessionID+"-edits.jsonl")
}

// AppendFileEdit appends a single FileEdit record to the session's edit log.
// This is an append-only operation — no read-modify-write cycle.
func (s *StateStore) AppendFileEdit(sessionID string, edit agent.FileEdit) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	if err := os.MkdirAll(s.stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open file edits log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(edit)
	if err != nil {
		return fmt.Errorf("failed to marshal file edit: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write file edit: %w", err)
	}
	return nil
}

// ReadFileEdits reads all FileEdit records from the session's edit log.
// Returns empty slice (not error) if the log doesn't exist.
func (s *StateStore) ReadFileEdits(sessionID string) ([]agent.FileEdit, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open file edits log: %w", err)
	}
	defer f.Close()

	var edits []agent.FileEdit
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var edit agent.FileEdit
		if err := json.Unmarshal(line, &edit); err != nil {
			continue // Skip malformed lines
		}
		edits = append(edits, edit)
	}
	if err := scanner.Err(); err != nil {
		return edits, fmt.Errorf("failed to read file edits log: %w", err)
	}
	return edits, nil
}

// ClearFileEdits removes the session's edit log file.
func (s *StateStore) ClearFileEdits(sessionID string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file edits log: %w", err)
	}
	return nil
}
```

**Step 4: Run tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/session/ -run "TestAppendAndReadFileEdits|TestClearFileEdits|TestReadFileEdits_NonExistent|TestFileEditsPath" -v`
Expected: PASS

**Step 5: Commit**

```bash
git add cmd/entire/cli/session/file_edits.go cmd/entire/cli/session/file_edits_test.go
git commit -m "feat: add FileEdit JSONL append/read/clear to session StateStore"
```

---

### Task 3: Add post-file-edit hook constant and Claude Code hook installation

**Files:**
- Modify: `cmd/entire/cli/agent/claudecode/hooks.go`
- Modify: `cmd/entire/cli/agent/claudecode/hooks_test.go` (if exists, or create)

**Step 1: Write test for hook installation including Write/Edit matchers**

Add a test that verifies `InstallHooks` adds PostToolUse matchers for `Write` and `Edit` pointing to the `post-file-edit` command. Follow the pattern of existing hook install tests.

**Step 2: Add the constant and modify InstallHooks**

Add to `hooks.go` constants:

```go
HookNamePostFileEdit = "post-file-edit"
```

Add to `GetHookNames()`:

```go
HookNamePostFileEdit,
```

Add to `InstallHooks()` — in the hook command definitions section, add:

```go
var postFileEditCmd string
if localDev {
    postFileEditCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code post-file-edit"
} else {
    postFileEditCmd = "entire hooks claude-code post-file-edit"
}
```

And in the hook installation section, add matchers for Write and Edit:

```go
if !hookCommandExistsWithMatcher(postToolUse, "Write", postFileEditCmd) {
    postToolUse = addHookToMatcher(postToolUse, "Write", postFileEditCmd)
    count++
}
if !hookCommandExistsWithMatcher(postToolUse, "Edit", postFileEditCmd) {
    postToolUse = addHookToMatcher(postToolUse, "Edit", postFileEditCmd)
    count++
}
```

**Step 3: Run tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/agent/claudecode/ -v`
Expected: PASS

**Step 4: Commit**

```bash
git add cmd/entire/cli/agent/claudecode/hooks.go
git commit -m "feat: add post-file-edit hook with Write/Edit matchers for Claude Code"
```

---

### Task 4: Add file edit hook input parsing

**Files:**
- Modify: `cmd/entire/cli/hooks.go`
- Create: `cmd/entire/cli/hooks_file_edit_test.go`

**Step 1: Write test for parsing PostToolUse[Write] and PostToolUse[Edit] inputs**

```go
// cmd/entire/cli/hooks_file_edit_test.go
package cli

import (
	"strings"
	"testing"
)

func TestParseFileEditHookInput_Write(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "abc-123",
		"tool_name": "Write",
		"tool_use_id": "tu-1",
		"tool_input": {"file_path": "/repo/cmd/main.go", "content": "package main\n\nfunc main() {\n}\n"},
		"tool_response": ""
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput: %v", err)
	}
	if parsed.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want %q", parsed.SessionID, "abc-123")
	}
	if parsed.ToolName != "Write" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Write")
	}
	if parsed.FilePath != "/repo/cmd/main.go" {
		t.Errorf("FilePath = %q, want %q", parsed.FilePath, "/repo/cmd/main.go")
	}
	if parsed.LinesAdded != 4 {
		t.Errorf("LinesAdded = %d, want 4", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 0 {
		t.Errorf("LinesRemoved = %d, want 0", parsed.LinesRemoved)
	}
}

func TestParseFileEditHookInput_Edit(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "abc-123",
		"tool_name": "Edit",
		"tool_use_id": "tu-2",
		"tool_input": {"file_path": "/repo/cmd/main.go", "old_string": "line1\nline2\n", "new_string": "line1\nline2\nline3\nline4\n"},
		"tool_response": ""
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput: %v", err)
	}
	if parsed.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Edit")
	}
	if parsed.LinesAdded != 4 {
		t.Errorf("LinesAdded = %d, want 4", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 2 {
		t.Errorf("LinesRemoved = %d, want 2", parsed.LinesRemoved)
	}
}
```

**Step 2: Run tests to verify fail**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/ -run "TestParseFileEditHookInput" -v`
Expected: FAIL

**Step 3: Implement input parsing**

Add to `cmd/entire/cli/hooks.go` (or create `hooks_file_edit.go`):

```go
// FileEditHookInput represents parsed input from PostToolUse[Write|Edit] hooks.
type FileEditHookInput struct {
	SessionID    string
	ToolName     string
	ToolUseID    string
	FilePath     string
	LinesAdded   int
	LinesRemoved int
}

// fileEditToolInputWrite is the tool_input for Write tool
type fileEditToolInputWrite struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// fileEditToolInputEdit is the tool_input for Edit tool
type fileEditToolInputEdit struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// parseFileEditHookInput parses PostToolUse[Write|Edit] hook input.
func parseFileEditHookInput(r io.Reader) (*FileEditHookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var raw SubagentCheckpointHookInput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	result := &FileEditHookInput{
		SessionID: raw.SessionID,
		ToolName:  raw.ToolName,
		ToolUseID: raw.ToolUseID,
	}

	switch raw.ToolName {
	case "Write":
		var input fileEditToolInputWrite
		if err := json.Unmarshal(raw.ToolInput, &input); err != nil {
			return nil, fmt.Errorf("failed to parse Write tool_input: %w", err)
		}
		result.FilePath = input.FilePath
		result.LinesAdded = agent.CountLines(input.Content)
		result.LinesRemoved = 0
	case "Edit":
		var input fileEditToolInputEdit
		if err := json.Unmarshal(raw.ToolInput, &input); err != nil {
			return nil, fmt.Errorf("failed to parse Edit tool_input: %w", err)
		}
		result.FilePath = input.FilePath
		result.LinesAdded = agent.CountLines(input.NewString)
		result.LinesRemoved = agent.CountLines(input.OldString)
	default:
		return nil, fmt.Errorf("unsupported tool: %s", raw.ToolName)
	}

	return result, nil
}
```

**Step 4: Run tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && go test ./cmd/entire/cli/ -run "TestParseFileEditHookInput" -v`
Expected: PASS

**Step 5: Commit**

```bash
git add cmd/entire/cli/hooks.go cmd/entire/cli/hooks_file_edit_test.go
# or hooks_file_edit.go if you created a new file
git commit -m "feat: add file edit hook input parsing for Write and Edit tools"
```

---

### Task 5: Implement the post-file-edit handler and register it

**Files:**
- Modify: `cmd/entire/cli/hooks_claudecode_handlers.go`
- Modify: `cmd/entire/cli/hook_registry.go`

**Step 1: Write the handler**

Add to `hooks_claudecode_handlers.go`:

```go
// handleClaudeCodePostFileEdit handles the PostToolUse[Write|Edit] hook.
// Updates FilesTouched in session state and appends to the file edits log.
func handleClaudeCodePostFileEdit() error {
	input, err := parseFileEditHookInput(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to parse PostToolUse file edit input: %w", err)
	}

	ag, err := GetCurrentHookAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), ag.Name())
	logging.Debug(logCtx, "post-file-edit",
		slog.String("hook", "post-file-edit"),
		slog.String("hook_type", "tool"),
		slog.String("session_id", input.SessionID),
		slog.String("tool_name", input.ToolName),
		slog.String("file_path", input.FilePath),
	)

	sessionID := input.SessionID
	if sessionID == "" {
		return nil // No session context
	}

	// Normalize file path to repo-relative
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}
	relPath := paths.ToRelativePath(input.FilePath, repoRoot)
	if relPath == "" {
		// Path is outside repo, skip
		return nil
	}

	// Determine action
	var action agent.FileEditAction
	switch input.ToolName {
	case "Write":
		action = agent.FileEditActionWrite
	default:
		action = agent.FileEditActionEdit
	}

	// Build the FileEdit record
	edit := agent.FileEdit{
		FilePath:     relPath,
		Action:       action,
		ToolName:     input.ToolName,
		LinesAdded:   input.LinesAdded,
		LinesRemoved: input.LinesRemoved,
		Timestamp:    time.Now(),
	}

	// Append to JSONL log (fast, append-only)
	store, err := session.NewStateStore()
	if err != nil {
		return fmt.Errorf("failed to create state store: %w", err)
	}
	if err := store.AppendFileEdit(sessionID, edit); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to append file edit: %v\n", err)
		// Continue to update FilesTouched even if JSONL append fails
	}

	// Merge file path into FilesTouched in session state
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load session state: %v\n", err)
		return nil // Non-fatal: session state may not exist yet
	}
	if state == nil {
		return nil // Session not initialized yet
	}

	// Check if file already tracked
	for _, f := range state.FilesTouched {
		if f == relPath {
			return nil // Already tracked
		}
	}
	state.FilesTouched = append(state.FilesTouched, relPath)

	if err := store.Save(context.Background(), state); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save session state: %v\n", err)
	}

	return nil
}
```

**Step 2: Register the handler in hook_registry.go**

Add to the Claude Code section in `init()`:

```go
RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNamePostFileEdit, func() error {
    enabled, err := IsEnabled()
    if err == nil && !enabled {
        return nil
    }
    return handleClaudeCodePostFileEdit()
})
```

**Step 3: Update getHookType in hook_registry.go**

Add `post-file-edit` to the tool category:

```go
case geminicli.HookNameBeforeTool, geminicli.HookNameAfterTool, claudecode.HookNamePostFileEdit:
    return "tool"
```

**Step 4: Run all tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && mise run fmt && mise run lint && mise run test`
Expected: PASS

**Step 5: Commit**

```bash
git add cmd/entire/cli/hooks_claudecode_handlers.go cmd/entire/cli/hook_registry.go
git commit -m "feat: implement post-file-edit handler with FilesTouched update and JSONL logging"
```

---

### Task 6: Include file edits in checkpoint metadata at condensation time

**Files:**
- Modify: `cmd/entire/cli/strategy/manual_commit_condensation.go`
- Modify: `cmd/entire/cli/checkpoint/checkpoint.go` (add FileEdits field to WriteCommittedOptions)
- Modify: `cmd/entire/cli/checkpoint/committed.go` (write file_edits.jsonl to tree)

**Step 1: Add FileEdits to WriteCommittedOptions**

In `checkpoint.go`, add to `WriteCommittedOptions`:

```go
// FileEdits contains the file edit log entries for this checkpoint.
// When non-nil, stored as file_edits.jsonl in the checkpoint tree.
FileEdits []agent.FileEdit
```

**Step 2: Write file_edits.jsonl in committed.go**

In the `WriteCommitted` method, after writing other files, add code to write `file_edits.jsonl` if `opts.FileEdits` is non-empty:

```go
if len(opts.FileEdits) > 0 {
    var buf bytes.Buffer
    for _, edit := range opts.FileEdits {
        line, err := json.Marshal(edit)
        if err != nil {
            continue
        }
        buf.Write(line)
        buf.WriteByte('\n')
    }
    // Add file_edits.jsonl to the session directory in the tree
    // (alongside full.jsonl, prompt.txt, etc.)
}
```

**Step 3: Pass file edits from condensation**

In `manual_commit_condensation.go`, in the `condenseSession` method, load file edits from the session's JSONL log and pass them to `WriteCommitted`:

```go
// Load file edits from JSONL log
store, storeErr := session.NewStateStore()
if storeErr == nil {
    fileEdits, readErr := store.ReadFileEdits(state.SessionID)
    if readErr == nil && len(fileEdits) > 0 {
        // Pass to WriteCommitted
    }
}
```

After successful condensation, clear the edit log:

```go
if storeErr == nil {
    _ = store.ClearFileEdits(state.SessionID)
}
```

**Step 4: Run tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && mise run fmt && mise run lint && mise run test`
Expected: PASS

**Step 5: Commit**

```bash
git add cmd/entire/cli/strategy/manual_commit_condensation.go cmd/entire/cli/checkpoint/checkpoint.go cmd/entire/cli/checkpoint/committed.go
git commit -m "feat: include file edits in checkpoint metadata at condensation time"
```

---

### Task 7: Integration test — file edit hook updates FilesTouched for mid-turn commit

**Files:**
- Create: `cmd/entire/cli/integration_test/file_edit_hooks_test.go`

**Step 1: Write integration test**

Write an integration test that simulates:
1. Session starts (user-prompt-submit)
2. Agent edits a file (post-file-edit hook fires)
3. User commits mid-turn
4. PostCommit overlap check succeeds because FilesTouched was updated by the hook

Follow the patterns in existing integration tests in `cmd/entire/cli/integration_test/`.

**Step 2: Run integration tests**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && mise run test:integration`
Expected: PASS

**Step 3: Commit**

```bash
git add cmd/entire/cli/integration_test/file_edit_hooks_test.go
git commit -m "test: add integration test for file edit hooks fixing mid-turn commit overlap"
```

---

### Task 8: Final validation and cleanup

**Step 1: Run full CI suite**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && mise run fmt && mise run lint && mise run test:ci`
Expected: PASS

**Step 2: Run duplication check**

Run: `cd /Users/alex/workspace/cli/.worktrees/2 && mise run dup:staged`
Expected: No significant duplication

**Step 3: Commit any cleanup**

If fmt/lint/dup found issues, fix and commit.
