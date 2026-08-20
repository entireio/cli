//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShadow_GitignoredFilesExcludedFromSessionState tests that files matching
// .gitignore patterns are NOT included in UntrackedFilesAtStart, preventing
// bloated session state from large ignored directories like node_modules/.
func TestShadow_GitignoredFilesExcludedFromSessionState(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// Create initial commit with a .gitignore
	env.WriteFile(".gitignore", "node_modules/\n*.log\nbuild/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore", "README.md")
	env.GitCommit("Initial commit with .gitignore")

	env.GitCheckoutNewBranch("feature/gitignore-test")

	// Create gitignored files (simulating node_modules and build artifacts)
	env.WriteFile("node_modules/express/index.js", "module.exports = {}")
	env.WriteFile("node_modules/express/package.json", `{"name": "express"}`)
	env.WriteFile("node_modules/lodash/index.js", "module.exports = {}")
	env.WriteFile("build/app.js", "compiled output")
	env.WriteFile("debug.log", "some log output")

	// Create legitimate untracked files (NOT gitignored)
	env.WriteFile("config.local.json", `{"key": "value"}`)
	env.WriteFile("notes.txt", "my notes")

	// Initialize Entire and start session
	env.InitEntire()

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a checkpoint so session state is persisted
	env.WriteFile("app.go", "package main")
	session.CreateTranscript(
		"Create app",
		[]FileChange{{Path: "app.go", Content: "package main"}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Read session state and check UntrackedFilesAtStart
	sessionStateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	stateFiles, err := os.ReadDir(sessionStateDir)
	if err != nil {
		t.Fatalf("Failed to read session state dir: %v", err)
	}
	if len(stateFiles) == 0 {
		t.Fatal("Expected session state file")
	}

	stateFile := filepath.Join(sessionStateDir, stateFiles[0].Name())
	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("Failed to read session state file: %v", err)
	}
	stateContent := string(stateData)
	t.Logf("Session state:\n%s", stateContent)

	// Gitignored files should NOT be in session state
	if strings.Contains(stateContent, "node_modules") {
		t.Error("node_modules should NOT be in UntrackedFilesAtStart (gitignored)")
	}
	if strings.Contains(stateContent, "build/app.js") {
		t.Error("build/app.js should NOT be in UntrackedFilesAtStart (gitignored)")
	}
	if strings.Contains(stateContent, "debug.log") {
		t.Error("debug.log should NOT be in UntrackedFilesAtStart (gitignored via *.log)")
	}

	// Legitimate untracked files SHOULD be in session state
	if !strings.Contains(stateContent, "config.local.json") {
		t.Error("config.local.json should be in UntrackedFilesAtStart (not gitignored)")
	}
	if !strings.Contains(stateContent, "notes.txt") {
		t.Error("notes.txt should be in UntrackedFilesAtStart (not gitignored)")
	}
}
