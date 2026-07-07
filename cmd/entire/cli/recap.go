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
	// repoName is the human owner/repo label for the scope line: the ?repo=
	// scope value is a repo_id ULID when routed to a cell (echoed back verbatim
	// by the response), which is meaningless to show a user. Both are empty when
	// no repo query is sent, so an unscoped recap isn't mislabelled.
	client, repoScope, repoName, err := newRecapClient(ctx, f.insecureHTTP)
	if err != nil {
		if errors.Is(err, api.ErrInsecureHTTP) {
			fmt.Fprintf(errW, "ENTIRE_API_BASE_URL is set to an insecure http:// URL (%s). Use https:// for production, or pass --insecure-http-auth for local dev.\n", api.BaseURL())
			return NewSilentError(err)
		}
		// Token resolution can fail for many reasons unrelated to the
		// keyring — STS exchange rejected, network error, audience
		// misconfiguration. Surface the underlying error verbatim
		// rather than misattributing it to a missing or locked
		// keyring entry; main.go's default printer is honest about
		// what went wrong.
		return err
	}
	rangeKey := f.rangeKey()
	if f.useTUI(interactive.IsTerminalWriter(w), interactive.CanPromptInteractively(), IsAccessibleMode()) {
		return runRecapTUI(ctx, client, recapTUIOptions{
			Range:    rangeKey,
			View:     mode,
			Agent:    f.agentName(),
			Repo:     repoScope,
			RepoName: repoName,
			Color:    color,
		})
	}
	start, end := rangeKey.Bounds(time.Now())
	resp, err := recap.FetchMeRecap(ctx, client, start, end, repoScope, 0)
	if err != nil {
		return handleRecapFetchError(errW, err)
	}
	fmt.Fprint(w, recap.RenderStaticRecap(resp, recap.RenderOptions{
		Range:    rangeKey,
		View:     mode,
		Agent:    f.agentName(),
		Width:    terminalWidth(w),
		Color:    color,
		RepoName: repoName,
	}))
	fmt.Fprintln(w)
	return nil
}

// newRecapClient does not gate on a missing token; FetchMeRecap surfaces
// 401s via recapLoadErrorMessage so flag effects (--week, --agent, ...)
// and the real auth error are not collapsed into one "sign in" hint.
//
// Goes through auth.ResolveDataAPIToken (the same context-aware path as
// activity/search/dispatch) so the data host's /.well-known/entire-api.json
// picks the matching login context and exchanges for the advertised audience;
// a host that doesn't advertise discovery is a surfaced error, not a fallback.
// ErrNotLoggedIn is collapsed back into an empty token so the caller's "render
// with no bearer, let the server respond 401" path still fires. Every other
// resolution failure (no eligible/ambiguous context, STS exchange rejected,
// network error, keyring locked) surfaces verbatim to the caller — previously
// these were all relabelled as keyring read failures via keyringReadError,
// which sent users on wild goose chases when the keyring was fine and the real
// problem was downstream.
// newRecapClient returns the recap client, the value to pass as /me/recap's
// ?repo= (its team/contributors scope), and the repo's owner/repo display name.
// The scope is the current repo's ULID when routed to an entire-api cell (which
// addresses repos by id), or its owner/repo slug on the data API (which
// addresses them by name); both come from one remote/mirror resolution, so the
// caller never re-resolves for display. Empty when the current repo can't be
// resolved — recap then shows the personal side only.
//
// It prefers the caller's home entire-api cell (the shared client) and falls
// back to the data API on ANY cell-client failure — the cell path is a
// best-effort upgrade, so a cell problem must never break a command that worked
// before it existed. Expected fallbacks (region has no cell yet, not logged in)
// are silent; unexpected ones are debug-logged (logCellClientFallback). Only
// failures of the data-API path itself surface — except ErrNotLoggedIn, which
// recap tolerates, rendering and letting the server answer 401.
func newRecapClient(ctx context.Context, insecureHTTP bool) (client *api.Client, repoScope, repoName string, err error) {
	// Best-effort upgrade: on any cell failure fall back to the data API path
	// below, which tolerates a missing login (renders, lets the server 401) so
	// the not-logged-in case keeps working.
	cellClient, cellErr := auth.NewEntireAPICellClient(ctx, insecureHTTP, nil)
	if cellErr == nil {
		repoID, repoSlug := currentRepoRef(ctx)
		return cellClient, repoID, repoSlug, nil
	}
	logCellClientFallback(ctx, cellErr)

	if insecureHTTP {
		auth.EnableInsecureHTTP()
	}
	token, err := auth.ResolveDataAPIToken(ctx, api.BaseURL())
	if errors.Is(err, auth.ErrNotLoggedIn) {
		token = ""
		err = nil
	}
	if err != nil {
		return nil, "", "", err
	}
	if token != "" && !insecureHTTP {
		if err := api.RequireSecureURL(api.BaseURL()); err != nil {
			return nil, "", "", fmt.Errorf("base URL check: %w", err)
		}
	}
	// The data API scopes by slug, so scope and display name coincide.
	slug := currentRepoSlug(ctx)
	return api.NewClient(token), slug, slug, nil
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
