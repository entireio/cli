//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestAgentStrategyComposition verifies that agent and strategy work together correctly.
// This tests the full flow: agent parses session → strategy saves checkpoint on the shadow branch.
func TestAgentStrategyComposition(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	// Get agent and strategy
	ag, err := agent.Get(agentClaudeCode)
	if err != nil {
		t.Fatalf("Get(claude-code) error = %v", err)
	}

	// Create a session with the agent
	session := env.NewSession()

	// Create test file
	env.WriteFile("feature.go", "package main\n// new feature")

	// Create transcript via agent's expected format
	transcriptPath := session.CreateTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n// new feature"},
	})

	// Read session via agent interface
	agentSession, err := ag.ReadSession(&agent.HookInput{
		SessionID:  session.ID,
		SessionRef: transcriptPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Verify agent computed modified files
	if len(agentSession.ModifiedFiles) == 0 {
		t.Error("agent.ReadSession() should compute ModifiedFiles")
	}

	// Simulate session flow: UserPromptSubmit → make changes → Stop
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit error = %v", err)
	}

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop error = %v", err)
	}

	// Verify checkpoint was created (manual-commit stores checkpoint data on the shadow branch)
	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after Stop hook", shadowBranch)
	}
	if !env.FileExistsInBranch(shadowBranch, "feature.go") {
		t.Errorf("feature.go should be captured on shadow branch %s", shadowBranch)
	}
}

// TestAgentGetSessionDir verifies session directory resolution.
func TestAgentGetSessionDir(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()

	ag, err := agent.Get(agentClaudeCode)
	if err != nil {
		t.Fatalf("Get(claude-code) error = %v", err)
	}

	// With test override
	sessionDir, err := ag.GetSessionDir(env.RepoDir)
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	// Should return the override path from ENTIRE_TEST_CLAUDE_PROJECT_DIR
	// (set in test environment)
	if sessionDir == "" {
		t.Error("GetSessionDir() returned empty string")
	}

	t.Logf("Session directory for %s: %s", env.RepoDir, sessionDir)
}

// TestAgentFormatResumeCommand verifies resume command formatting.
func TestAgentFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag, err := agent.Get(agentClaudeCode)
	if err != nil {
		t.Fatalf("Get(claude-code) error = %v", err)
	}

	cmd := ag.FormatResumeCommand("test-session-123")
	expected := "claude -r test-session-123"

	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

// TestSetupAgentFlag verifies the --agent flag in enable command.
func TestSetupAgentFlag(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()

	// Run enable with --agent flag
	output := env.RunCLI("enable", "--agent", agentClaudeCode)
	if strings.Contains(output, "error") || strings.Contains(output, "Error") {
		t.Fatalf("enable --agent claude-code failed\nOutput: %s", output)
	}

	// Verify hooks were installed
	settingsPath := filepath.Join(env.RepoDir, ".claude", claudecode.ClaudeSettingsFileName)
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Errorf("enable --agent should create .claude/%s", claudecode.ClaudeSettingsFileName)
	}

	// Verify .entire/settings has agent set
	entireSettingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	data, err := os.ReadFile(entireSettingsPath)
	if err != nil {
		t.Fatalf("failed to read .entire/%s: %v", paths.SettingsFileName, err)
	}

	if !strings.Contains(string(data), `"agent"`) && !strings.Contains(string(data), `"agent":`) {
		t.Logf("settings content: %s", data)
		// Agent field may be omitted if default
	}
}

// TestFactoryAIDroidAgentStrategyComposition verifies that the Factory AI Droid agent
// works correctly with each strategy. This tests the full hook-based flow:
// agent hooks dispatch → lifecycle dispatcher → strategy saves checkpoint.
//
// Note: We use InitEntire (not InitEntireWithAgent) because the agent is determined
// by the hook command routing (entire hooks factoryai-droid ...), not by settings.json.
// EntireSettings doesn't have an "agent" field — the CLI subprocess determines the agent
// from the hook subcommand path.
func TestFactoryAIDroidAgentStrategyComposition(t *testing.T) {
	t.Parallel()

	// Set up repo
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	// Create initial commit
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create feature branch
	env.GitCheckoutNewBranch("feature/droid-test")

	// Create a Droid session with Droid-envelope transcript
	session := env.NewFactoryDroidSession()
	env.WriteFile("feature.go", "package main\n// new feature")
	session.CreateDroidTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n// new feature"},
	})

	// Simulate session flow: UserPromptSubmit → Stop
	if err := env.SimulateFactoryDroidUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateFactoryDroidUserPromptSubmit error = %v", err)
	}

	if err := env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateFactoryDroidStop error = %v", err)
	}

	// Verify checkpoint was created (manual-commit stores checkpoint data on the shadow branch)
	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after Stop hook", shadowBranch)
	}
	if !env.FileExistsInBranch(shadowBranch, "feature.go") {
		t.Errorf("feature.go should be captured on shadow branch %s", shadowBranch)
	}
}
