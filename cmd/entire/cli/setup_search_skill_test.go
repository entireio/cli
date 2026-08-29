package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestScaffoldSearchSkill_CreatesManagedFiles(t *testing.T) {
	testCases := []struct {
		name        string
		scaffoldFn  func() (managedScaffoldResult, error)
		relPath     string
		wantSnippet string
	}{
		{
			name: "claude",
			scaffoldFn: func() (managedScaffoldResult, error) {
				return scaffoldSearchSkill(context.Background(), claudecode.NewClaudeCodeAgent())
			},
			relPath:     filepath.Join(".claude", "agents", "entire-search.md"),
			wantSnippet: "tools: Bash",
		},
		{
			name: "codex",
			scaffoldFn: func() (managedScaffoldResult, error) {
				return scaffoldSearchSkill(context.Background(), codex.NewCodexAgent())
			},
			relPath:     filepath.Join(".codex", "agents", "entire-search.toml"),
			wantSnippet: `sandbox_mode = "read-only"`,
		},
		{
			name: "gemini",
			scaffoldFn: func() (managedScaffoldResult, error) {
				return scaffoldSearchSkill(context.Background(), geminicli.NewGeminiCLIAgent())
			},
			relPath:     filepath.Join(".gemini", "agents", "entire-search.md"),
			wantSnippet: "- run_shell_command",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := setupTestRepo(t)

			result, err := tc.scaffoldFn()
			if err != nil {
				t.Fatalf("scaffoldSearchSkill() error = %v", err)
			}
			if result.Status != managedScaffoldCreated {
				t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldCreated)
			}
			if result.RelPath != tc.relPath {
				t.Fatalf("scaffoldSearchSkill() relPath = %q, want %q", result.RelPath, tc.relPath)
			}

			data, err := os.ReadFile(filepath.Join(tmpDir, tc.relPath))
			if err != nil {
				t.Fatalf("failed to read scaffolded file: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, entireManagedSearchSkillMarker) {
				t.Fatal("scaffolded file should contain Entire-managed marker")
			}
			assertStrictJSONSearchInstructions(t, content)
			if !strings.Contains(content, tc.wantSnippet) {
				t.Fatalf("scaffolded file missing expected snippet %q", tc.wantSnippet)
			}
		})
	}
}

func TestScaffoldSearchSkill_IdempotentManagedFile(t *testing.T) {
	setupTestRepo(t)

	ag := claudecode.NewClaudeCodeAgent()
	if _, err := scaffoldSearchSkill(context.Background(), ag); err != nil {
		t.Fatalf("first scaffoldSearchSkill() error = %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("second scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldUnchanged {
		t.Fatalf("second scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldUnchanged)
	}
}

func TestScaffoldSearchSkill_UpdatesManagedFile(t *testing.T) {
	tmpDir := setupTestRepo(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := searchSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("searchSkillTemplate() unexpectedly unsupported for claude")
	}

	targetPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	oldContent := "<!-- " + legacyEntireManagedSearchSubagentMarker + " -->\noutdated\n"
	if err := os.WriteFile(targetPath, []byte(oldContent), 0o644); err != nil {
		t.Fatalf("failed to write old managed content: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldUpdated {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldUpdated)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read updated content: %v", err)
	}
	if !strings.Contains(string(data), "tools: Bash") {
		t.Fatal("updated managed file should contain the current template")
	}
	assertStrictJSONSearchInstructions(t, string(data))
}

func TestScaffoldSearchSkill_PreservesUserOwnedFile(t *testing.T) {
	tmpDir := setupTestRepo(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := searchSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("searchSkillTemplate() unexpectedly unsupported for claude")
	}

	targetPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	userContent := "user-owned search agent\n"
	if err := os.WriteFile(targetPath, []byte(userContent), 0o644); err != nil {
		t.Fatalf("failed to write user-owned file: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldSkippedConflict {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldSkippedConflict)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read preserved file: %v", err)
	}
	if string(data) != userContent {
		t.Fatal("user-owned file should not be overwritten")
	}
}

func TestSetupAgentHooksNonInteractive_SearchSkillOptInOnly(t *testing.T) {
	tmpDir := setupTestRepo(t)
	ag := claudecode.NewClaudeCodeAgent()

	var out bytes.Buffer
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(default) error = %v", err)
	}
	searchPath := filepath.Join(tmpDir, ".claude", "agents", "entire-search.md")
	if _, err := os.Stat(searchPath); !os.IsNotExist(err) {
		t.Fatalf("default setup should not install search skill, stat err = %v", err)
	}

	out.Reset()
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{SearchSkill: true}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(search skill) error = %v", err)
	}
	if _, err := os.Stat(searchPath); err != nil {
		t.Fatalf("opt-in setup should install search skill: %v", err)
	}
	if !strings.Contains(out.String(), "Installed Claude Code search skill") {
		t.Fatalf("output should mention installed search skill, got: %s", out.String())
	}
}

func TestManageAgentsNonInteractive_SearchSkillWithoutAgentsShowsInstallGuidance(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var out bytes.Buffer
	err := runManageAgents(context.Background(), &out, EnableOptions{SearchSkill: true}, nil)
	if err == nil {
		t.Fatal("expected error when --search-skill cannot choose an agent non-interactively")
	}
	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}

	output := out.String()
	for _, want := range []string{
		"Cannot install the search skill in non-interactive mode because no agents are enabled.",
		"entire enable --agent <name> --search-skill",
		"entire agent add <name> --search-skill",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q, got: %s", want, output)
		}
	}
}

