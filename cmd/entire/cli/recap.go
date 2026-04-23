package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

type recapFlags struct {
	// range selection (mutually exclusive)
	day, week, month, d30, d90 bool
	// agent filter (mutually exclusive, direct flags match agent names)
	claudeCode, codex, gemini, opencode, cursor, factoryaiDroid, copilotCLI bool
	// formatting
	format string
	view   string // me | contributors | both
}

// Canonical agent identifiers. Match session.State AgentType values so the
// filter predicate hits correctly.
const (
	agentClaudeCode     = "claude-code"
	agentCodex          = "codex"
	agentGeminiCLI      = "gemini-cli"
	agentOpencode       = "opencode"
	agentCursor         = "cursor"
	agentFactoryAiDroid = "factoryai-droid"
	agentCopilotCLI     = "copilot-cli"
)

// agentFlagNames lists all direct agent-filter flag names. Kept in one place
// so MarkFlagsMutuallyExclusive and help text stay consistent with agentName.
var agentFlagNames = []string{
	agentClaudeCode, agentCodex, agentGeminiCLI,
	agentOpencode, agentCursor, agentFactoryAiDroid, agentCopilotCLI,
}

const (
	recapFormatAuto    = "auto"
	recapFormatTUI     = "tui"
	recapFormatStatic  = "static"
	recapDefaultWidth  = 100
	recapMinTermHeight = 10
)

