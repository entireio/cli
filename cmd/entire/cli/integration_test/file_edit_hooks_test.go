//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestPostFileEdit_UpdatesFilesTouched verifies that the post-file-edit hook
// updates FilesTouched in session state when a Write tool is used.
func TestPostFileEdit_UpdatesFilesTouched(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-1"

	// Start a session turn (initializes session state)
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Build the post-file-edit hook input (PostToolUse[Write] format)
	filePath := filepath.Join(env.RepoDir, "newfile.go")
	hookInput := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Write",
		"tool_use_id": "toolu_write1",
		"tool_input": map[string]interface{}{
			"file_path": filePath,
			"content":   "package main\n\nfunc main() {}\n",
		},
	}
	inputJSON, err := json.Marshal(hookInput)
	if err != nil {
		t.Fatalf("failed to marshal hook input: %v", err)
	}

	// Invoke the post-file-edit hook via CLI binary
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
		t.Fatalf("post-file-edit hook failed: %v", err)
	}

	// Read session state and verify FilesTouched
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state is nil")
	}

	if len(state.FilesTouched) != 1 {
		t.Fatalf("FilesTouched = %v, want 1 entry", state.FilesTouched)
	}
	if state.FilesTouched[0] != "newfile.go" {
		t.Errorf("FilesTouched[0] = %q, want %q", state.FilesTouched[0], "newfile.go")
	}
}

// TestPostFileEdit_CreatesEditLog verifies that the post-file-edit hook
// creates a JSONL edit log file with the correct FileEdit record.
func TestPostFileEdit_CreatesEditLog(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-2"

	// Start a session turn
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Build the post-file-edit hook input for a Write tool
	filePath := filepath.Join(env.RepoDir, "hello.go")
	content := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	hookInput := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Write",
		"tool_use_id": "toolu_write2",
		"tool_input": map[string]interface{}{
			"file_path": filePath,
			"content":   content,
		},
	}
	inputJSON, err := json.Marshal(hookInput)
	if err != nil {
		t.Fatalf("failed to marshal hook input: %v", err)
	}

	// Invoke the post-file-edit hook
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
		t.Fatalf("post-file-edit hook failed: %v", err)
	}

	// Read the JSONL edit log
	stateStore := session.NewStateStoreWithDir(
		filepath.Join(env.RepoDir, ".git", session.SessionStateDirName),
	)
	edits, err := stateStore.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("failed to read file edits: %v", err)
	}
	if len(edits) != 1 {
		t.Fatalf("edits count = %d, want 1", len(edits))
	}

	edit := edits[0]
	if edit.FilePath != "hello.go" {
		t.Errorf("edit.FilePath = %q, want %q", edit.FilePath, "hello.go")
	}
	if edit.Action != agent.FileEditActionWrite {
		t.Errorf("edit.Action = %q, want %q", edit.Action, agent.FileEditActionWrite)
	}
	if edit.ToolName != "Write" {
		t.Errorf("edit.ToolName = %q, want %q", edit.ToolName, "Write")
	}
	if edit.LinesAdded < 1 {
		t.Errorf("edit.LinesAdded = %d, want > 0", edit.LinesAdded)
	}
	if edit.LinesRemoved != 0 {
		t.Errorf("edit.LinesRemoved = %d, want 0", edit.LinesRemoved)
	}
}

