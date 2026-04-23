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
	day, week, month, d30, d90 bool
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
	cmd.Flags().BoolVar(&f.refresh, "refresh", false, "Clear the local analysis cache and re-fetch from the server")
	cmd.Flags().BoolVar(&f.insecureHTTP, "insecure-http-auth", false, "Allow plain-HTTP auth (local dev only; never set in production)")
	cmd.MarkFlagsMutuallyExclusive("day", "week", "month", "30", "90")
	cmd.MarkFlagsMutuallyExclusive(agentFlagNames...)
	return cmd
}

func (f *recapFlags) rangeKey() recap.RangeKey {
	switch {
	case f.week:
		return recap.RangeWeek
	case f.month:
		return recap.RangeMonth
	case f.d30:
		return recap.Range30d
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

	// Contributors come from the repo-overview endpoints. Best-effort: a
	// fetch failure just leaves the contributors columns as "—" rather than
	// blocking the recap output. We also track *why* contributors might be
	// empty so we can surface a helpful hint below.
	contributors, diag := fetchContributorsWithDiag(ctx, worktreeRoot, rangeKey, now)

	view := recap.BuildView(act.Sessions, recap.BuildOpts{
		Range:        rangeKey,
		AgentFilter:  agentFilter,
		Mode:         f.mode(),
		Contributors: contributors,
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

// fetchContributorsWithDiag fetches contributor data and also returns a slice
// of diagnostic strings for the cases where data is unavailable. Empty
// diag slice means "data either came through or was intentionally skipped
// without user-visible cause." Non-empty means we want the renderer to
// surface a hint so users understand why the contributors column is empty.
//
// Tries /api/v1/me/recap first (new server-side aggregation, one call).
// Falls back to the old per-endpoint fetch stack when the new endpoint
// isn't deployed yet (404) or fails, so this keeps working against older
// server deployments.
func fetchContributorsWithDiag(
	ctx context.Context,
	worktreeRoot string,
	rangeKey recap.RangeKey,
	now time.Time,
) (*recap.ContributorsData, []string) {
	var diag []string
	repo := recap.ResolveRepoFromWorktree(ctx, worktreeRoot)
	if repo == "" || repo == "unknown" {
		diag = append(diag, "Contributor comparisons require a git remote — none detected on this worktree.")
		return nil, diag
	}
	token, err := auth.LookupCurrentToken()
	if err != nil || token == "" {
		diag = append(diag, "Sign in with `entire login` to see contributor comparisons (team tokens, agents, skills).")
		return nil, diag
	}
	client := api.NewClient(token)

	// Prefer the consolidated /me/recap endpoint. When it 200s we skip the
	// older three-endpoint fetch path entirely.
	if resp, err := recap.FetchMeRecap(ctx, client, recap.TimeframeForRange(rangeKey), repo, 500); err == nil && resp != nil && len(resp.Agents) > 0 {
		data := recap.ContributorsFromMeRecap(resp)
		if data != nil && len(data.ByAgent) > 0 {
			return data, nil
		}
		diag = append(diag, "No contributor activity for "+repo+" in this range — try a longer window (--week, --30, --90).")
		return nil, diag
	} else if err != nil {
		logging.Debug(ctx, "recap: /me/recap failed; falling back", "repo", repo, "error", err.Error())
	}

	// Fallback: old per-endpoint fetch stack. Kept so the CLI still works
	// against server deployments that don't have /me/recap yet.
	start, end := rangeKey.Bounds(now)
	data, err := recap.FetchContributors(ctx, client, repo, start, end)
	if err != nil {
		logging.Debug(ctx, "recap: fetch contributors failed", "repo", repo, "error", err.Error())
		diag = append(diag, "Could not fetch contributor data for "+repo+" — repo may not be tracked in entire.io yet.")
		return nil, diag
	}
	if data == nil || len(data.ByAgent) == 0 {
		diag = append(diag, "No contributor activity for "+repo+" in this range — try a longer window (--week, --30, --90).")
		return nil, diag
	}
	return data, nil
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

func runRecapTUI(ctx context.Context, initial recap.View, sessions []recap.RecapSession, agentFilter string) error {
	m := recap.NewTUIModel(sessions, initial, agentFilter)
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
