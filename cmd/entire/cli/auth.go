package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// coreSessionsPath is entire-core's login-session endpoint family
// (list / revoke / current). These are OAuth refresh-token families, served
// by the auth host (entire-core), NOT entire.io's `/api/v1/auth/tokens` —
// that path is entire.io's legacy `ent_` personal-access-token surface, which
// rejects entire-core JWTs. The CLI authenticates with a core JWT, so all
// session management goes to core.
const coreSessionsPath = "/api/auth/tokens"

// User-visible placeholder strings. lastUsedJustNow is consumed by
// formatRelativeDuration in status.go.
const (
	placeholderDash = "-"
	lastUsedJustNow = "just now"
)

// requireSecureBaseURL enforces TLS unless insecureHTTPAuth is set. Every
// command that sends a bearer token over the network (login, logout,
// auth status) must call this so credentials don't leak over plaintext HTTP
// without explicit opt-in.
//
// Both the auth and data API origins are checked: the bearer travels to the
// auth host for login + session management, and to the data host for
// search/activity/dispatch/etc. When both origins resolve to the same host
// (e.g. an explicitly collapsed single-host deployment) the redundant second
// parse is skipped.
//
// When the opt-in flag is set, the tokenmanager's matching HTTP guard is
// also relaxed via auth.EnableInsecureHTTP — otherwise an STS exchange
// against a private-network http:// auth host would fail at the
// tokenmanager layer even though the per-command TLS check was waived.
func requireSecureBaseURL(insecureHTTPAuth bool) error {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
		return nil
	}
	dataURL, authURL := api.BaseURL(), api.AuthBaseURL()
	if err := api.RequireSecureURL(dataURL); err != nil {
		return fmt.Errorf("base URL check: %w", err)
	}
	if authURL == dataURL {
		return nil
	}
	if err := api.RequireSecureURL(authURL); err != nil {
		return fmt.Errorf("auth base URL check: %w", err)
	}
	return nil
}

// newSessionsClient builds an api.Client for entire-core's login-session
// endpoints (coreSessionsPath). It targets the auth host (api.AuthBaseURL()),
// since that's where the session/refresh-token families live; the supplied
// bearer must be the session-scoped login JWT, obtained via
// resolveAuthHostToken (a same-host resolution that returns the login token
// unchanged, preserving its entire:session scope — entire-core's session
// routes require it).
func newSessionsClient(token string) *api.Client {
	return api.NewClientWithBaseURL(token, api.AuthBaseURL()).
		WithAuthTokensPath(coreSessionsPath)
}

// resolveAuthHostToken returns a bearer scoped for the auth host (entire-core).
// For the auth host's own origin the tokenmanager hits the same-host shortcut
// and returns the stored login JWT unchanged — keeping the entire:session
// scope that core's session endpoints (and /me) require, with no STS exchange.
func resolveAuthHostToken(ctx context.Context) (string, error) {
	token, err := auth.TokenForResource(ctx, api.OriginOnly(api.AuthBaseURL()))
	if err != nil {
		return "", fmt.Errorf("resolve auth-host token: %w", err)
	}
	return token, nil
}

// isKeychainTokenRejected reports whether err indicates the stored
// keyring token can't authenticate against entire-core. Failure modes that
// collapse into the single "the user must re-login" branch:
//
//   - core API returned 401 (surfaces as *coreapi.ErrorModelStatusCode),
//     or a data API 401 (api.HTTPError),
//   - tokenmanager's preflight rejected an expired core token JWT
//     (surfacing as auth.ErrNotLoggedIn even though the keyring entry
//     is still present),
//   - the STS endpoint rejected the core token during exchange in a
//     split-host setup. auth-go's sts package returns the response as
//     "token exchange: status 4xx: <code>[: <desc>]" with no typed
//     sentinel exposed, so detection has to be by prefix. The "status
//     4" anchor catches the entire 4xx range — every 4xx from STS is
//     a credential problem, none are retryable without user action.
//
// Other shapes (network errors, malformed STS response, manager
// construction failures) deliberately don't match — the user sees the
// real diagnostic instead of a misleading "re-login" hint.
func isKeychainTokenRejected(err error) bool {
	if api.IsHTTPErrorStatus(err, http.StatusUnauthorized) {
		return true
	}
	// The /me liveness probe goes through the core API client, whose 401
	// surfaces as *coreapi.ErrorModelStatusCode rather than api.HTTPError.
	var coreErr *coreapi.ErrorModelStatusCode
	if errors.As(err, &coreErr) && coreErr.StatusCode == http.StatusUnauthorized {
		return true
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return true
	}
	return strings.Contains(err.Error(), "token exchange: status 4")
}

// addInsecureHTTPAuthFlag attaches the hidden --insecure-http-auth flag used
// by every authenticated command for local development.
func addInsecureHTTPAuthFlag(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVar(target, "insecure-http-auth", false, "Allow authentication over plain HTTP (insecure, for local development only)")
	if err := cmd.Flags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication",
		Long:  "Authentication subcommands. Includes login, logout, status, and login-context management (contexts, use).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newAuthStatusCmd())
	cmd.AddCommand(newAuthContextsCmd())
	cmd.AddCommand(newAuthUseCmd())
	return cmd
}

// --- status -----------------------------------------------------------------

