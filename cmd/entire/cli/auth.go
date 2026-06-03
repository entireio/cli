package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// coreSessionsPath is entire-core's login-session endpoint family
// (list / revoke / current) on the auth host. Sessions are OAuth
// refresh-token families; the CLI authenticates against them with its core
// JWT. Session management must target the auth host (entire-core), never the
// data host.
const coreSessionsPath = "/api/auth/tokens"

// User-visible placeholder strings. lastUsedJustNow is consumed by
// formatRelativeDuration in status.go.
const (
	placeholderDash = "-"
	lastUsedNever   = "never"
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
// endpoints (coreSessionsPath) on coreURL, authenticated with the
// session-scoped login JWT. coreURL is the active context's CoreURL (or the
// configured auth host when no context is active) — session management always
// targets a login server, never the data host.
func newSessionsClient(coreURL, token string) *api.Client {
	return api.NewClientWithBaseURL(token, coreURL).WithSessionsPath(coreSessionsPath)
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
	// A 401 whose body isn't JSON (e.g. a gateway returning text/plain) fails
	// the ogen typed decode, so it never becomes an ErrorModelStatusCode — it
	// arrives as a decode error whose message carries "(code 401)". Match that
	// so the user still gets the re-login hint, not a raw decode dump.
	if strings.Contains(err.Error(), "code 401") {
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
			target := resolveStatusTarget(auth.NewContextStore(), auth.Contexts, api.AuthBaseURL())
			// We send the session token to target.coreURL; enforce TLS on it
			// too (it may differ from AuthBaseURL when a context is active).
			if !insecureHTTPAuth {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					return fmt.Errorf("context core URL check: %w", err)
				}
			}
			return runAuthStatus(cmd.Context(), cmd.OutOrStdout(), defaultFetchProfile, defaultListSessions, target)
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

// profileFetcher fetches a user's profile via GET /me on coreURL, authenticated
// with token. Injected so status stays unit-testable without a live core.
type profileFetcher func(ctx context.Context, coreURL, token string) (*authProfile, error)

// sessionLister lists the active login sessions on coreURL (the user's
// refresh-token families). Injected for testability; production wires
// defaultListSessions.
type sessionLister func(ctx context.Context, coreURL, token string) ([]api.Session, error)

// contextsProvider returns the stored login contexts and the active context
// name. Injected for testability; production wires auth.Contexts.
type contextsProvider func() ([]*contexts.Context, string, error)

// statusTarget is the resolved core to act against: the active context's
// CoreURL + its session token, or (no active context) the configured
// AuthBaseURL + legacy keyring entry. Shared by `auth status` (profile +
// session list) and `logout` (revocation) so both hit the same login server.
type statusTarget struct {
	coreURL       string
	token         string
	activeContext string // "" when falling back to the legacy entry
	totalContexts int
}

// resolveStatusTarget picks the core + token for `entire auth status`. The
// active contexts.json context wins (so `auth use` retargets status onto that
// login server); otherwise it falls back to the legacy keyring entry keyed by
// the configured auth host.
func resolveStatusTarget(store tokenStore, listContexts contextsProvider, fallbackBaseURL string) statusTarget {
	all, current, err := listContexts()
	total := 0
	if err == nil {
		total = len(all)
		for _, c := range all {
			if c.Name != current || c.CoreURL == "" {
				continue
			}
			if tok, terr := auth.LoginTokenForContext(c); terr == nil && tok != "" {
				return statusTarget{coreURL: c.CoreURL, token: tok, activeContext: c.Name, totalContexts: total}
			}
		}
	}
	tok, gerr := store.GetToken(fallbackBaseURL)
	if gerr != nil {
		tok = "" // best-effort: a keyring read failure just reads as "no token"
	}
	return statusTarget{coreURL: fallbackBaseURL, token: tok, totalContexts: total}
}

// defaultFetchProfile fetches a user's profile from coreURL's GET /me with the
// given bearer. It doubles as the liveness check for `entire auth status`: a
// 401 (or an expired login) means the token is no longer usable, which
// isKeychainTokenRejected maps to a re-login hint.
func defaultFetchProfile(ctx context.Context, coreURL, token string) (*authProfile, error) {
	client, err := coreapi.NewWithBearer(coreURL, token)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", coreURL, err)
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

// defaultListSessions lists the user's active login sessions on coreURL.
func defaultListSessions(ctx context.Context, coreURL, token string) ([]api.Session, error) {
	return newSessionsClient(coreURL, token).ListSessions(ctx) //nolint:wrapcheck // ListSessions already wraps with action context
}

// runAuthStatus reports auth state against the target core: GET /me validates
// the token and supplies the profile header, the active login context is shown
// locally, and the active sessions (refresh-token families) on that core are
// listed so the effect of `logout` / `logout --all` is visible.
func runAuthStatus(ctx context.Context, w io.Writer, fetchProfile profileFetcher, listSessions sessionLister, t statusTarget) error {
	if t.token == "" {
		fmt.Fprintf(w, "Not logged in to %s\n", t.coreURL)
		fmt.Fprintln(w, "Run 'entire login' to authenticate.")
		return nil
	}

	profile, err := fetchProfile(ctx, t.coreURL, t.token)
	if err != nil {
		if isKeychainTokenRejected(err) {
			fmt.Fprintf(w, "Login for %s is no longer valid.\n", t.coreURL)
			fmt.Fprintln(w, "Run 'entire login' to re-authenticate.")
			return nil
		}
		return fmt.Errorf("validate token: %w", err)
	}

	fmt.Fprintf(w, "Logged in to %s\n", t.coreURL)
	writeProfileLines(w, profile)
	if t.activeContext != "" {
		fmt.Fprintf(w, "  %-9s %s\n", "Context:", t.activeContext)
	}
	fmt.Fprintf(w, "  %-9s %s\n", "Token:", "stored in OS keychain")

	// Active sessions on this core. The token is already known good, so a
	// listing failure is non-fatal — note it and carry on.
	sessions, serr := listSessions(ctx, t.coreURL, t.token)
	switch {
	case serr != nil:
		fmt.Fprintf(w, "\n(could not list active sessions: %v)\n", serr)
	case len(sessions) > 0:
		sortSessionsByRecency(sessions)
		fmt.Fprintf(w, "\nActive sessions (%d):\n", len(sessions))
		renderSessionsTable(w, newAuthTableStyles(w), sessions)
		fmt.Fprintln(w, "\nRun 'entire logout' to end this session, or 'entire logout --all' to end all of them.")
	}

	if t.totalContexts > 1 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d login contexts saved; run 'entire auth contexts' to list or 'entire auth use <name>' to switch.\n", t.totalContexts)
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

// renderSessionsTable prints the active login sessions as an aligned table.
// No id column: there's no per-session CLI action (revoke-by-id is gone), so
// NAME/CREATED/LAST USED/EXPIRES is what's useful.
func renderSessionsTable(w io.Writer, sty authTableStyles, sessions []api.Session) {
	header := []string{
		sty.render(sty.header, "NAME"),
		sty.render(sty.header, "CREATED"),
		sty.render(sty.header, "LAST USED"),
		sty.render(sty.header, "EXPIRES"),
	}
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, []string{
			sty.render(sty.name, fallback(s.Name, placeholderDash)),
			sty.render(sty.value, formatAuthDate(s.CreatedAt)),
			sty.render(sty.value, formatLastUsed(s.LastUsedAt)),
			sty.render(sty.value, formatAuthDate(s.ExpiresAt)),
		})
	}
	renderAlignedTable(w, header, rows)
}

// sortSessionsByRecency orders sessions most-recently-used first, then most
// recently created, then by id — a fully specified order independent of the
// server's response ordering.
func sortSessionsByRecency(sessions []api.Session) {
	sort.Slice(sessions, func(i, j int) bool {
		li, lj := lastUsedSortKey(sessions[i]), lastUsedSortKey(sessions[j])
		if li != lj {
			return li > lj
		}
		if sessions[i].CreatedAt != sessions[j].CreatedAt {
			return sessions[i].CreatedAt > sessions[j].CreatedAt
		}
		return sessions[i].ID < sessions[j].ID
	})
}

func lastUsedSortKey(s api.Session) string {
	if s.LastUsedAt == nil {
		return ""
	}
	return *s.LastUsedAt
}

// formatAuthDate renders an RFC3339 timestamp as YYYY-MM-DD in local time,
// falling back to a dash (empty) or the raw value (unparseable).
func formatAuthDate(s string) string {
	if s == "" {
		return placeholderDash
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts.Local().Format("2006-01-02")
	}
	return s
}

func formatLastUsed(s *string) string {
	if s == nil || *s == "" {
		return lastUsedNever
	}
	return formatAuthDate(*s)
}
