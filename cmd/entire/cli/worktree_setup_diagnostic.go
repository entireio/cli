package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const claudeCodeAgentName = "claude-code"

type worktreeSetupIssue struct {
	ConfiguredWorktree        string
	MissingProjectSettings    bool
	MissingClaudeProjectHooks bool
	CurrentCaptureInactive    bool
}

func inspectWorktreeSetup(ctx context.Context) *worktreeSetupIssue {
	currentRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}

	project, local, err := settings.FilesPresent(ctx)
	if err != nil {
		return nil
	}
	hasAnySettings := project || local
	if hasAnySettings {
		currentSettings, loadErr := LoadEntireSettings(ctx)
		if loadErr != nil || !currentSettings.Enabled {
			return nil
		}
	}
	hasPortableProjectSettings := project && settings.ProjectSettingsEnabledForWorktreeRoot(currentRoot)

	hasClaudeProjectHooks, err := claudecode.AreProjectHooksInstalledInWorktree(ctx, currentRoot)
	if err != nil {
		return nil
	}
	if hasPortableProjectSettings && hasClaudeProjectHooks {
		return nil
	}

	for _, sibling := range siblingWorktrees(ctx, currentRoot) {
		if !settings.ProjectSettingsEnabledForWorktreeRoot(sibling) ||
			!settings.IsSetUpAndEnabledForWorktreeRoot(ctx, sibling) {
			continue
		}
		hasHooks, hookErr := claudecode.AreProjectHooksInstalledInWorktree(ctx, sibling)
		if hookErr == nil && hasHooks {
			return &worktreeSetupIssue{
				ConfiguredWorktree:        sibling,
				MissingProjectSettings:    !hasPortableProjectSettings,
				MissingClaudeProjectHooks: !hasClaudeProjectHooks,
				CurrentCaptureInactive:    !hasAnySettings,
			}
		}
	}
	return nil
}

func siblingWorktrees(ctx context.Context, currentRoot string) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain", "-z")
	cmd.Dir = currentRoot
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseSiblingWorktrees(output, normalizeWorktreePath(currentRoot))
}

func parseSiblingWorktrees(porcelain []byte, currentRoot string) []string {
	var worktrees []string
	var path string
	prunable := false
	bare := false
	for _, field := range bytes.Split(porcelain, []byte{0}) {
		line := string(field)
		switch {
		case line == "":
			if path != "" && !prunable && !bare && normalizeWorktreePath(path) != currentRoot {
				worktrees = append(worktrees, path)
			}
			path = ""
			prunable = false
			bare = false
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			prunable = true
		case line == "bare":
			bare = true
		}
	}
	return worktrees
}

func writeWorktreeSetupIssue(w io.Writer, issue *worktreeSetupIssue) {
	if issue == nil {
		return
	}
	fmt.Fprintln(w, "Claude Code worktree portability: INCOMPLETE")
	fmt.Fprintf(w, "  Entire capture is configured in another worktree: %s\n", issue.ConfiguredWorktree)
	fmt.Fprintf(w, "  Missing here: %s.\n", strings.Join(issue.missingLabels(), ", "))
	if issue.CurrentCaptureInactive {
		fmt.Fprintln(w, "  Without Entire settings, every Entire hook in this worktree is inactive.")
		fmt.Fprintln(w, "  Claude Code sessions started here will not create checkpoints, and commits will not receive Entire trailers.")
		fmt.Fprintln(w, "  Run `entire enable --agent claude-code` in this worktree.")
	} else {
		fmt.Fprintln(w, "  Fresh worktrees and clones may miss Entire capture until the portable project files are committed.")
		fmt.Fprintln(w, "  Local Entire settings may still keep Entire active in this worktree.")
		fmt.Fprintln(w, "  Claude user or local settings may still provide hooks for this worktree.")
		fmt.Fprintln(w, "  Run `entire enable --agent claude-code --project` to create any missing project setup.")
	}
	fmt.Fprintln(w, "  Commit `.entire/settings.json` and `.claude/settings.json` when the setup should apply across worktrees and clones.")
}

func (i *worktreeSetupIssue) missingLabels() []string {
	missing := make([]string, 0, 2)
	if i.MissingProjectSettings {
		missing = append(missing, "Entire project settings")
	}
	if i.MissingClaudeProjectHooks {
		missing = append(missing, "shared Claude Code hook config")
	}
	return missing
}

func (i *worktreeSetupIssue) missingJSONFields() []string {
	missing := make([]string, 0, 2)
	if i.MissingProjectSettings {
		missing = append(missing, "entire_project_settings")
	}
	if i.MissingClaudeProjectHooks {
		missing = append(missing, "claude_project_hook_config")
	}
	return missing
}
