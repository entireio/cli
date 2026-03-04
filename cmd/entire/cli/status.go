package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var detailed bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show Entire status",
		Long:  "Show whether Entire is currently enabled or disabled",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), detailed)
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "Show detailed status for each settings file")

	return cmd
}

func runStatus(ctx context.Context, w io.Writer, detailed bool) error {
	// Check if we're in a git repository
	if _, repoErr := paths.WorktreeRoot(ctx); repoErr != nil {
		fmt.Fprintln(w, "✕ not a git repository")
		return nil //nolint:nilerr // Not being in a git repo is a valid status, not an error
	}

	// Get absolute paths for settings files
	settingsPath, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		settingsPath = EntireSettingsFile
	}
	localSettingsPath, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localSettingsPath = EntireSettingsLocalFile
	}

	// Check which settings files exist
	_, projectErr := os.Stat(settingsPath)
	if projectErr != nil && !errors.Is(projectErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot access project settings file: %w", projectErr)
	}
	_, localErr := os.Stat(localSettingsPath)
	if localErr != nil && !errors.Is(localErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot access local settings file: %w", localErr)
	}
	projectExists := projectErr == nil
	localExists := localErr == nil

	if !projectExists && !localExists {
		fmt.Fprintln(w, "○ not set up (run `entire enable` to get started)")
		return nil
	}

	sty := newStatusStyles(w)

	if detailed {
		return runStatusDetailed(ctx, w, sty, settingsPath, localSettingsPath, projectExists, localExists)
	}

	// Short output: just show the effective/merged state
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, formatSettingsStatusShort(ctx, s, sty))
	if s.Enabled {
		writeActiveSessions(ctx, w, sty)
	}

	return nil
}

// runStatusDetailed shows the effective status plus detailed status for each settings file.
func runStatusDetailed(ctx context.Context, w io.Writer, sty statusStyles, settingsPath, localSettingsPath string, projectExists, localExists bool) error {
	// First show the effective/merged status
	effectiveSettings, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, formatSettingsStatusShort(ctx, effectiveSettings, sty))
	fmt.Fprintln(w) // blank line

	// Show project settings if it exists
	if projectExists {
		projectSettings, err := settings.LoadFromFile(settingsPath)
		if err != nil {
			return fmt.Errorf("failed to load project settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Project", projectSettings, sty))
	}

	// Show local settings if it exists
	if localExists {
		localSettings, err := settings.LoadFromFile(localSettingsPath)
		if err != nil {
			return fmt.Errorf("failed to load local settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Local", localSettings, sty))
	}

	if effectiveSettings.Enabled {
		writeActiveSessions(ctx, w, sty)
	}

	return nil
}

// formatSettingsStatusShort formats a short settings status line.
// Output format: "● Enabled · manual-commit · branch main" or "○ Disabled"
func formatSettingsStatusShort(ctx context.Context, s *EntireSettings, sty statusStyles) string {
	displayName := strategy.StrategyNameManualCommit

	var b strings.Builder

	if s.Enabled {
		b.WriteString(sty.render(sty.green, "●"))
		b.WriteString(" ")
		b.WriteString(sty.render(sty.bold, "Enabled"))
	} else {
		b.WriteString(sty.render(sty.red, "○"))
		b.WriteString(" ")
		b.WriteString(sty.render(sty.bold, "Disabled"))
	}

	b.WriteString(sty.render(sty.dim, " · "))
	b.WriteString(displayName)

	// Resolve branch from repo root
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		if branch := resolveWorktreeBranch(ctx, repoRoot); branch != "" {
			b.WriteString(sty.render(sty.dim, " · "))
			b.WriteString("branch ")
			b.WriteString(sty.render(sty.cyan, branch))
		}
	}

	return b.String()
}

// formatSettingsStatus formats a settings status line with source prefix.
// Output format: "Project · enabled · manual-commit" or "Local · disabled"
func formatSettingsStatus(prefix string, s *EntireSettings, sty statusStyles) string {
	displayName := strategy.StrategyNameManualCommit

	var b strings.Builder
	b.WriteString(sty.render(sty.bold, prefix))
	b.WriteString(sty.render(sty.dim, " · "))

	if s.Enabled {
		b.WriteString("enabled")
	} else {
		b.WriteString("disabled")
	}

	b.WriteString(sty.render(sty.dim, " · "))
	b.WriteString(displayName)

	return b.String()
}

// timeAgo formats a time as a human-readable relative duration.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	}
}

// worktreeGroup groups sessions by worktree path for display.
type worktreeGroup struct {
	path     string
	branch   string
	sessions []*session.State
}

const (
	unknownPlaceholder  = "(unknown)"
	detachedHEADDisplay = "HEAD"
)