// TestPostFileEdit_EditTool verifies that the post-file-edit hook correctly
// handles Edit tool input with old_string and new_string.
func TestPostFileEdit_EditTool(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-3"

	// Start a session turn
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Build the post-file-edit hook input for an Edit tool
	filePath := filepath.Join(env.RepoDir, "main.go")
	hookInput := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Edit",
		"tool_use_id": "toolu_edit1",
		"tool_input": map[string]interface{}{
			"file_path":  filePath,
			"old_string": "func main() {\n}",
			"new_string": "func main() {\n\tfmt.Println(\"hello\")\n}",
		},
	}
	inputJSON, err := json.Marshal(hookInput)
	if err != nil {
		t.Fatalf("failed to marshal hook input: %v", err)
	}

	// Invoke the post-file-edit hook
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
		t.Fatalf("post-file-edit hook failed: %v", err)
	}

	// Read the JSONL edit log
	stateStore := session.NewStateStoreWithDir(
		filepath.Join(env.RepoDir, ".git", session.SessionStateDirName),
	)
	edits, err := stateStore.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("failed to read file edits: %v", err)
	}
	if len(edits) != 1 {
		t.Fatalf("edits count = %d, want 1", len(edits))
	}

	edit := edits[0]
	if edit.FilePath != "main.go" {
		t.Errorf("edit.FilePath = %q, want %q", edit.FilePath, "main.go")
	}
	if edit.Action != agent.FileEditActionEdit {
		t.Errorf("edit.Action = %q, want %q", edit.Action, agent.FileEditActionEdit)
	}
	if edit.ToolName != "Edit" {
		t.Errorf("edit.ToolName = %q, want %q", edit.ToolName, "Edit")
	}
	// old_string has 2 lines, new_string has 3 lines
	if edit.LinesAdded != 3 {
		t.Errorf("edit.LinesAdded = %d, want 3", edit.LinesAdded)
	}
	if edit.LinesRemoved != 2 {
		t.Errorf("edit.LinesRemoved = %d, want 2", edit.LinesRemoved)
	}

	// Verify FilesTouched also updated
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state is nil")
	}
	if len(state.FilesTouched) != 1 {
		t.Fatalf("FilesTouched = %v, want 1 entry", state.FilesTouched)
	}
	if state.FilesTouched[0] != "main.go" {
		t.Errorf("FilesTouched[0] = %q, want %q", state.FilesTouched[0], "main.go")
	}
}

// TestPostFileEdit_Idempotent verifies that editing the same file twice only adds
// it once to FilesTouched, but records both edits in the JSONL log.
func TestPostFileEdit_Idempotent(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-4"

	// Start a session turn
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	filePath := filepath.Join(env.RepoDir, "repeated.go")
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)

	// First edit
	hookInput1 := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Write",
		"tool_use_id": "toolu_write_a",
		"tool_input": map[string]interface{}{
			"file_path": filePath,
			"content":   "package main\n",
		},
	}
	inputJSON1, _ := json.Marshal(hookInput1)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON1); err != nil {
		t.Fatalf("first post-file-edit hook failed: %v", err)
	}

	// Second edit to the same file
	hookInput2 := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Edit",
		"tool_use_id": "toolu_edit_b",
		"tool_input": map[string]interface{}{
			"file_path":  filePath,
			"old_string": "package main\n",
			"new_string": "package main\n\nfunc init() {}\n",
		},
	}
	inputJSON2, _ := json.Marshal(hookInput2)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON2); err != nil {
		t.Fatalf("second post-file-edit hook failed: %v", err)
	}

	// Verify FilesTouched has the path only ONCE
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state is nil")
	}
	if len(state.FilesTouched) != 1 {
		t.Fatalf("FilesTouched = %v, want exactly 1 entry (deduplication)", state.FilesTouched)
	}
	if state.FilesTouched[0] != "repeated.go" {
		t.Errorf("FilesTouched[0] = %q, want %q", state.FilesTouched[0], "repeated.go")
	}

	// Verify the JSONL log has TWO entries (all edits recorded)
	stateStore := session.NewStateStoreWithDir(
		filepath.Join(env.RepoDir, ".git", session.SessionStateDirName),
	)
	edits, err := stateStore.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("failed to read file edits: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("edits count = %d, want 2", len(edits))
	}

	// First edit should be Write
	if edits[0].Action != agent.FileEditActionWrite {
		t.Errorf("edits[0].Action = %q, want %q", edits[0].Action, agent.FileEditActionWrite)
	}
	// Second edit should be Edit
	if edits[1].Action != agent.FileEditActionEdit {
		t.Errorf("edits[1].Action = %q, want %q", edits[1].Action, agent.FileEditActionEdit)
	}
}

