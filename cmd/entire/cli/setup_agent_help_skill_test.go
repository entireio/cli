package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The agent-help skill scaffolds a marker-managed, near-immutable file that
// points the agent at `entire agent-help` (and carries the no-ask repo rule),
// for each supported agent.
func TestScaffoldAgentHelpSkill_CreatesManagedFiles(t *testing.T) {
	testCases := []struct {
		name    string
		scaffN  func() (managedScaffoldResult, error)
		relPath string
	}{
		{
			name: "claude",
			scaffN: func() (managedScaffoldResult, error) {
				return scaffoldAgentHelpSkill(context.Background(), claudecode.NewClaudeCodeAgent())
			},
			relPath: filepath.Join(".claude", "skills", "entire", "SKILL.md"),
		},
		{
			name: "codex",
			scaffN: func() (managedScaffoldResult, error) {
				return scaffoldAgentHelpSkill(context.Background(), codex.NewCodexAgent())
			},
			relPath: filepath.Join(".codex", "agents", "entire.toml"),
		},
		{
			name: "gemini",
			scaffN: func() (managedScaffoldResult, error) {
				return scaffoldAgentHelpSkill(context.Background(), geminicli.NewGeminiCLIAgent())
			},
			relPath: filepath.Join(".gemini", "agents", "entire.md"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := setupTestDir(t)

			result, err := tc.scaffN()
			if err != nil {
				t.Fatalf("scaffoldAgentHelpSkill() error = %v", err)
			}
			if result.Status != managedScaffoldCreated {
				t.Fatalf("status = %q, want %q", result.Status, managedScaffoldCreated)
			}
			if result.RelPath != tc.relPath {
				t.Fatalf("relPath = %q, want %q", result.RelPath, tc.relPath)
			}

			data, err := os.ReadFile(filepath.Join(tmpDir, tc.relPath))
			if err != nil {
				t.Fatalf("read scaffolded file: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, entireManagedAgentHelpSkillMarker) {
				t.Error("scaffolded file should contain the Entire-managed marker")
			}
			if !strings.Contains(content, agentHelpCommand) {
				t.Errorf("scaffolded file should point at `entire agent-help`:\n%s", content)
			}
			if !strings.Contains(strings.ToLower(content), "never ask") {
				t.Errorf("scaffolded file should carry the no-ask repo rule:\n%s", content)
			}

			// Idempotent: a second scaffold of identical content reports unchanged.
			again, err := tc.scaffN()
			if err != nil {
				t.Fatalf("second scaffoldAgentHelpSkill() error = %v", err)
			}
			if again.Status != managedScaffoldUnchanged {
				t.Errorf("second scaffold status = %q, want %q (no churn)", again.Status, managedScaffoldUnchanged)
			}
		})
	}
}

// An unmanaged pre-existing file is never overwritten.
func TestScaffoldAgentHelpSkill_SkipsUnmanagedConflict(t *testing.T) {
	tmpDir := setupTestDir(t)
	rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")
	target := filepath.Join(tmpDir, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("hand-written, not entire-managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := scaffoldAgentHelpSkill(context.Background(), claudecode.NewClaudeCodeAgent())
	if err != nil {
		t.Fatalf("scaffoldAgentHelpSkill() error = %v", err)
	}
	if result.Status != managedScaffoldSkippedConflict {
		t.Errorf("status = %q, want %q", result.Status, managedScaffoldSkippedConflict)
	}
}

// A pre-existing Entire-managed agent-help file with stale content is rewritten
// to the current template (Updated), not left as-is or treated as a conflict.
func TestScaffoldAgentHelpSkill_UpdatesManagedFile(t *testing.T) {
	tmpDir := setupTestDir(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := agentHelpSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("agentHelpSkillTemplate() unexpectedly unsupported for claude")
	}

	targetPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	stale := "<!-- " + entireManagedAgentHelpSkillMarker + " -->\noutdated body\n"
	if err := os.WriteFile(targetPath, []byte(stale), 0o600); err != nil {
		t.Fatalf("failed to write stale managed content: %v", err)
	}

	result, err := scaffoldAgentHelpSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldAgentHelpSkill() error = %v", err)
	}
	if result.Status != managedScaffoldUpdated {
		t.Fatalf("status = %q, want %q", result.Status, managedScaffoldUpdated)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read updated content: %v", err)
	}
	if !strings.Contains(string(data), agentHelpCommand) {
		t.Error("updated managed file should contain the current template")
	}
	if strings.Contains(string(data), "outdated body") {
		t.Error("stale content should have been overwritten")
	}
}

// The agent-help skill is opt-in: a default enable installs nothing; only
// --agent-help-skill (EnableOptions.AgentHelpSkill) scaffolds it.
func TestSetupAgentHooksNonInteractive_AgentHelpSkillOptInOnly(t *testing.T) {
	tmpDir := setupTestDir(t)
	testutil.InitRepo(t, tmpDir)
	ag := claudecode.NewClaudeCodeAgent()
	skillPath := filepath.Join(tmpDir, ".claude", "skills", "entire", "SKILL.md")

	var out bytes.Buffer
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(default) error = %v", err)
	}
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("default setup must not install the agent-help skill, stat err = %v", err)
	}

	out.Reset()
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{AgentHelpSkill: true}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(agent-help skill) error = %v", err)
	}
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("opt-in setup should install the agent-help skill: %v", err)
	}
	if !strings.Contains(out.String(), "Installed Claude Code agent-help skill") {
		t.Fatalf("output should mention the installed agent-help skill, got: %s", out.String())
	}
}