func newAuthStatusCmd() *cobra.Command {
	var insecureHTTPAuth bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show authentication status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			return runAuthStatus(cmd.Context(), cmd.OutOrStdout(),
				auth.NewContextStore(), defaultFetchProfile, auth.Contexts, api.AuthBaseURL())
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// authProfile is the subset of the core API's GET /me that `entire auth
// status` renders.
type authProfile struct {
	Handle         string
	DisplayName    string
	Email          string
	Provider       string
	ProviderUserID string
}

// profileFetcher fetches the logged-in user's profile via GET /me on the core
// API. Injected so status stays unit-testable without a live core.
type profileFetcher func(ctx context.Context) (*authProfile, error)

// contextsProvider returns the stored login contexts and the active context
// name, for the local-context lines in `entire auth status`. Injected for
// testability; production wires auth.Contexts.
type contextsProvider func() ([]*contexts.Context, string, error)

// defaultFetchProfile fetches the current user's profile from the core API's
// GET /me. It doubles as the liveness check for `entire auth status`: a 401
// (or an expired login that can't be exchanged) means the stored token is no
// longer usable, which isKeychainTokenRejected maps to a re-login hint.
func defaultFetchProfile(ctx context.Context) (*authProfile, error) {
	client, err := coreapi.New()
	if err != nil {
		return nil, fmt.Errorf("connect to Entire control plane: %w", err)
	}
	me, err := client.GetMe(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch profile: %w", err)
	}
	p := &authProfile{
		Provider:       me.Auth.Provider,
		ProviderUserID: me.Auth.ProviderUserId,
	}
	p.Handle, _ = me.Global.Handle.Get()
	if reg, ok := me.Regional.Get(); ok {
		p.DisplayName, _ = reg.DisplayName.Get()
		p.Email, _ = reg.Email.Get()
	}
	return p, nil
}

// runAuthStatus reports auth state without listing server-side sessions: GET
// /me validates the token and supplies the profile header, and the active
// login context is read locally. (Session listing/revocation lives on
// entire-core and is reached only by logout — see newSessionsClient.)
func runAuthStatus(ctx context.Context, w io.Writer, store tokenStore, fetchProfile profileFetcher, listContexts contextsProvider, baseURL string) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		return fmt.Errorf("read keychain: %w", err)
	}
	if token == "" {
		fmt.Fprintf(w, "Not logged in to %s\n", baseURL)
		fmt.Fprintln(w, "Run 'entire login' to authenticate.")
		return nil
	}

	profile, err := fetchProfile(ctx)
	if err != nil {
		if isKeychainTokenRejected(err) {
			fmt.Fprintf(w, "Token in keychain for %s is no longer valid.\n", baseURL)
			fmt.Fprintln(w, "Run 'entire login' to re-authenticate.")
			return nil
		}
		return fmt.Errorf("validate token: %w", err)
	}

	fmt.Fprintf(w, "Logged in to %s\n", baseURL)
	writeProfileLines(w, profile)

	// Local context info is informational; a read failure shouldn't fail the
	// command, so on error we just skip the context lines.
	all, current, ctxErr := listContexts()
	if ctxErr == nil && current != "" {
		fmt.Fprintf(w, "  %-9s %s\n", "Context:", current)
	}
	fmt.Fprintf(w, "  %-9s %s\n", "Token:", "stored in OS keychain")

	if ctxErr == nil && len(all) > 1 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d login contexts saved; run 'entire auth contexts' to list or 'entire auth use <name>' to switch.\n", len(all))
	}
	return nil
}

// writeProfileLines renders the user identity from GET /me as aligned
// label/value lines, omitting any field the server didn't populate.
func writeProfileLines(w io.Writer, p *authProfile) {
	var parts []string
	if p.DisplayName != "" {
		parts = append(parts, p.DisplayName)
	}
	if p.Handle != "" {
		parts = append(parts, "@"+p.Handle)
	}
	if p.Email != "" {
		parts = append(parts, "<"+p.Email+">")
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "  %-9s %s\n", "User:", strings.Join(parts, " "))
	}
	if p.Provider != "" {
		identity := p.Provider
		if p.ProviderUserID != "" {
			identity += "/" + p.ProviderUserID
		}
		fmt.Fprintf(w, "  %-9s %s\n", "Identity:", identity)
	}
}

// --- auth tables -------------------------------------------------------------

// authTableStyles holds the lipgloss styles for the `entire auth contexts`
// table. Mirrors the approach in activity_render.go: keep style construction
// tied to color detection, and render plain text when color is disabled.
type authTableStyles struct {
	colorEnabled bool

	header lipgloss.Style // bold + dim, used for column headers
	id     lipgloss.Style // yellow accent (active-context marker)
	name   lipgloss.Style // bold (active context name)
	value  lipgloss.Style // default fg
}

func newAuthTableStyles(w io.Writer) authTableStyles {
	useColor := shouldUseColor(w)
	s := authTableStyles{colorEnabled: useColor}
	if !useColor {
		return s
	}
	s.header = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
	s.id = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	s.name = lipgloss.NewStyle().Bold(true)
	s.value = lipgloss.NewStyle() // default fg
	return s
}

func (s authTableStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

// renderAlignedTable writes header followed by rows in left-aligned columns,
// sizing each column to its widest (possibly pre-styled) cell. Column widths
// use lipgloss.Width so ANSI escapes don't inflate the padding.
func renderAlignedTable(w io.Writer, header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if cw := lipgloss.Width(c); cw > widths[i] {
				widths[i] = cw
			}
		}
	}

	writeRow(w, header, widths)
	for _, row := range rows {
		writeRow(w, row, widths)
	}
}

func writeRow(w io.Writer, cells []string, widths []int) {
	for i, c := range cells {
		fmt.Fprint(w, c)
		if i < len(cells)-1 {
			fmt.Fprint(w, strings.Repeat(" ", widths[i]-lipgloss.Width(c)+2))
		}
	}
	fmt.Fprintln(w)
}

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}
