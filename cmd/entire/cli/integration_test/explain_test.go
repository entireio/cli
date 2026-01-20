//go:build integration

package integration

import (
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/strategy"
)

func TestExplain_NoCurrentSession(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Explain without any sessions should show branch view with 0 checkpoints
		// This is the expected behavior - branch view is always valid
		output := env.RunCLI("explain", "--no-pager")

		// Should show branch header
		if !strings.Contains(output, "Branch:") {
			t.Errorf("expected branch header in output, got: %s", output)
		}

		// Should show 0 checkpoints
		if !strings.Contains(output, "Checkpoints: 0") {
			t.Errorf("expected 'Checkpoints: 0' in output, got: %s", output)
		}
	})
}

func TestExplain_SessionNotFound(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Try to explain a non-existent session
		output, err := env.RunCLIWithError("explain", "--session", "nonexistent-session-id")

		if err == nil {
			t.Errorf("expected error for nonexistent session, got output: %s", output)
			return
		}

		if !strings.Contains(output, "session not found") {
			t.Errorf("expected 'session not found' error, got: %s", output)
		}
	})
}

func TestExplain_BothFlagsError(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Try to provide both --session and --commit flags
		output, err := env.RunCLIWithError("explain", "--session", "test-session", "--commit", "abc123")

		if err == nil {
			t.Errorf("expected error when both flags provided, got output: %s", output)
			return
		}

		if !strings.Contains(strings.ToLower(output), "cannot specify both") {
			t.Errorf("expected 'cannot specify both' error, got: %s", output)
		}
	})
}

// TestExplain_DefaultOutput tests the default branch-centric explain output.
// Uses auto-commit strategy which creates commits automatically, making
// checkpoint tracking more straightforward for testing.
func TestExplain_DefaultOutput(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository - use auto-commit strategy
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/explain-test")
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file change
	fileContent := "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", fileContent)

	session.CreateTranscript("Create main function", []FileChange{{Path: "main.go", Content: fileContent}})

	// SimulateStop with auto-commit creates the commit automatically
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Run explain (default view)
	output := env.RunCLI("explain", "--no-pager")

	// Verify branch header is present
	if !strings.Contains(output, "Branch: feature/explain-test") {
		t.Errorf("expected 'Branch: feature/explain-test' in output, got:\n%s", output)
	}

	// Verify checkpoint count is shown (1 committed checkpoint)
	if !strings.Contains(output, "Checkpoints: 1") {
		t.Errorf("expected 'Checkpoints: 1' in output, got:\n%s", output)
	}

	// Verify intent/outcome placeholders are shown
	if !strings.Contains(output, "Intent:") {
		t.Errorf("expected 'Intent:' label in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Outcome:") {
		t.Errorf("expected 'Outcome:' label in output, got:\n%s", output)
	}

	t.Log("Default explain output test passed!")
}

// TestExplain_VerboseOutput tests the --verbose flag which shows prompts and files.
// Uses auto-commit strategy for consistent checkpoint tracking.
func TestExplain_VerboseOutput(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository - use auto-commit strategy
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/explain-verbose")
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file changes
	fileAContent := "package main\n\nfunc A() {}\n"
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("a.go", fileAContent)
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create functions A and B")
	session.TranscriptBuilder.AddAssistantMessage("I'll create the functions for you.")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)
	session.TranscriptBuilder.AddAssistantMessage("Done!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// SimulateStop with auto-commit creates the commit automatically
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Run explain with --verbose
	output := env.RunCLI("explain", "--no-pager", "--verbose")

	// Verify branch header
	if !strings.Contains(output, "Branch: feature/explain-verbose") {
		t.Errorf("expected branch name in output, got:\n%s", output)
	}

	// Verbose should show session ID
	if !strings.Contains(output, "Session:") {
		t.Errorf("expected 'Session:' label in verbose output, got:\n%s", output)
	}

	// Verbose should show prompt (extracted from rewind point)
	if !strings.Contains(output, "Prompt:") {
		t.Errorf("expected 'Prompt:' label in verbose output, got:\n%s", output)
	}

	// Verbose should show the actual prompt content
	if !strings.Contains(output, "Create functions A and B") {
		t.Errorf("expected prompt content in verbose output, got:\n%s", output)
	}

	t.Log("Verbose explain output test passed!")
}

