package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
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
			return runStatus(cmd.OutOrStdout(), detailed)
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "Show detailed status for each settings file")

	return cmd
}

func runStatus(w io.Writer, detailed bool) error {
	// Check if we're in a git repository
	if _, repoErr := paths.RepoRoot(); repoErr != nil {
		fmt.Fprintln(w, "✕ not a git repository")
		return nil //nolint:nilerr // Not being in a git repo is a valid status, not an error
	}

	// Get absolute paths for settings files
	settingsPath, err := paths.AbsPath(EntireSettingsFile)
	if err != nil {
		settingsPath = EntireSettingsFile
	}
	localSettingsPath, err := paths.AbsPath(EntireSettingsLocalFile)
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

	if detailed {
		return runStatusDetailed(w, settingsPath, localSettingsPath, projectExists, localExists)
	}

	// Short output: just show the effective/merged state
	settings, err := LoadEntireSettings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	fmt.Fprintln(w, formatSettingsStatusShort(settings))

	if settings.Enabled {
		writeActiveSessions(w)
	}

	return nil
}

// runStatusDetailed shows the effective status plus detailed status for each settings file.
func runStatusDetailed(w io.Writer, settingsPath, localSettingsPath string, projectExists, localExists bool) error {
	// First show the effective/merged status
	effectiveSettings, err := LoadEntireSettings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	fmt.Fprintln(w, formatSettingsStatusShort(effectiveSettings))
	fmt.Fprintln(w) // blank line

	// Show project settings if it exists
	if projectExists {
		projectSettings, err := settings.LoadFromFile(settingsPath)
		if err != nil {
			return fmt.Errorf("failed to load project settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Project", projectSettings))
	}

	// Show local settings if it exists
	if localExists {
		localSettings, err := settings.LoadFromFile(localSettingsPath)
		if err != nil {
			return fmt.Errorf("failed to load local settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Local", localSettings))
	}

	if effectiveSettings.Enabled {
		writeActiveSessions(w)
	}

	return nil
}

// formatSettingsStatusShort formats a short settings status line.
// Output format: "Enabled (manual-commit)" or "Disabled (auto-commit)"
func formatSettingsStatusShort(settings *EntireSettings) string {
	displayName := settings.Strategy
	if dn, ok := strategyInternalToDisplay[settings.Strategy]; ok {
		displayName = dn
	}

	if settings.Enabled {
		return fmt.Sprintf("Enabled (%s)", displayName)
	}
	return fmt.Sprintf("Disabled (%s)", displayName)
}

// formatSettingsStatus formats a settings status line with source prefix.
// Output format: "Project, enabled (manual-commit)" or "Local, disabled (auto-commit)"
func formatSettingsStatus(prefix string, settings *EntireSettings) string {
	displayName := settings.Strategy
	if dn, ok := strategyInternalToDisplay[settings.Strategy]; ok {
		displayName = dn
	}

	if settings.Enabled {
		return fmt.Sprintf("%s, enabled (%s)", prefix, displayName)
	}
	return fmt.Sprintf("%s, disabled (%s)", prefix, displayName)
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

const unknownPlaceholder = "(unknown)"

// writeActiveSessions writes active session information grouped by worktree.
func writeActiveSessions(w io.Writer) {
	store, err := session.NewStateStore()
	if err != nil {
		return
	}

	states, err := store.List(context.Background())
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
			g.branch = resolveWorktreeBranch(g.path)
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

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Active Sessions:")
	for i, g := range sortedGroups {
		header := g.path
		if g.branch != "" {
			header += " (" + g.branch + ")"
		}
		fmt.Fprintf(w, "  %s\n", header)

		for _, st := range g.sessions {
			agentLabel := string(st.AgentType)
			if agentLabel == "" {
				agentLabel = unknownPlaceholder
			}

			shortID := st.SessionID
			if len(shortID) > 7 {
				shortID = shortID[:7]
			}

			age := "started " + timeAgo(st.StartedAt)

			// Show "active X ago" when LastInteractionAt differs meaningfully from StartedAt
			activeStr := ""
			if st.LastInteractionAt != nil && st.LastInteractionAt.Sub(st.StartedAt) > time.Minute {
				activeStr = ", active " + timeAgo(*st.LastInteractionAt)
			}

			fmt.Fprintf(w, "    [%s] %-9s %s%s\n",
				agentLabel, shortID, age, activeStr)

			// Show first prompt on indented second line
			if st.FirstPrompt != "" {
				prompt := stringutil.TruncateRunes(st.FirstPrompt, 60, "...")
				fmt.Fprintf(w, "      \"%s\"\n", prompt)
			}
		}

		// Blank line between groups, but not after the last one
		if i < len(sortedGroups)-1 {
			fmt.Fprintln(w)
		}
	}
}

// resolveWorktreeBranch resolves the current branch for a worktree path.
func resolveWorktreeBranch(worktreePath string) string {
	cmd := exec.CommandContext(context.Background(), "git", "-C", worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
