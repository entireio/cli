package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

type recapFlags struct {
	day, week, month, d90 bool
	view                  string
	agent                 string
	color                 string
	insecureHTTP          bool
}

const (
	recapColorAuto   = "auto"
	recapColorAlways = "always"
	recapColorNever  = "never"
)

func newRecapCmd() *cobra.Command {
	f := &recapFlags{view: string(recap.ViewBoth), agent: recap.AgentAll, color: recapColorAuto}
	cmd := &cobra.Command{
		Use:   "recap",
		Short: "Summarize recent checkpoint activity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecap(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().BoolVar(&f.day, "day", false, "Today only (default)")
	cmd.Flags().BoolVar(&f.week, "week", false, "Last 7 days")
	cmd.Flags().BoolVar(&f.month, "month", false, "This calendar month")
	cmd.Flags().BoolVar(&f.d90, "90", false, "Rolling 90 days")
	cmd.Flags().StringVar(&f.agent, "agent", recap.AgentAll, "Agent id to show, or all")
	cmd.Flags().StringVar(&f.view, "view", string(recap.ViewBoth), "Which columns to show: you, team, or both")
	cmd.Flags().StringVar(&f.color, "color", recapColorAuto, "Color output: auto, always, or never")
	cmd.Flags().BoolVar(&f.insecureHTTP, "insecure-http-auth", false, "Allow plain-HTTP auth (local dev only)")
	cmd.MarkFlagsMutuallyExclusive("day", "week", "month", "90")
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
	default:
		return recap.RangeDay
	}
}

func (f *recapFlags) mode() recap.ViewMode {
	switch strings.ToLower(strings.TrimSpace(f.view)) {
	case "you", "me":
		return recap.ViewYou
	case "team", "contributors":
		return recap.ViewTeam
	case "both", "":
		return recap.ViewBoth
	default:
		return recap.ViewMode(f.view)
	}
}

func (f *recapFlags) agentName() string {
	agent := strings.ToLower(strings.TrimSpace(f.agent))
	if agent == "" {
		return recap.AgentAll
	}
	return agent
}

func (f *recapFlags) colorEnabled(w io.Writer) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(f.color)) {
	case "", recapColorAuto:
		return shouldUseColor(w) && !IsAccessibleMode(), nil
	case recapColorAlways:
		return true, nil
	case recapColorNever:
		return false, nil
	default:
		return false, fmt.Errorf("invalid --color %q (use auto, always, or never)", f.color)
	}
}

func runRecap(ctx context.Context, w io.Writer, f *recapFlags) error {
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		fmt.Fprintln(w, "Not a git repository. Run 'entire recap' from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}
	mode := f.mode()
	if !mode.Valid() {
		return fmt.Errorf("invalid --view %q (use you, team, or both)", f.view)
	}
	color, err := f.colorEnabled(w)
	if err != nil {
		return err
	}
	client, err := NewAuthenticatedAPIClient(f.insecureHTTP)
	if err != nil {
		fmt.Fprintln(w, "Sign in with `entire login` to use `entire recap`.")
		return NewSilentError(err)
	}
	rangeKey := f.rangeKey()
	start, end := rangeKey.Bounds(time.Now())
	resp, err := recap.FetchMeRecap(ctx, client, start, end, currentRepoSlug(ctx), 0)
	if err != nil {
		return fmt.Errorf("fetch recap: %w", err)
	}
	fmt.Fprint(w, recap.RenderStaticRecap(resp, recap.RenderOptions{
		Range: rangeKey,
		View:  mode,
		Agent: f.agentName(),
		Width: terminalWidth(w),
		Color: color,
	}))
	fmt.Fprintln(w)
	return nil
}

func terminalWidth(w io.Writer) int {
	file, ok := w.(*os.File)
	if !ok {
		return recap.DefaultWidth
	}
	if !isatty.IsTerminal(file.Fd()) {
		return recap.DefaultWidth
	}
	width, _, err := term.GetSize(int(file.Fd())) //nolint:gosec // fd values fit in int on supported platforms
	if err != nil || width <= 0 {
		return recap.DefaultWidth
	}
	return width
}

func currentRepoSlug(ctx context.Context) string {
	_, owner, repoName, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || owner == "" || repoName == "" {
		return ""
	}
	return owner + "/" + repoName
}
