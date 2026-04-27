package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

type recapFlags struct {
	// range selection (mutually exclusive)
	day, week, month, d90 bool
	// agent filter (mutually exclusive, direct flags match agent names)
	claudeCode, codex, gemini, opencode, cursor, factoryaiDroid, copilotCLI bool
	// formatting
	format       string
	view         string // me | contributors | both
	refresh      bool   // clear local analysis cache and re-fetch
	insecureHTTP bool   // allow http:// API base URL (local dev)
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
	cmd.Flags().BoolVar(&f.day, "day", false, "Today only (default)")
	cmd.Flags().BoolVar(&f.week, "week", false, "Last 7 days")
	cmd.Flags().BoolVar(&f.month, "month", false, "This calendar month")
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
	cmd.Flags().BoolVar(&f.refresh, "refresh", false, "Clear the local analysis cache and re-fetch from the server")
	cmd.Flags().BoolVar(&f.insecureHTTP, "insecure-http-auth", false, "Allow plain-HTTP auth (local dev only; never set in production)")
	cmd.MarkFlagsMutuallyExclusive("day", "week", "month", "90")
	cmd.MarkFlagsMutuallyExclusive(agentFlagNames...)
	return cmd
}

func (f *recapFlags) rangeKey() recap.RangeKey {
	switch {
	case f.week:
		return recap.RangeWeek
	case f.month:
		return recap.RangeMonth
	case f.d90:
		return recap.Range90d
	}
	// --day is the default (no flag or explicit --day).
	return recap.RangeDay
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

	// --refresh clears the on-disk analysis cache so this run re-fetches
	// every checkpoint. Useful when server analysis has finished after a
	// previous "pending" run cached empties.
	if f.refresh {
		if err := clearRecapCache(ctx); err != nil {
			logging.Debug(ctx, "recap: could not clear cache (continuing)", "error", err.Error())
		}
	}

	act, err := recap.LoadRecap(ctx, recap.LoadOpts{
		Scope:         recap.ScopeCurrent,
		EnrichFromAPI: true,
		InsecureHTTP:  f.insecureHTTP,
	})
	if err != nil {
		return fmt.Errorf("load recap: %w", err)
	}

	agentFilter := f.agentName()
	rangeKey := f.rangeKey()
	now := time.Now()

	// Both me-side and team-side come from /api/v1/me/recap — the server is
	// the source of truth so the CLI numbers match entire.io's dashboard.
	// Best-effort: if the fetch fails, the view falls back to pure-local
	// me-side (offline mode) and shows team columns as "—".
	serverMe, contributors, daily, diag := fetchRecapDataWithDiag(ctx, worktreeRoot, rangeKey, now)

	view := recap.BuildView(act.Sessions, recap.BuildOpts{
		Range:        rangeKey,
		AgentFilter:  agentFilter,
		Mode:         f.mode(),
		ServerMe:     serverMe,
		Contributors: contributors,
		ServerDaily:  daily,
		Now:          now,
	})
	view.Notes = append(view.Notes, diag...)

	// Surface a hint when labels never populated despite having checkpoints —
	// this is usually the analysis-pipeline still processing new checkpoints.
	if view.Summary.CheckpointCount > 0 && labelsAllEmpty(view) {
		view.Notes = append(view.Notes,
			"Labels require server analysis (may take a few minutes after committing). "+
				"Re-run shortly to see them populate.")
	}

	if resolveFormat(f.format, w) == recapFormatTUI {
		return runRecapTUI(ctx, view, act.Sessions, agentFilter, serverMe, contributors, daily)
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

// fetchRecapDataWithDiag fetches BOTH me and team sides from /api/v1/me/recap
// in one call, returning them plus any diagnostic strings. Using the server as
// the source of truth for me-side metrics (checkpoints, tokens, labels, skills,
// tool mix) keeps CLI and entire.io dashboard in sync — they read the same
// query. Session / models / repos stay local since the server doesn't track
// those concepts.
//
// Returns (me, team, diag). Either side can be nil if the endpoint failed or
// the range has no data. Diagnostic strings surface as footer hints.
func fetchRecapDataWithDiag(
	ctx context.Context,
	worktreeRoot string,
	rangeKey recap.RangeKey,
	now time.Time,
) (serverMe, contributors *recap.ContributorsData, daily []recap.DailyCount, diag []string) {
	repo := recap.ResolveRepoFromWorktree(ctx, worktreeRoot)
	if repo == "" || repo == "unknown" {
		diag = append(diag, "Server sync requires a git remote — none detected on this worktree.")
		return nil, nil, nil, diag
	}
	token, err := auth.LookupCurrentToken()
	if err != nil || token == "" {
		diag = append(diag, "Sign in with `entire login` to sync with entire.io (counts may drift from the dashboard otherwise).")
		return nil, nil, nil, diag
	}
	client := api.NewClient(token)

	// Pass the CLI's exact range bounds so the server's aggregation window
	// matches the CLI's local-filtering window 1:1. No more "ask for today,
	// get 30 days" — server respects --day / --week / --month / --90 exactly.
	start, end := rangeKey.Bounds(now)
	resp, err := recap.FetchMeRecap(ctx, client, start, end, repo, 500)
	if err != nil {
		logging.Debug(ctx, "recap: /me/recap failed", "repo", repo, "error", err.Error())
		diag = append(diag, "Could not fetch recap data for "+repo+" — repo may not be tracked in entire.io yet.")
		return nil, nil, nil, diag
	}

	serverMe = recap.MeFromMeRecap(resp)
	contributors = recap.ContributorsFromMeRecap(resp)
	daily = resp.Daily
	if (contributors == nil || len(contributors.ByAgent) == 0) &&
		(serverMe == nil || len(serverMe.ByAgent) == 0) {
		diag = append(diag, "No activity for "+repo+" in this range — try a longer window (--week, --90).")
	}
	return serverMe, contributors, daily, diag
}

// clearRecapCache removes the recap analysis cache directory inside the
// repo's .git dir. Best-effort — missing dir / permission error both return
// nil so a failed clear doesn't block the recap from running.
func clearRecapCache(ctx context.Context) error {
	gitCommonDir, err := strategy.GetGitCommonDir(ctx)
	if err != nil {
		return nil //nolint:nilerr // best-effort; falling back to normal fetch path is fine
	}
	target := filepath.Join(gitCommonDir, "entire-recap-cache")
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove recap cache: %w", err)
	}
	return nil
}

// labelsAllEmpty reports whether the view has no label data on either the
// summary band or any agent card. Used to decide whether to surface the
// "server analysis pending" hint.
func labelsAllEmpty(v recap.View) bool {
	if v.Summary.DominantLabel != "" {
		return false
	}
	if len(v.Labels) > 0 {
		return false
	}
	for _, card := range v.AgentCards {
		if len(card.MeLabels) > 0 {
			return false
		}
	}
	return true
}

func runRecapTUI(
	ctx context.Context,
	initial recap.View,
	sessions []recap.RecapSession,
	agentFilter string,
	serverMe *recap.ContributorsData,
	contributors *recap.ContributorsData,
	daily []recap.DailyCount,
) error {
	m := recap.NewTUIModel(sessions, initial, agentFilter, serverMe, contributors, daily)
	// Alt-screen + internal viewport scroll: content stays bounded by the
	// terminal (no top-line cut-off), and arrow keys / pgup-pgdn / mouse
	// wheel scroll the body inside the TUI so a tall recap stays fully
	// reachable regardless of the host terminal's scroll behavior.
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("recap tui: %w", err)
	}
	return nil
}
