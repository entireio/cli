//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/sessionid"
)

// TestSubdirectory_EntireDirCreatedAtRepoRoot verifies that when the CLI is run
// from a subdirectory within a git repo, the .entire directory and its contents
// are created at the repository root, not in the subdirectory.
//
// This is a regression test for a bug where running Claude from frontend/ would
// create frontend/.entire/ instead of using the repo root's .entire/.
func TestSubdirectory_EntireDirCreatedAtRepoRoot(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a subdirectory to simulate running from frontend/
		subdirName := "frontend"
		subdirPath := filepath.Join(env.RepoDir, subdirName)
		if err := os.MkdirAll(subdirPath, 0o755); err != nil {
			t.Fatalf("failed to create subdirectory: %v", err)
		}

		// Run the user-prompt-submit hook FROM the subdirectory
		sessionID := "test-subdir-session"
		input := map[string]string{
			"session_id":      sessionID,
			"transcript_path": "",
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("failed to marshal input: %v", err)
		}

		cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "user-prompt-submit")
		cmd.Dir = subdirPath // Run from subdirectory!
		cmd.Stdin = bytes.NewReader(inputJSON)
		cmd.Env = append(os.Environ(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		)

		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook failed: %v\nOutput: %s", err, output)
		}

		// Verify .entire/tmp was NOT created in the subdirectory
		subdirEntire := filepath.Join(subdirPath, ".entire")
		if _, err := os.Stat(subdirEntire); !os.IsNotExist(err) {
			t.Errorf(".entire directory should NOT exist in subdirectory %s, but it does", subdirName)
		}

		// Verify .entire/tmp WAS created at the repo root
		rootEntireTmp := filepath.Join(env.RepoDir, ".entire", "tmp")
		if _, err := os.Stat(rootEntireTmp); os.IsNotExist(err) {
			t.Errorf(".entire/tmp should exist at repo root, but it doesn't")
		}

		// Verify the pre-prompt state file was created at repo root
		entireSessionID := sessionid.EntireSessionID(sessionID)
		stateFile := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-prompt-"+entireSessionID+".json")
		if _, err := os.Stat(stateFile); os.IsNotExist(err) {
			t.Errorf("pre-prompt state file should exist at %s, but it doesn't", stateFile)
		}

		// Also verify the state file was NOT created in the subdirectory
		subdirStateFile := filepath.Join(subdirPath, ".entire", "tmp", "pre-prompt-"+entireSessionID+".json")
		if _, err := os.Stat(subdirStateFile); !os.IsNotExist(err) {
			t.Errorf("pre-prompt state file should NOT exist in subdirectory at %s", subdirStateFile)
		}
	})
}

// TestSubdirectory_SaveChangesFromSubdir verifies that SaveChanges (stop hook)
// works correctly when run from a subdirectory.
func TestSubdirectory_SaveChangesFromSubdir(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a subdirectory
		subdirName := "src"
		subdirPath := filepath.Join(env.RepoDir, subdirName)
		if err := os.MkdirAll(subdirPath, 0o755); err != nil {
			t.Fatalf("failed to create subdirectory: %v", err)
		}

		// Create a session and files
		session := env.NewSession()

		// Create a file in the subdirectory (as if Claude wrote it there)
		env.WriteFile(filepath.Join(subdirName, "app.js"), "console.log('hello');")

		// Create transcript
		session.CreateTranscript("Create app.js", []FileChange{
			{Path: filepath.Join(subdirName, "app.js"), Content: "console.log('hello');"},
		})

		// Simulate user-prompt-submit FROM subdirectory
		input := map[string]string{
			"session_id":      session.ID,
			"transcript_path": "",
		}
		inputJSON, _ := json.Marshal(input)

		cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "user-prompt-submit")
		cmd.Dir = subdirPath // Run from subdirectory
		cmd.Stdin = bytes.NewReader(inputJSON)
		cmd.Env = append(os.Environ(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("user-prompt-submit hook failed: %v\nOutput: %s", err, output)
		}

		// Simulate stop FROM subdirectory
		stopInput := map[string]string{
			"session_id":      session.ID,
			"transcript_path": session.TranscriptPath,
		}
		stopInputJSON, _ := json.Marshal(stopInput)

		stopCmd := exec.Command(getTestBinary(), "hooks", "claude-code", "stop")
		stopCmd.Dir = subdirPath // Run from subdirectory
		stopCmd.Stdin = bytes.NewReader(stopInputJSON)
		stopCmd.Env = append(os.Environ(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		)
		if output, err := stopCmd.CombinedOutput(); err != nil {
			t.Fatalf("stop hook failed: %v\nOutput: %s", err, output)
		}

		// Verify .entire was NOT created in subdirectory
		subdirEntire := filepath.Join(subdirPath, ".entire")
		if _, err := os.Stat(subdirEntire); !os.IsNotExist(err) {
			t.Errorf(".entire directory should NOT exist in subdirectory %s", subdirName)
		}

		// Verify we can get rewind points (this uses ListSessions/GetRewindPoints)
		points := env.GetRewindPoints()
		// Shadow strategy should have at least one rewind point
		// Other strategies create commits
		if strategyName == "manual-commit" {
			if len(points) == 0 {
				t.Error("expected at least one rewind point after save")
			}
		}
	})
}
