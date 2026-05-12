package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

type recapFlags struct {
	day, week, month, d90 bool
	view                  string
	agent                 string
	color                 string
	static                bool
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
			return runRecap(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}
	cmd.Flags().BoolVar(&f.day, "day", false, "Today only (default)")
	cmd.Flags().BoolVar(&f.week, "week", false, "Last 7 days")
	cmd.Flags().BoolVar(&f.month, "month", false, "This calendar month")
	cmd.Flags().BoolVar(&f.d90, "90", false, "Rolling 90 days")
	cmd.Flags().StringVar(&f.agent, "agent", recap.AgentAll, "Agent id to show, or all")
	cmd.Flags().StringVar(&f.view, "view", string(recap.ViewBoth), "Which columns to show: you, team, or both")
	cmd.Flags().StringVar(&f.color, "color", recapColorAuto, "Color output: auto, always, or never")
	cmd.Flags().BoolVar(&f.static, "static", false, "Print static output instead of opening the interactive recap")
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

func (f *recapFlags) useTUI(isTerminal, canPrompt, accessible bool) bool {
	return isTerminal && canPrompt && !accessible && !f.static
}

func runRecap(ctx context.Context, w, errW io.Writer, f *recapFlags) error {
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		fmt.Fprintln(errW, "Not a git repository. Run 'entire recap' from within a git repository.")
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
	client, err := newRecapClient(f.insecureHTTP)
	if err != nil {
		var keyringErr *keyringReadError
		switch {
		case errors.Is(err, api.ErrInsecureHTTP):
			fmt.Fprintf(errW, "ENTIRE_API_BASE_URL is set to an insecure http:// URL (%s). Use https:// for production, or pass --insecure-http-auth for local dev.\n", api.BaseURL())
		case errors.As(err, &keyringErr):
			fmt.Fprintf(errW, "Could not read your auth token from the system keyring: %v. Running `entire login` may not help — the keyring may be locked or inaccessible. Check your OS keychain settings.\n", keyringErr.Cause)
		default:
			return err
		}
		return NewSilentError(err)
	}
	rangeKey := f.rangeKey()
	repoSlug := currentRepoSlug(ctx)
	if f.useTUI(interactive.IsTerminalWriter(w), interactive.CanPromptInteractively(), IsAccessibleMode()) {
		return runRecapTUI(ctx, client, recapTUIOptions{
			Range: rangeKey,
			View:  mode,
			Agent: f.agentName(),
			Repo:  repoSlug,
			Color: color,
		})
	}
	start, end := rangeKey.Bounds(time.Now())
	resp, err := recap.FetchMeRecap(ctx, client, start, end, repoSlug, 0)
	if err != nil {
		return handleRecapFetchError(errW, err)
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

// keyringReadError marks a failure to read the auth token from the system
// keyring (locked, permission denied, etc.) — distinct from "no token saved",
// which keyring.ErrNotFound resolves to (token=="", err==nil) upstream.
type keyringReadError struct{ Cause error }

func (e *keyringReadError) Error() string {
	return "read auth token from keyring: " + e.Cause.Error()
}
func (e *keyringReadError) Unwrap() error { return e.Cause }

// newRecapClient does not gate on a missing token; FetchMeRecap surfaces 401s
// via recapLoadErrorMessage so flag effects (--week, --agent, ...) and the
// real auth error are not collapsed into one "sign in" hint. A keyring read
// failure is surfaced as *keyringReadError so the caller can show a targeted
// message instead of misattributing it to a missing login.
func newRecapClient(insecureHTTP bool) (*api.Client, error) {
	token, err := auth.LookupCurrentToken()
	if err != nil {
		return nil, &keyringReadError{Cause: err}
	}
	if token != "" && !insecureHTTP {
		if err := api.RequireSecureURL(api.BaseURL()); err != nil {
			return nil, fmt.Errorf("base URL check: %w", err)
		}
	}
	return api.NewClient(token), nil
}

func handleRecapFetchError(w io.Writer, err error) error {
	if shouldShowRecapLoadErrorMessage(err) {
		fmt.Fprintln(w, recapLoadErrorMessage(err))
		return NewSilentError(err)
	}
	return fmt.Errorf("fetch recap: %w", err)
}

func shouldShowRecapLoadErrorMessage(err error) bool {
	var apiErr *api.HTTPError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized ||
			apiErr.StatusCode == http.StatusBadRequest ||
			apiErr.StatusCode == http.StatusNotFound ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}
	return isRecapNetworkError(err)
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