func assertStrictJSONSearchInstructions(t *testing.T, content string) {
	t.Helper()

	if !strings.Contains(content, "entire search --json") {
		t.Fatal("scaffolded file should instruct use of `entire search --json`")
	}
	if !strings.Contains(content, "Never run `entire search` without `--json`; it opens an interactive TUI.") {
		t.Fatal("scaffolded file should explicitly forbid plain `entire search`")
	}
	if strings.Contains(content, "Your only history-search mechanism is the `entire search` command.") {
		t.Fatal("scaffolded file should not present plain `entire search` as the required command")
	}
	if !strings.Contains(content, "entire search --json --compact") {
		t.Fatal("scaffolded file should recommend `--json --compact` for scanning results")
	}
	if !strings.Contains(content, "entire checkpoint explain <id>") {
		t.Fatal("scaffolded file should point drill-down at `entire checkpoint explain <id>`")
	}
	if !strings.Contains(content, "entire checkpoint explain --session <id>") {
		t.Fatal("scaffolded file should bridge session hits via `explain --session`")
	}
	if !strings.Contains(content, "session hit on the current branch") {
		t.Fatal("scaffolded file should scope the session bridge to the current branch")
	}
	if !strings.Contains(content, "session hits are projections of the same checkpoints") {
		t.Fatal("scaffolded file should frame session hits as projections of checkpoints")
	}
	if !strings.Contains(content, "add `--full` to pull the checkpoint's entire session transcript") {
		t.Fatal("scaffolded file should escalate to `explain --full` for the session transcript")
	}
	if !strings.Contains(content, "summarize from the compact fields alone") {
		t.Fatal("scaffolded file should tell agents repo/pr and cross-repo hits aren't explainable")
	}
}

// TestSearchSkillTemplates_NameMatchesTelemetryProbe pins the scaffolded
// subagent name to strategy.EntireSearchSubagentName, the value the
// commit-condensed telemetry probe matches Task/Agent dispatches against.
// Without this pin, renaming the subagent in the templates compiles and passes
// every template test while silently zeroing used_search_source="subagent" —
// the probe's primary path.
func TestSearchSkillTemplates_NameMatchesTelemetryProbe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		template string
		// nameDecl is how the template's frontmatter/config declares the
		// subagent name for its agent's format.
		nameDecl string
	}{
		{"claude", claudeSearchSkillTemplate, "name: " + strategy.EntireSearchSubagentName + "\n"},
		{"gemini", geminiSearchSkillTemplate, "name: " + strategy.EntireSearchSubagentName + "\n"},
		{"codex", codexSearchSkillTemplate, "name = \"" + strategy.EntireSearchSubagentName + "\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(tc.template, tc.nameDecl) {
				t.Errorf("template does not declare the subagent name the telemetry probe matches: want %q", tc.nameDecl)
			}
		})
	}

	for _, agentName := range []types.AgentName{agent.AgentNameClaudeCode, agent.AgentNameCodex, agent.AgentNameGemini} {
		relPath, _, ok := searchSkillTemplate(agentName)
		if !ok {
			t.Fatalf("searchSkillTemplate(%s) unexpectedly unsupported", agentName)
		}
		base := filepath.Base(relPath)
		if got := strings.TrimSuffix(base, filepath.Ext(base)); got != strategy.EntireSearchSubagentName {
			t.Errorf("scaffold path for %s names %q, probe matches %q", agentName, got, strategy.EntireSearchSubagentName)
		}
	}
}