// writeActiveSessions writes active session information grouped by worktree.
func writeActiveSessions(ctx context.Context, w io.Writer, sty statusStyles) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return
	}

	states, err := store.List(ctx)
	if err != nil || len(states) == 0 {
		return
	}

	// Filter to active sessions only
	var active []*session.State
	for _, s := range states {
		if s.EndedAt == nil {
			active = append(active, s)
		}
	}
	if len(active) == 0 {
		return
	}

	// Group by worktree path
	groups := make(map[string]*worktreeGroup)
	for _, s := range active {
		wp := s.WorktreePath
		if wp == "" {
			wp = unknownPlaceholder
		}
		g, ok := groups[wp]
		if !ok {
			g = &worktreeGroup{path: wp}
			groups[wp] = g
		}
		g.sessions = append(g.sessions, s)
	}

	// Resolve branch names for each worktree (skip for unknown paths)
	for _, g := range groups {
		if g.path != unknownPlaceholder {
			g.branch = resolveWorktreeBranch(ctx, g.path)
		}
	}

	// Sort groups: alphabetical by path
	sortedGroups := make([]*worktreeGroup, 0, len(groups))
	for _, g := range groups {
		sortedGroups = append(sortedGroups, g)
	}
	sort.Slice(sortedGroups, func(i, j int) bool {
		return sortedGroups[i].path < sortedGroups[j].path
	})

	// Sort sessions within each group by StartedAt (newest first)
	for _, g := range sortedGroups {
		sort.Slice(g.sessions, func(i, j int) bool {
			return g.sessions[i].StartedAt.After(g.sessions[j].StartedAt)
		})
	}

	// Track aggregate totals
	var totalSessions int

	fmt.Fprintln(w)
	printedHeader := false
	for _, g := range sortedGroups {
		if !printedHeader {
			fmt.Fprintln(w, sty.sectionRule("Active Sessions", sty.width))
			fmt.Fprintln(w)
			printedHeader = true
		}

		for _, st := range g.sessions {
			totalSessions++

			agentLabel := string(st.AgentType)
			if agentLabel == "" {
				agentLabel = unknownPlaceholder
			}

			shortID := st.SessionID
			if len(shortID) > 7 {
				shortID = shortID[:7]
			}

			// Line 1: Agent (model) · shortID
			if st.ModelName != "" {
				fmt.Fprintf(w, "%s %s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "("+st.ModelName+")"),
					sty.render(sty.dim, "·"),
					shortID)
			} else {
				fmt.Fprintf(w, "%s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "·"),
					shortID)
			}

			// Line 2: > "first prompt" (chevron + quoted, truncated)
			if st.FirstPrompt != "" {
				prompt := stringutil.TruncateRunes(st.FirstPrompt, 60, "...")
				fmt.Fprintf(w, "%s \"%s\"\n", sty.render(sty.dim, ">"), prompt)
			}

			// Line 3: stats line — started Xd ago · active now · files N · tokens X.Xk
			var stats []string
			stats = append(stats, "started "+timeAgo(st.StartedAt))

			if st.LastInteractionTime != nil && st.LastInteractionTime.Sub(st.StartedAt) > time.Minute {
				stats = append(stats, activeTimeDisplay(st.LastInteractionTime))
			}

			stats = append(stats, "tokens "+formatTokenCount(totalTokens(st.TokenUsage)))

			statsLine := strings.Join(stats, sty.render(sty.dim, " · "))
			fmt.Fprintln(w, sty.render(sty.dim, statsLine))
			fmt.Fprintln(w)
		}
	}

	// Footer: horizontal rule + session count
	fmt.Fprintln(w, sty.horizontalRule(sty.width))
	var footer string
	if totalSessions == 1 {
		footer = "1 session"
	} else {
		footer = fmt.Sprintf("%d sessions", totalSessions)
	}
	fmt.Fprintln(w, sty.render(sty.dim, footer))
	fmt.Fprintln(w)
}

// resolveWorktreeBranch resolves the current branch for a worktree path
// by reading the HEAD ref directly from the filesystem
func resolveWorktreeBranch(ctx context.Context, worktreePath string) string {
	gitPath := filepath.Join(worktreePath, ".git")

	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}

	var headPath string
	if fi.IsDir() {
		// Regular repo: .git is a directory
		headPath = filepath.Join(gitPath, "HEAD")
	} else {
		// Worktree: .git is a file containing "gitdir: <path>"
		data, err := os.ReadFile(gitPath) //nolint:gosec // path derived from known worktree dir
		if err != nil {
			return ""
		}
		content := strings.TrimSpace(string(data))
		if !strings.HasPrefix(content, "gitdir: ") {
			return ""
		}
		gitdirPath := strings.TrimPrefix(content, "gitdir: ")
		if !filepath.IsAbs(gitdirPath) {
			gitdirPath = filepath.Join(worktreePath, gitdirPath)
		}
		headPath = filepath.Join(gitdirPath, "HEAD")
	}

	data, err := os.ReadFile(headPath) //nolint:gosec // path constructed from .git/HEAD
	if err != nil {
		return ""
	}

	ref := strings.TrimSpace(string(data))

	// Symbolic ref: "ref: refs/heads/<branch>"
	if strings.HasPrefix(ref, "ref: refs/heads/") {
		branch := strings.TrimPrefix(ref, "ref: refs/heads/")
		// Reftable ref storage uses "ref: refs/heads/.invalid" as a dummy HEAD stub.
		// Fall back to git to resolve the actual branch in that case.
		if branch == ".invalid" {
			return resolveWorktreeBranchGit(ctx, worktreePath)
		}
		return branch
	}

	// Detached HEAD or other ref type
	return detachedHEADDisplay
}

// resolveWorktreeBranchGit resolves the branch name by shelling out to git.
// Used as a fallback for reftable ref storage where .git/HEAD is a stub.
func resolveWorktreeBranchGit(ctx context.Context, worktreePath string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--symbolic-full-name", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return detachedHEADDisplay
	}
	ref := strings.TrimSpace(string(out))
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	return detachedHEADDisplay
}