func newRecapCmd() *cobra.Command {
	f := &recapFlags{}
	cmd := &cobra.Command{
		Use:   "recap",
		Short: "Summarize checkpoint activity in the terminal",
		Long: `Recap shows what happened, when, and what to do next.

Pulls checkpoint analyses from entire.io (when logged in) and combines them
with local session state, then renders a 4-panel view over a time range.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecap(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().BoolVar(&f.day, "day", false, "Today only")
	cmd.Flags().BoolVar(&f.week, "week", false, "Last 7 days (default)")
	cmd.Flags().BoolVar(&f.month, "month", false, "This calendar month")
	cmd.Flags().BoolVar(&f.d30, "30", false, "Rolling 30 days")
	cmd.Flags().BoolVar(&f.d90, "90", false, "Rolling 90 days")
	cmd.Flags().BoolVar(&f.claudeCode, agentClaudeCode, false, "Filter to Claude Code sessions")
	cmd.Flags().BoolVar(&f.codex, agentCodex, false, "Filter to Codex sessions")
	cmd.Flags().BoolVar(&f.gemini, agentGeminiCLI, false, "Filter to Gemini CLI sessions")
	cmd.Flags().BoolVar(&f.opencode, agentOpencode, false, "Filter to OpenCode sessions")
	cmd.Flags().BoolVar(&f.cursor, agentCursor, false, "Filter to Cursor sessions")
	cmd.Flags().BoolVar(&f.factoryaiDroid, agentFactoryAiDroid, false, "Filter to Factory AI Droid sessions")
	cmd.Flags().BoolVar(&f.copilotCLI, agentCopilotCLI, false, "Filter to GitHub Copilot CLI sessions")
	cmd.Flags().StringVar(&f.format, "format", recapFormatAuto, "Output format: tui, static, or auto")
	cmd.Flags().StringVar(&f.view, "view", "both", "Which columns to show: me, contributors, or both")
	cmd.MarkFlagsMutuallyExclusive("day", "week", "month", "30", "90")
	cmd.MarkFlagsMutuallyExclusive(agentFlagNames...)
	return cmd
}

func (f *recapFlags) rangeKey() recap.RangeKey {
	switch {
	case f.day:
		return recap.RangeDay
	case f.month:
		return recap.RangeMonth
	case f.d30:
		return recap.Range30d
	case f.d90:
		return recap.Range90d
	}
	// --week is the default (no flag or explicit --week).
	return recap.RangeWeek
}

func (f *recapFlags) mode() recap.ViewMode {
	switch f.view {
	case "me":
		return recap.ViewMe
	case "contributors":
		return recap.ViewContributors
	}
	return recap.ViewBoth
}

// agentName returns the canonical agent identifier for whichever direct flag
// is set, or "" if no filter is active. Canonical names here match what
// session.State records as AgentType, so the filter matches correctly.
func (f *recapFlags) agentName() string {
	switch {
	case f.claudeCode:
		return agentClaudeCode
	case f.codex:
		return agentCodex
	case f.gemini:
		return agentGeminiCLI
	case f.opencode:
		return agentOpencode
	case f.cursor:
		return agentCursor
	case f.factoryaiDroid:
		return agentFactoryAiDroid
	case f.copilotCLI:
		return agentCopilotCLI
	}
	return ""
}

func runRecap(ctx context.Context, w io.Writer, f *recapFlags) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd := &cobra.Command{}
		cmd.SilenceUsage = true
		fmt.Fprintln(w, "Not a git repository. Run 'entire recap' from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}

	act, err := recap.LoadRecap(ctx, recap.LoadOpts{
		Scope:         recap.ScopeCurrent,
		EnrichFromAPI: true,
	})
	if err != nil {
		return fmt.Errorf("load recap: %w", err)
	}

	agentFilter := f.agentName()
	rangeKey := f.rangeKey()
	now := time.Now()

	// Contributors come from the repo-overview endpoints. Best-effort: a
	// fetch failure just leaves the contributors columns as "—" rather than
	// blocking the recap output.
	contributors := fetchContributorsBestEffort(ctx, worktreeRoot, rangeKey, now)

	view := recap.BuildView(act.Sessions, recap.BuildOpts{
		Range:        rangeKey,
		AgentFilter:  agentFilter,
		Mode:         f.mode(),
		Contributors: contributors,
		Now:          now,
	})

	if resolveFormat(f.format, w) == recapFormatTUI {
		return runRecapTUI(ctx, view, act.Sessions, agentFilter)
	}
	styles := newStylesFor(w)
	width := terminalWidth(w)
	fmt.Fprint(w, recap.RenderStatic(view, styles, width))
	fmt.Fprintln(w)
	return nil
}

// resolveFormat picks static vs TUI. In auto mode, TUI only when stdout is a
// TTY, ACCESSIBLE isn't set, and we're not inside `go test`.
func resolveFormat(requested string, w io.Writer) string {
	switch requested {
	case recapFormatTUI:
		return recapFormatTUI
	case recapFormatStatic:
		return recapFormatStatic
	}
	if os.Getenv("ACCESSIBLE") != "" {
		return recapFormatStatic
	}
	f, ok := w.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return recapFormatStatic
	}
	return recapFormatTUI
}

func newStylesFor(w io.Writer) recap.Styles {
	useColor := false
	if f, ok := w.(*os.File); ok && isatty.IsTerminal(f.Fd()) && os.Getenv("ACCESSIBLE") == "" {
		useColor = true
	}
	return recap.NewStyles(useColor)
}

func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return recapDefaultWidth
	}
	width, _, err := term.GetSize(int(f.Fd())) //nolint:gosec // fd values fit in int on all supported platforms
	if err != nil || width <= 0 {
		return recapDefaultWidth
	}
	return width
}

// fetchContributorsBestEffort resolves the repo from the current worktree,
// fetches the three repo-overview endpoints in parallel, and returns nil on
// any failure. Contributors is optional enrichment — the CLI stays usable
// when the user is offline, unauthed, or in a repo that isn't tracked.
func fetchContributorsBestEffort(ctx context.Context, worktreeRoot string, rangeKey recap.RangeKey, now time.Time) *recap.ContributorsData {
	repo := recap.ResolveRepoFromWorktree(ctx, worktreeRoot)
	if repo == "" {
		return nil
	}
	token, err := auth.LookupCurrentToken()
	if err != nil || token == "" {
		return nil
	}
	client := api.NewClient(token)
	start, end := rangeKey.Bounds(now)
	data, err := recap.FetchContributors(ctx, client, repo, start, end)
	if err != nil {
		logging.Debug(ctx, "recap: fetch contributors failed", "repo", repo, "error", err.Error())
		return nil
	}
	return data
}

func runRecapTUI(ctx context.Context, initial recap.View, sessions []recap.RecapSession, agentFilter string) error {
	m := recap.NewTUIModel(sessions, initial, agentFilter)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen())
	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("recap tui: %w", err)
	}
	return nil
}
