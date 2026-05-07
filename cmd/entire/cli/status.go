package cli

import (
	"context"
	"encoding/json"
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
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"
)

type headLinkage struct {
	commitHash    string
	checkpointIDs []string
}

func newStatusCmd() *cobra.Command {
	var detailed bool
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show Entire status",
		Long:  "Show whether Entire is currently enabled or disabled",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), detailed, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "Show detailed status for each settings file")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.MarkFlagsMutuallyExclusive("detailed", "json")

	return cmd
}

func runStatus(ctx context.Context, w io.Writer, detailed, jsonOutput bool) error {
	if jsonOutput {
		return runStatusJSON(ctx, w)
	}

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

	// Show enabled agents
	if s.Enabled {
		if displayNames := InstalledAgentDisplayNames(ctx); len(displayNames) > 0 {
			b.WriteString("\n")
			b.WriteString(sty.render(sty.dim, "  Agents · "))

			b.WriteString(strings.Join(displayNames, ", "))
		}
	}

	// Show review status for HEAD's checkpoint, if any.
	if reviewed, meta := headHasReviewCheckpoint(ctx); reviewed {
		b.WriteString("\n")
		b.WriteString(sty.render(sty.dim, "  Review · "))
		b.WriteString("reviewed (")
		b.WriteString(meta)
		b.WriteString(")")
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
	return formatRelativeDuration(time.Since(t))
}

// formatRelativeDuration renders a positive duration as "just now" / "Xm ago"
// / "Xh ago" / "Xd ago". Shared between `entire status` and `entire auth list`
// so the bucket thresholds and labels stay consistent.
func formatRelativeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return lastUsedJustNow
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
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

	repoRoot, head, headErr := currentHeadLinkage(ctx)
	divergenceWarnings := make(map[string]string)
	if headErr == nil && repoRoot != "" && head.commitHash != "" {
		divergenceWarnings = computeSessionDivergenceWarnings(repoRoot, active, head)
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

			// Line 1: Agent (model) · sessionID
			if st.ModelName != "" {
				fmt.Fprintf(w, "%s %s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "("+st.ModelName+")"),
					sty.render(sty.dim, "·"),
					st.SessionID)
			} else {
				fmt.Fprintf(w, "%s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "·"),
					st.SessionID)
			}

			// Line 2: > "first prompt" (chevron + quoted, truncated)
			if st.LastPrompt != "" {
				prompt := stringutil.TruncateRunes(st.LastPrompt, 60, "...")
				fmt.Fprintf(w, "%s \"%s\"\n", sty.render(sty.dim, ">"), prompt)
			}

			// Line 3: stats line — started Xd ago · active now · files N · tokens X.Xk
			var stats []string
			stats = append(stats, "started "+timeAgo(st.StartedAt))

			if st.LastInteractionTime != nil && st.LastInteractionTime.Sub(st.StartedAt) > time.Minute {
				stats = append(stats, activeTimeDisplay(st.LastInteractionTime))
			}

			if t := totalTokens(st.TokenUsage); t > 0 {
				stats = append(stats, "tokens "+formatTokenCount(t))
			}

			statsLine := strings.Join(stats, sty.render(sty.dim, " · "))
			if st.IsStuckActive() {
				fmt.Fprintf(w, "%s %s %s\n", sty.render(sty.dim, statsLine),
					sty.render(sty.dim, "·"),
					sty.render(sty.yellow, "stale")+" (run 'entire doctor')")
			} else {
				fmt.Fprintln(w, sty.render(sty.dim, statsLine))
			}
			if warning := divergenceWarnings[st.SessionID]; warning != "" {
				fmt.Fprintf(w, "%s %s\n", sty.render(sty.yellow, "!"), sty.render(sty.yellow, warning))
			}
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

func currentHeadLinkage(ctx context.Context) (string, headLinkage, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("resolve worktree root: %w", err)
	}

	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("open repo: %w", err)
	}

	headRef, err := repo.Head()
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("resolve HEAD: %w", err)
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("load HEAD commit: %w", err)
	}

	head := headLinkage{commitHash: headRef.Hash().String()}
	if checkpointIDs := trailers.ParseAllCheckpoints(commit.Message); len(checkpointIDs) > 0 {
		head.checkpointIDs = make([]string, 0, len(checkpointIDs))
		for _, checkpointID := range checkpointIDs {
			head.checkpointIDs = append(head.checkpointIDs, checkpointID.String())
		}
	}

	return repoRoot, head, nil
}

