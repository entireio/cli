package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// entireManagedAgentHelpSkillMarker tags the agent-help skill files Entire owns,
// so re-running setup can safely update them and never clobbers a hand-written
// file at the same path.
const entireManagedAgentHelpSkillMarker = "ENTIRE-MANAGED AGENT-HELP SKILL v1"

// setupOptionalAgentHelpSkill installs the stable "how to use entire" skill for
// ag when opts.AgentHelpSkill is set. The skill body is near-immutable — it only
// points the agent at `entire agent-help` — so re-running enable reports
// "unchanged" rather than churning a diff.
func setupOptionalAgentHelpSkill(ctx context.Context, w io.Writer, ag agent.Agent, opts EnableOptions) error {
	if !opts.AgentHelpSkill {
		return nil
	}
	result, err := scaffoldAgentHelpSkill(ctx, ag)
	if err != nil {
		return fmt.Errorf("failed to scaffold %s agent-help skill: %w", ag.Name(), err)
	}
	reportAgentHelpSkillScaffold(w, ag, result)
	return nil
}

func setupOptionalAgentHelpSkillForNames(ctx context.Context, w io.Writer, names []string, opts EnableOptions) error {
	return setupOptionalSkillForNames(ctx, w, names, opts.AgentHelpSkill, setupOptionalAgentHelpSkill, opts)
}

func scaffoldAgentHelpSkill(ctx context.Context, ag agent.Agent) (managedScaffoldResult, error) {
	relPath, content, ok := agentHelpSkillTemplate(ag.Name())
	if !ok {
		return managedScaffoldResult{Status: managedScaffoldUnsupported}, nil
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails in tests
		if err != nil {
			return managedScaffoldResult{}, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	targetPath := filepath.Join(repoRoot, relPath)
	return writeManagedScaffold(targetPath, relPath, content, isManagedAgentHelpSkill)
}

func isManagedAgentHelpSkill(data []byte) bool {
	return bytes.Contains(data, []byte(entireManagedAgentHelpSkillMarker))
}

func reportAgentHelpSkillScaffold(w io.Writer, ag agent.Agent, result managedScaffoldResult) {
	switch result.Status {
	case managedScaffoldCreated:
		fmt.Fprintf(w, "  ✓ Installed %s agent-help skill\n", ag.Type())
		fmt.Fprintf(w, "    %s\n", result.RelPath)
	case managedScaffoldUpdated:
		fmt.Fprintf(w, "  ✓ Updated %s agent-help skill\n", ag.Type())
		fmt.Fprintf(w, "    %s\n", result.RelPath)
	case managedScaffoldSkippedConflict:
		fmt.Fprintf(w, "  Skipped %s agent-help skill (unmanaged file exists)\n", ag.Type())
		fmt.Fprintf(w, "    %s\n", result.RelPath)
	case managedScaffoldUnsupported:
		fmt.Fprintf(w, "  Agent-help skill is not supported for %s\n", ag.Type())
		fmt.Fprintf(w, "    %s discovers entire via `%s` (passive — no skill file needed)\n", ag.Type(), agentHelpCommand)
	case managedScaffoldUnchanged:
		fmt.Fprintf(w, "  Agent-help skill already installed for %s\n", ag.Type())
		fmt.Fprintf(w, "    %s\n", result.RelPath)
	}
}

func agentHelpSkillTemplate(agentName types.AgentName) (string, []byte, bool) {
	switch agentName {
	case agent.AgentNameClaudeCode:
		return filepath.Join(".claude", "skills", "entire", "SKILL.md"), []byte(strings.TrimSpace(claudeAgentHelpSkillTemplate) + "\n"), true
	case agent.AgentNameCodex:
		return filepath.Join(".codex", "agents", "entire.toml"), []byte(strings.TrimSpace(codexAgentHelpSkillTemplate) + "\n"), true
	case agent.AgentNameGemini:
		return filepath.Join(".gemini", "agents", "entire.md"), []byte(strings.TrimSpace(geminiAgentHelpSkillTemplate) + "\n"), true
	default:
		return "", nil, false
	}
}

// agentHelpSkillBody is the shared, format-agnostic instruction body for the
// agent-help skill. It is byte-identical across claude/gemini/codex (only the
// surrounding frontmatter differs), so it lives in one place to stay in sync.
// The drill-down example uses checkpoint (always advertised) rather than a
// feature-gated command, matching renderAgentHelpTop's example selection so the
// static skill never points the agent at a command it can't use.
const agentHelpSkillBody = `Entire's CLI is the source of truth for its own usage. Do not guess flags or subcommands.

Run ` + "`entire agent-help`" + ` for a map of when to use entire and which subcommand to use,
then ` + "`entire agent-help <command>`" + ` (e.g. ` + "`entire agent-help checkpoint`" + `) for that command's
exact, currently-installed flags.

You are already inside the repo — entire auto-detects it from the git origin remote.
Never ask the user for the repo name.`

const claudeAgentHelpSkillTemplate = `
---
name: entire
description: How to use the Entire CLI (checkpoints, search, sessions, and more). Use whenever a task involves entire, checkpoints, or the ` + "`entire`" + ` command.
---

<!-- ` + entireManagedAgentHelpSkillMarker + ` -->

` + agentHelpSkillBody + `
`

const geminiAgentHelpSkillTemplate = `
---
name: entire
description: How to use the Entire CLI (checkpoints, search, sessions, and more). Use whenever a task involves entire, checkpoints, or the ` + "`entire`" + ` command.
kind: local
tools:
  - run_shell_command
---

<!-- ` + entireManagedAgentHelpSkillMarker + ` -->

` + agentHelpSkillBody + `
`

const codexAgentHelpSkillTemplate = `
# ` + entireManagedAgentHelpSkillMarker + `
name = "entire"
description = "How to use the Entire CLI (checkpoints, search, sessions, and more). Use whenever a task involves entire, checkpoints, or the ` + "`entire`" + ` command."
developer_instructions = """
` + agentHelpSkillBody + `
"""
`