// TestExplain_FullOutput tests the --full flag which shows transcript content.
// Uses auto-commit strategy because --full reads from entire/sessions branch
// which requires committed checkpoint data.
func TestExplain_FullOutput(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository - use auto-commit strategy which commits automatically
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/explain-full")
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file change with distinctive transcript content
	fileContent := "package main\n\nfunc unique_function_name() {}\n"
	env.WriteFile("unique.go", fileContent)

	session.TranscriptBuilder.AddUserMessage("Create unique function")
	session.TranscriptBuilder.AddAssistantMessage("Creating unique_function_name for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "unique.go", fileContent)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done creating the unique function!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// SimulateStop with auto-commit creates the commit automatically
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Run explain with --full
	output := env.RunCLI("explain", "--no-pager", "--full")

	// Full should show transcript markers
	if !strings.Contains(output, "--- Transcript ---") {
		t.Errorf("expected '--- Transcript ---' marker in full output, got:\n%s", output)
	}

	// Full should show transcript end marker
	if !strings.Contains(output, "--- End Transcript ---") {
		t.Errorf("expected '--- End Transcript ---' marker in full output, got:\n%s", output)
	}

	t.Log("Full explain output test passed!")
}

// TestExplain_GenerateFlag tests the --generate flag which creates summaries.
// Uses auto-commit strategy because --generate reads/writes to entire/sessions branch
// which requires committed checkpoint data.
func TestExplain_GenerateFlag(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository - use auto-commit strategy which commits automatically
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/explain-generate")
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file change with a specific prompt that will become the Intent
	fileContent := "package main\n\nfunc AddUserAuth() {}\n"
	env.WriteFile("auth.go", fileContent)

	session.TranscriptBuilder.AddUserMessage("Implement user authentication")
	session.TranscriptBuilder.AddAssistantMessage("I'll implement user authentication for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "auth.go", fileContent)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("User authentication has been implemented successfully!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// SimulateStop with auto-commit creates the commit automatically
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// First run without --generate to verify placeholders
	outputBefore := env.RunCLI("explain", "--no-pager")
	if !strings.Contains(outputBefore, "(not generated)") {
		t.Errorf("expected '(not generated)' placeholder before --generate, got:\n%s", outputBefore)
	}

	// Run explain with --generate
	output := env.RunCLI("explain", "--no-pager", "--generate")

	// After --generate, intent should show the user prompt
	if !strings.Contains(output, "Implement user authentication") {
		t.Errorf("expected generated intent to contain user prompt, got:\n%s", output)
	}

	// Run again without --generate to verify persistence
	outputAfter := env.RunCLI("explain", "--no-pager")
	if !strings.Contains(outputAfter, "Implement user authentication") {
		t.Errorf("expected persisted intent to appear without --generate, got:\n%s", outputAfter)
	}

	t.Log("Generate flag test passed!")
}

// TestExplain_LimitOnMainBranch tests that checkpoints are limited on main branch.
func TestExplain_LimitOnMainBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository on main branch
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Initialize Entire on main branch (auto-commit strategy supports main)
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Create 12 checkpoints to exceed the default limit of 10
	for i := 1; i <= 12; i++ {
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed for iteration %d: %v", i, err)
		}

		fileContent := "package main\n\nfunc F" + string(rune('A'+i-1)) + "() {}\n"
		fileName := "file" + string(rune('a'+i-1)) + ".go"
		env.WriteFile(fileName, fileContent)

		session.TranscriptBuilder.AddUserMessage("Create function " + string(rune('A'+i-1)))
		session.TranscriptBuilder.AddAssistantMessage("Done!")
		toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", fileName, fileContent)
		session.TranscriptBuilder.AddToolResult(toolID)

		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("Failed to write transcript for iteration %d: %v", i, err)
		}

		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed for iteration %d: %v", i, err)
		}

		// Clear session state to avoid concurrent session warning
		if err := env.ClearSessionState(session.ID); err != nil {
			t.Fatalf("ClearSessionState failed for iteration %d: %v", i, err)
		}
	}

	// Run explain on main branch
	output := env.RunCLI("explain", "--no-pager")

	// Should show "showing last 10" since we have 12 checkpoints
	if !strings.Contains(output, "showing last 10") {
		t.Errorf("expected 'showing last 10' for main branch with 12 checkpoints, got:\n%s", output)
	}

	// Should show total count message
	if !strings.Contains(output, "12 total checkpoints") {
		t.Errorf("expected '12 total checkpoints' message, got:\n%s", output)
	}

	t.Log("Limit on main branch test passed!")
}

// TestExplain_LimitFlagOverride tests that --limit overrides the default limit.
// Uses auto-commit strategy for consistent checkpoint tracking.
func TestExplain_LimitFlagOverride(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository - use auto-commit strategy
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/explain-limit")
	env.InitEntire(strategy.StrategyNameAutoCommit)

	// Create 5 checkpoints
	for i := 1; i <= 5; i++ {
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed for iteration %d: %v", i, err)
		}

		fileContent := "package main\n\nfunc F" + string(rune('A'+i-1)) + "() {}\n"
		fileName := "file" + string(rune('a'+i-1)) + ".go"
		env.WriteFile(fileName, fileContent)

		session.TranscriptBuilder.AddUserMessage("Create function " + string(rune('A'+i-1)))
		session.TranscriptBuilder.AddAssistantMessage("Done!")
		toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", fileName, fileContent)
		session.TranscriptBuilder.AddToolResult(toolID)

		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("Failed to write transcript for iteration %d: %v", i, err)
		}

		// SimulateStop with auto-commit creates the commit automatically
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed for iteration %d: %v", i, err)
		}

		// Clear session state for next iteration
		if err := env.ClearSessionState(session.ID); err != nil {
			t.Fatalf("ClearSessionState failed for iteration %d: %v", i, err)
		}
	}

	// Run explain with --limit 2
	output := env.RunCLI("explain", "--no-pager", "--limit", "2")

	// On feature branch, --limit 2 should show exactly 2 checkpoints
	// The "showing last N" message only appears on default branch (main/master)
	if !strings.Contains(output, "Checkpoints: 2") {
		t.Errorf("expected 'Checkpoints: 2' with --limit 2, got:\n%s", output)
	}

	// Count checkpoint entries by looking for the date format pattern after checkpoint ID
	// e.g., "[abc123def456] 2026-01-20"
	dateMarkers := strings.Count(output, "] 20")
	if dateMarkers != 2 {
		t.Errorf("expected 2 checkpoint entries with --limit 2, got %d:\n%s", dateMarkers, output)
	}

	t.Log("Limit flag override test passed!")
}