func computeSessionDivergenceWarnings(
	repoRoot string,
	active []*session.State,
	head headLinkage,
) map[string]string {
	warnings := make(map[string]string)
	normalizedRepoRoot := normalizeWorktreePath(repoRoot)

	for _, st := range active {
		if normalizeWorktreePath(st.WorktreePath) != normalizedRepoRoot {
			continue
		}

		if st.BaseCommit == "" {
			// Session linkage is incomplete (migration refuses to run and save-step
			// must reinitialize). Surface this explicitly rather than skipping silently,
			// so operators don't see a false-clean status for a session that cannot
			// be attributed until the next prompt reinitializes it.
			warnings[st.SessionID] = "session linkage incomplete; awaiting reinitialization"
			continue
		}

		if st.BaseCommit == head.commitHash {
			if st.AttributionBaseCommit != "" && st.AttributionBaseCommit != st.BaseCommit {
				warnings[st.SessionID] = "attribution base diverged after history movement; figures may be off until next checkpoint"
			}
			continue
		}

		// BaseCommit != HEAD — hooks haven't reconciled/migrated yet
		if len(head.checkpointIDs) > 0 {
			warnings[st.SessionID] = "tracking diverged from current HEAD; HEAD links to checkpoint(s) " + strings.Join(head.checkpointIDs, ", ")
			continue
		}

		warnings[st.SessionID] = "tracking diverged from current HEAD after git history movement"
	}

	return warnings
}

func normalizeWorktreePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// statusJSON is the JSON output for `entire status --json`.
type statusJSON struct {
	Enabled        bool               `json:"enabled"`
	Agents         []string           `json:"agents"`
	ActiveSessions []sessionBriefJSON `json:"active_sessions"`
	Error          string             `json:"error,omitempty"`
}

type sessionBriefJSON struct {
	Agent  string `json:"agent"`
	Model  string `json:"model,omitempty"`
	Status string `json:"status"`
}

func runStatusJSON(ctx context.Context, w io.Writer) error {
	writeJSON := func(v statusJSON) error {
		return json.NewEncoder(w).Encode(v)
	}

	if _, err := paths.WorktreeRoot(ctx); err != nil {
		return writeJSON(statusJSON{Error: "not a git repository"})
	}

	settingsPath, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		settingsPath = EntireSettingsFile
	}
	localSettingsPath, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localSettingsPath = EntireSettingsLocalFile
	}

	_, projectErr := os.Stat(settingsPath)
	if projectErr != nil && !errors.Is(projectErr, fs.ErrNotExist) {
		return writeJSON(statusJSON{Error: fmt.Sprintf("cannot access project settings file: %v", projectErr)})
	}
	_, localErr := os.Stat(localSettingsPath)
	if localErr != nil && !errors.Is(localErr, fs.ErrNotExist) {
		return writeJSON(statusJSON{Error: fmt.Sprintf("cannot access local settings file: %v", localErr)})
	}

	if projectErr != nil && localErr != nil {
		return writeJSON(statusJSON{Error: "not set up"})
	}

	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return writeJSON(statusJSON{Error: fmt.Sprintf("failed to load settings: %v", err)})
	}

	result := statusJSON{
		Enabled:        s.Enabled,
		Agents:         []string{},
		ActiveSessions: []sessionBriefJSON{},
	}

	if s.Enabled {
		if names := InstalledAgentDisplayNames(ctx); len(names) > 0 {
			result.Agents = names
		}

		if store, err := session.NewStateStore(ctx); err == nil {
			if states, err := store.List(ctx); err == nil {
				// Deduplicate by agent: one entry per agent, "active" wins over "idle".
				type agentEntry struct {
					brief    sessionBriefJSON
					isActive bool
				}
				byAgent := make(map[string]*agentEntry)
				for _, st := range states {
					if st.EndedAt != nil {
						continue
					}
					agent := string(st.AgentType)
					if agent == "" {
						agent = unknownPlaceholder
					}
					active := st.Phase == session.PhaseActive
					if existing, ok := byAgent[agent]; ok {
						if active && !existing.isActive {
							existing.brief.Model = st.ModelName
							existing.brief.Status = sessionStatusLabel(st)
							existing.isActive = true
						}
					} else {
						byAgent[agent] = &agentEntry{
							brief: sessionBriefJSON{
								Agent:  agent,
								Model:  st.ModelName,
								Status: sessionStatusLabel(st),
							},
							isActive: active,
						}
					}
				}
				for _, e := range byAgent {
					result.ActiveSessions = append(result.ActiveSessions, e.brief)
				}
				sort.Slice(result.ActiveSessions, func(i, j int) bool {
					return result.ActiveSessions[i].Agent < result.ActiveSessions[j].Agent
				})
			}
		}
	}

	return writeJSON(result)
}

// sessionStatusLabel derives a display status from a session state.
func sessionStatusLabel(s *session.State) string {
	if s.EndedAt != nil {
		return "ended"
	}
	if s.Phase != "" {
		return string(s.Phase)
	}
	return string(session.PhaseIdle)
}
