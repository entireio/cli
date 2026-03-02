//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestPostFileEdit_UpdatesFilesTouched verifies that the post-file-edit hook
// updates FilesTouched in session state when a Write tool modifies a file.
func TestPostFileEdit_UpdatesFilesTouched(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)
	session := env.NewSession()

	// Initialize session with user-prompt-submit
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Verify initial state has no FilesTouched
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if state == nil {
		t.Fatal("Session state is nil after user-prompt-submit")
	}
	if len(state.FilesTouched) != 0 {
		t.Errorf("Initial FilesTouched should be empty, got %v", state.FilesTouched)
	}

	// Simulate post-file-edit hook for a Write tool call.
	// The hook input matches the SubagentCheckpointHookInput format that
	// parseFileEditHookInput reuses.
	hookInput := map[string]interface{}{
		"session_id":  session.ID,
		"tool_name":   "Write",
		"tool_use_id": "toolu_01ABC",
		"tool_input": map[string]string{
			"file_path": filepath.Join(env.RepoDir, "new_file.go"),
			"content":   "package main\n\nfunc main() {\n}\n",
		},
		"tool_response": "",
	}
	inputJSON, err := json.Marshal(hookInput)
	if err != nil {
		t.Fatalf("Failed to marshal hook input: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "post-file-edit")
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("post-file-edit hook failed: %v\nOutput: %s", err, output)
	}

	// Verify FilesTouched was updated
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState after hook: %v", err)
	}
	if state == nil {
		t.Fatal("Session state is nil after post-file-edit")
	}

	found := false
	for _, f := range state.FilesTouched {
		if f == "new_file.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("FilesTouched should contain 'new_file.go', got %v", state.FilesTouched)
	}
}

// TestPostFileEdit_CreatesEditLog verifies that the post-file-edit hook
// creates a JSONL edit log in .git/entire-sessions/.
func TestPostFileEdit_CreatesEditLog(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)
	session := env.NewSession()

	// Initialize session
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Simulate post-file-edit hook for an Edit tool call
	hookInput := map[string]interface{}{
		"session_id":  session.ID,
		"tool_name":   "Edit",
		"tool_use_id": "toolu_02DEF",
		"tool_input": map[string]string{
			"file_path":  filepath.Join(env.RepoDir, "existing.go"),
			"old_string": "line1\nline2\n",
			"new_string": "line1\nline2\nline3\n",
		},
		"tool_response": "",
	}
	inputJSON, err := json.Marshal(hookInput)
	if err != nil {
		t.Fatalf("Failed to marshal hook input: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "post-file-edit")
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("post-file-edit hook failed: %v\nOutput: %s", err, output)
	}

	// Verify JSONL edit log was created
	editsPath := filepath.Join(env.RepoDir, ".git", "entire-sessions", session.ID+"-edits.jsonl")
	data, err := os.ReadFile(editsPath)
	if err != nil {
		t.Fatalf("Failed to read edit log: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("Edit log is empty")
	}

	// Parse the JSONL line (find first newline to get the first record)
	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		idx = len(data)
	}
	var editRecord map[string]interface{}
	if err := json.Unmarshal(data[:idx], &editRecord); err != nil {
		t.Fatalf("Failed to parse edit log entry: %v", err)
	}

	if editRecord["file_path"] != "existing.go" {
		t.Errorf("file_path = %v, want 'existing.go'", editRecord["file_path"])
	}
	if editRecord["action"] != "edit" {
		t.Errorf("action = %v, want 'edit'", editRecord["action"])
	}
	if editRecord["tool_name"] != "Edit" {
		t.Errorf("tool_name = %v, want 'Edit'", editRecord["tool_name"])
	}
	// lines_added: "line1\nline2\nline3\n" = 3 lines
	if linesAdded, ok := editRecord["lines_added"].(float64); !ok || int(linesAdded) != 3 {
		t.Errorf("lines_added = %v, want 3", editRecord["lines_added"])
	}
	// lines_removed: "line1\nline2\n" = 2 lines
	if linesRemoved, ok := editRecord["lines_removed"].(float64); !ok || int(linesRemoved) != 2 {
		t.Errorf("lines_removed = %v, want 2", editRecord["lines_removed"])
	}
}

// TestPostFileEdit_Idempotent verifies that calling post-file-edit multiple
// times for the same file doesn't duplicate entries in FilesTouched.
func TestPostFileEdit_Idempotent(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)
	session := env.NewSession()

	// Initialize session
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Call post-file-edit twice for the same file
	for i := range 2 {
		hookInput := map[string]interface{}{
			"session_id":  session.ID,
			"tool_name":   "Write",
			"tool_use_id": "toolu_03GHI",
			"tool_input": map[string]string{
				"file_path": filepath.Join(env.RepoDir, "same_file.go"),
				"content":   "package main\n",
			},
			"tool_response": "",
		}
		inputJSON, err := json.Marshal(hookInput)
		if err != nil {
			t.Fatalf("Failed to marshal hook input: %v", err)
		}

		cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "post-file-edit")
		cmd.Dir = env.RepoDir
		cmd.Stdin = bytes.NewReader(inputJSON)
		cmd.Env = append(os.Environ(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("post-file-edit hook (call %d) failed: %v\nOutput: %s", i+1, err, output)
		}
	}

	// Verify FilesTouched has the file only once
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	count := 0
	for _, f := range state.FilesTouched {
		if f == "same_file.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("FilesTouched should contain 'same_file.go' exactly once, got %d (all: %v)", count, state.FilesTouched)
	}
}
