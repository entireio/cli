//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
)

// TestSetupCodexHooks_AddsAllRequiredHooks is a smoke test verifying that
// `entire enable --agent codex` adds all required hooks and scaffolds the
// managed search subagent into the project.
func TestSetupCodexHooks_AddsAllRequiredHooks(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	output, err := env.RunCLIWithError("enable", "--agent", "codex")
	if err != nil {
		t.Fatalf("enable codex command failed: %v\nOutput: %s", err, output)
	}

	hooksPath := filepath.Join(env.RepoDir, ".codex", codex.HooksFileName)
	hooksData, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("failed to read generated Codex hooks.json: %v", err)
	}
	hooksContent := string(hooksData)
	if !strings.Contains(hooksContent, "entire hooks codex session-start") {
		t.Error("Codex SessionStart hook should exist")
	}
	if !strings.Contains(hooksContent, "entire hooks codex user-prompt-submit") {
		t.Error("Codex UserPromptSubmit hook should exist")
	}
	if !strings.Contains(hooksContent, "entire hooks codex stop") {
		t.Error("Codex Stop hook should exist")
	}

	searchAgentPath := filepath.Join(env.RepoDir, ".codex", "agents", "entire-search.toml")
	searchData, err := os.ReadFile(searchAgentPath)
	if err != nil {
		t.Fatalf("failed to read generated Codex search subagent: %v", err)
	}
	searchContent := string(searchData)
	if !strings.Contains(searchContent, "ENTIRE-MANAGED SEARCH SUBAGENT") {
		t.Error("Codex search subagent should be marked as Entire-managed")
	}
	if !strings.Contains(searchContent, "entire search --json") {
		t.Error("Codex search subagent should instruct use of `entire search --json`")
	}
}