// TestPostFileEdit_MultipleFiles verifies that editing different files adds
// each one to FilesTouched.
func TestPostFileEdit_MultipleFiles(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-5"

	// Start a session turn
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)

	files := []string{"file_a.go", "file_b.go", "file_c.go"}
	for i, name := range files {
		hookInput := map[string]interface{}{
			"session_id":  sessionID,
			"tool_name":   "Write",
			"tool_use_id": "toolu_write_" + name,
			"tool_input": map[string]interface{}{
				"file_path": filepath.Join(env.RepoDir, name),
				"content":   "package main\n",
			},
		}
		inputJSON, _ := json.Marshal(hookInput)
		if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
			t.Fatalf("post-file-edit hook for file %d failed: %v", i, err)
		}
	}

	// Verify all three files are in FilesTouched
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state is nil")
	}
	if len(state.FilesTouched) != 3 {
		t.Fatalf("FilesTouched = %v, want 3 entries", state.FilesTouched)
	}

	// Verify the JSONL log has three entries
	stateStore := session.NewStateStoreWithDir(
		filepath.Join(env.RepoDir, ".git", session.SessionStateDirName),
	)
	edits, err := stateStore.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("failed to read file edits: %v", err)
	}
	if len(edits) != 3 {
		t.Fatalf("edits count = %d, want 3", len(edits))
	}
}

// TestPostFileEdit_NoSession verifies that the post-file-edit hook does not
// error when no session state exists (graceful no-op).
func TestPostFileEdit_NoSession(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// Do NOT start a session -- go directly to post-file-edit
	hookInput := map[string]interface{}{
		"session_id":  "nonexistent-session",
		"tool_name":   "Write",
		"tool_use_id": "toolu_write_x",
		"tool_input": map[string]interface{}{
			"file_path": filepath.Join(env.RepoDir, "orphan.go"),
			"content":   "package main\n",
		},
	}
	inputJSON, _ := json.Marshal(hookInput)

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	// The hook should not fail -- it logs a warning and returns nil
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
		t.Fatalf("post-file-edit hook should not error without session state, got: %v", err)
	}

	// Verify no session state was created
	stateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", "nonexistent-session.json")
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Error("session state file should not be created for a nonexistent session")
	}

	// Verify no orphan edits file was created
	editsFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", "nonexistent-session-edits.jsonl")
	if _, err := os.Stat(editsFile); !os.IsNotExist(err) {
		t.Error("edits file should not be created when no session state exists")
	}
}

// TestPostFileEdit_PathOutsideRepo verifies that file paths outside the repo
// are silently ignored by the post-file-edit hook.
func TestPostFileEdit_PathOutsideRepo(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sessionID := "test-session-edit-6"

	// Start a session turn
	if err := env.SimulateUserPromptSubmit(sessionID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Use a path outside the repo
	hookInput := map[string]interface{}{
		"session_id":  sessionID,
		"tool_name":   "Write",
		"tool_use_id": "toolu_write_outside",
		"tool_input": map[string]interface{}{
			"file_path": "/tmp/outside-repo/file.go",
			"content":   "package main\n",
		},
	}
	inputJSON, _ := json.Marshal(hookInput)

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	if err := runner.runHookInRepoDir("post-file-edit", inputJSON); err != nil {
		t.Fatalf("post-file-edit hook should not error for outside-repo path, got: %v", err)
	}

	// Verify FilesTouched is empty (outside-repo path was skipped)
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state is nil")
	}
	if len(state.FilesTouched) != 0 {
		t.Errorf("FilesTouched = %v, want empty (outside-repo path should be skipped)", state.FilesTouched)
	}
}