// --agent-help-skill with no resolvable agent in non-interactive mode errors
// with actionable guidance.
func TestManageAgentsNonInteractive_AgentHelpSkillWithoutAgentsShowsGuidance(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var out bytes.Buffer
	err := runManageAgents(context.Background(), &out, EnableOptions{AgentHelpSkill: true}, nil)
	if err == nil {
		t.Fatal("expected error when --agent-help-skill cannot choose an agent non-interactively")
	}
	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}
	for _, want := range []string{
		"Cannot install the agent-help skill in non-interactive mode because no agents are enabled.",
		"entire enable --agent <name> --agent-help-skill",
		"entire agent add <name> --agent-help-skill",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q, got: %s", want, out.String())
		}
	}
}

func TestManageAgentsNonInteractive_BothSkillFlagsWithoutAgentsShowsBothGuidance(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var out bytes.Buffer
	err := runManageAgents(context.Background(), &out, EnableOptions{SearchSkill: true, AgentHelpSkill: true}, nil)
	if err == nil {
		t.Fatal("expected error when skill install cannot choose an agent non-interactively")
	}
	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}
	output := out.String()
	for _, want := range []string{
		"search skill",
		"agent-help skill",
		"entire enable --agent <name> --search-skill",
		"entire enable --agent <name> --agent-help-skill",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q, got: %s", want, output)
		}
	}
}

// The multi-agent dispatcher dedups repeated names and reports (without erroring)
// agents that have no agent-help template.
func TestSetupOptionalAgentHelpSkillForNames_DedupsAndSkipsUnsupported(t *testing.T) {
	tmpDir := setupTestDir(t)
	testutil.InitRepo(t, tmpDir)

	var out bytes.Buffer
	err := setupOptionalAgentHelpSkillForNames(context.Background(), &out,
		[]string{"claude-code", "claude-code", "cursor"}, EnableOptions{AgentHelpSkill: true})
	if err != nil {
		t.Fatalf("setupOptionalAgentHelpSkillForNames error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".claude", "skills", "entire", "SKILL.md")); err != nil {
		t.Fatalf("claude-code skill should be installed: %v", err)
	}
	if !strings.Contains(out.String(), "not supported") {
		t.Fatalf("cursor (no template) should be reported unsupported, got: %s", out.String())
	}
	// A no-channel agent must be pointed at the passive pull path, not left at a
	// dead-end "not supported" line.
	if !strings.Contains(out.String(), agentHelpCommand) {
		t.Fatalf("unsupported agent should be pointed at `entire agent-help`, got: %s", out.String())
	}
	if !strings.Contains(strings.ToLower(out.String()), "passive") {
		t.Fatalf("unsupported agent note should mention passive discovery, got: %s", out.String())
	}
}
