package cli

import (
	"context"
	"encoding/json"
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
	"github.com/spf13/cobra"
)

// authTokenLister lists API tokens for the authenticated user. The
// implementation resolves its own data-API bearer via
// auth.TokenForResource (RFC 8693 exchange in split-host setups, same-
// host shortcut otherwise); callers don't pass a bearer through, which
// removes the temptation to forward the wrong-audience keyring token.
type authTokenLister func(ctx context.Context) ([]api.Token, error)

// authTokenRevoker revokes a single API token by id. Same bearer-
// resolution contract as authTokenLister.
type authTokenRevoker func(ctx context.Context, id string) error

// User-visible placeholder strings. Promoted to constants so tests and
// production share a single source of truth.
const (
	placeholderDash = "-"
	lastUsedNever   = "never"
	lastUsedJustNow = "just now"
)

// requireSecureBaseURL enforces TLS unless insecureHTTPAuth is set. Every
// command that sends a bearer token over the network (login, logout,
// auth status/list/revoke) must call this so credentials don't leak over
// plaintext HTTP without explicit opt-in.
//
// Both the auth and data API origins are checked: the bearer travels to the
// auth host for login + auth-token management, and to the data host for
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

// newAPITokensClient builds an api.Client for the auth-token management
// endpoints (list / revoke / current). API tokens live on the data API
// regardless of split-host config — the auth host (entire-core in v2)
// mints OAuth tokens but doesn't host application API token management
// endpoints — so this targets api.BaseURL().
//
// The supplied token must already be scoped for api.BaseURL(). Callers
// must obtain it via resolveDataAPIToken (or auth.TokenForResource
// directly) rather than handing through the raw keyring entry — the
// keyring stores the auth-host-issued core token, which the data API
// rejects in split-host setups.
func newAPITokensClient(token string) *api.Client {
	return api.NewClientWithBaseURL(token, api.BaseURL()).
		WithAuthTokensPath(auth.CurrentProvider().AuthTokensPath)
}

// resolveDataAPIToken returns a bearer scoped for the data API. In
// split-host setups this triggers an RFC 8693 exchange against the
// auth host's STS endpoint; in single-host setups the tokenmanager
// hits the same-host shortcut and returns the core token unchanged.
// Centralised so the audience-mismatch bug that motivated this fix
// can't be reintroduced piecemeal at individual call sites.
func resolveDataAPIToken(ctx context.Context) (string, error) {
	token, err := auth.TokenForResource(ctx, api.OriginOnly(api.BaseURL()))
	if err != nil {
		return "", fmt.Errorf("resolve API token: %w", err)
	}
	return token, nil
}

// isKeychainTokenRejected reports whether err indicates the stored
// keyring token can't authenticate against the data API. Three failure
// modes collapse into this single "the user must re-login" branch:
//
//   - data API returned 401 (single-host, or after a successful STS
//     exchange whose result the data API then rejected),
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
		Short: "Manage authentication and API tokens",
		Long:  "Authentication subcommands. Includes login, logout, status, listing tokens, and revoking tokens.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newAuthStatusCmd())
	cmd.AddCommand(newAuthListCmd())
	cmd.AddCommand(newAuthRevokeCmd())
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
				auth.NewContextStore(), defaultListTokens, api.AuthBaseURL())
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

func defaultListTokens(ctx context.Context) ([]api.Token, error) {
	token, err := resolveDataAPIToken(ctx)
	if err != nil {
		return nil, err
	}
	return newAPITokensClient(token).ListTokens(ctx) //nolint:wrapcheck // ListTokens already wraps with action context
}

func runAuthStatus(ctx context.Context, w io.Writer, store tokenStore, list authTokenLister, baseURL string) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		return fmt.Errorf("read keychain: %w", err)
	}
	if token == "" {
		fmt.Fprintf(w, "Not logged in to %s\n", baseURL)
		fmt.Fprintln(w, "Run 'entire login' to authenticate.")
		return nil
	}

	tokens, err := list(ctx)
	if err != nil {
		if isKeychainTokenRejected(err) {
			fmt.Fprintf(w, "Token in keychain for %s is no longer valid.\n", baseURL)
			fmt.Fprintln(w, "Run 'entire login' to re-authenticate.")
			return nil
		}
		return fmt.Errorf("validate token: %w", err)
	}

	fmt.Fprintf(w, "Logged in to %s\n", baseURL)
	fmt.Fprintln(w, "  Token: stored in OS keychain")
	fmt.Fprintf(w, "  Active tokens on this account: %d\n", len(tokens))
	return nil
}

// --- list -------------------------------------------------------------------

func newAuthListCmd() *cobra.Command {
	var jsonOut bool
	var insecureHTTPAuth bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active API tokens for the authenticated user",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			return runAuthList(cmd.Context(), cmd.OutOrStdout(),
				auth.NewContextStore(), defaultListTokens, api.AuthBaseURL(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print tokens as JSON")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

func runAuthList(ctx context.Context, w io.Writer, store tokenStore, list authTokenLister, baseURL string, jsonOut bool) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		return fmt.Errorf("read keychain: %w", err)
	}
	if token == "" {
		return fmt.Errorf("not logged in to %s; run 'entire login' first", baseURL)
	}

	tokens, err := list(ctx)
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(tokens); err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		return nil
	}

	if len(tokens) == 0 {
		fmt.Fprintln(w, "No active tokens.")
		return nil
	}

	// Deterministic order: most recently used first, then most recently
	// created, then by id as a final tie-breaker so the output is fully
	// specified regardless of the server's response order.
	sort.Slice(tokens, func(i, j int) bool {
		li := lastUsedSortKey(tokens[i])
		lj := lastUsedSortKey(tokens[j])
		if li != lj {
			return li > lj
		}
		if tokens[i].CreatedAt != tokens[j].CreatedAt {
			return tokens[i].CreatedAt > tokens[j].CreatedAt
		}
		return tokens[i].ID < tokens[j].ID
	})

	sty := newAuthListStyles(w)
	renderAuthListTable(w, sty, tokens, time.Now())
	return nil
}

// authListStyles holds the lipgloss styles for `entire auth list`. Mirrors the
// approach in activity_render.go: keep style construction tied to color
// detection, and render plain text when color is disabled.
type authListStyles struct {
	colorEnabled bool

	header  lipgloss.Style // bold + dim, used for column headers
	id      lipgloss.Style // yellow accent
	name    lipgloss.Style // bold
	value   lipgloss.Style // default fg for scope/dates (no color)
	dim     lipgloss.Style // "never", "-"
	warning lipgloss.Style // expires-soon
	expired lipgloss.Style // already expired
}

func newAuthListStyles(w io.Writer) authListStyles {
	useColor := shouldUseColor(w)
	s := authListStyles{colorEnabled: useColor}
	if !useColor {
		return s
	}
	s.header = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
	s.id = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	s.name = lipgloss.NewStyle().Bold(true)
	s.value = lipgloss.NewStyle() // default fg
	s.dim = lipgloss.NewStyle().Faint(true)
	s.warning = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	s.expired = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	return s
}

func (s authListStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

// renderAuthListTable prints a styled, column-aligned table of tokens. Column
// padding is computed via lipgloss.Width — it strips ANSI escapes, so a styled
// cell's visible width matches its plain text. tabwriter can't be used here
// once cells contain ANSI codes.
func renderAuthListTable(w io.Writer, sty authListStyles, tokens []api.Token, now time.Time) {
	headerCells := []string{"ID", "NAME", "SCOPE", "CREATED", "LAST USED", "EXPIRES"}
	header := make([]string, len(headerCells))
	for i, h := range headerCells {
		header[i] = sty.render(sty.header, h)
	}

	rows := make([][]string, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, []string{
			sty.render(sty.id, t.ID),
			styleName(sty, t.Name),
			sty.render(sty.value, fallback(t.Scope, placeholderDash)),
			sty.render(sty.value, formatAuthDate(t.CreatedAt)),
			styleLastUsed(sty, t.LastUsedAt, now),
			styleExpires(sty, t.ExpiresAt, now),
		})
	}

	widths := make([]int, len(headerCells))
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

func styleName(sty authListStyles, name string) string {
	if name == "" {
		return sty.render(sty.dim, placeholderDash)
	}
	return sty.render(sty.name, name)
}

func styleLastUsed(sty authListStyles, lastUsed *string, now time.Time) string {
	if lastUsed == nil {
		return sty.render(sty.dim, lastUsedNever)
	}
	return sty.render(sty.value, formatAuthLastUsed(lastUsed, now))
}

func styleExpires(sty authListStyles, expiresAt string, now time.Time) string {
	formatted := formatAuthDate(expiresAt)
	switch classifyExpiresAt(expiresAt, now) {
	case expiresExpired:
		return sty.render(sty.expired, formatted)
	case expiresSoon:
		return sty.render(sty.warning, formatted)
	case expiresNormal:
		return sty.render(sty.value, formatted)
	}
	return sty.render(sty.value, formatted)
}

func lastUsedSortKey(t api.Token) string {
	if t.LastUsedAt == nil {
		return ""
	}
	return *t.LastUsedAt
}

// formatAuthDate renders an RFC3339 timestamp as YYYY-MM-DD in local time.
func formatAuthDate(s string) string {
	if s == "" {
		return placeholderDash
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts.Local().Format("2006-01-02")
	}
	return s
}

// formatAuthLastUsed renders a relative "last used" timestamp, with "yesterday"
// and absolute-date branches that the shared formatRelativeDuration helper
// doesn't cover.
func formatAuthLastUsed(s *string, now time.Time) string {
	if s == nil || *s == "" {
		return lastUsedNever
	}
	ts, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return *s
	}
	delta := now.Sub(ts)
	switch {
	case delta < 0, delta >= 30*24*time.Hour:
		return ts.Local().Format("2006-01-02")
	case delta >= 24*time.Hour && delta < 48*time.Hour:
		return "yesterday"
	default:
		return formatRelativeDuration(delta)
	}
}

type expiresState int

const (
	expiresNormal expiresState = iota
	expiresSoon
	expiresExpired
)

// classifyExpiresAt classifies an RFC3339 expires-at relative to now. Used to
// color the EXPIRES column so tokens worth rotating stand out.
func classifyExpiresAt(s string, now time.Time) expiresState {
	if s == "" {
		return expiresNormal
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return expiresNormal
	}
	delta := ts.Sub(now)
	switch {
	case delta <= 0:
		return expiresExpired
	case delta < 7*24*time.Hour:
		return expiresSoon
	default:
		return expiresNormal
	}
}

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

// --- revoke -----------------------------------------------------------------

func newAuthRevokeCmd() *cobra.Command {
	var revokeCurrent bool
	var insecureHTTPAuth bool
	cmd := &cobra.Command{
		Use:   "revoke [id]",
		Short: "Revoke an API token by id",
		Long:  "Revoke a specific API token. Use --current to revoke the token used by this CLI (equivalent to 'entire logout').",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			if id == "" && !revokeCurrent {
				return cmd.Help()
			}
			if id != "" && revokeCurrent {
				return errors.New("cannot use both <id> and --current")
			}
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			return runAuthRevoke(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				auth.NewContextStore(), defaultListTokens, defaultRevokeTokenByID, defaultRevokeCurrentToken,
				auth.RemoveCurrentContext, api.AuthBaseURL(), id, revokeCurrent)
		},
	}
	cmd.Flags().BoolVar(&revokeCurrent, "current", false, "Revoke the token used by this CLI and remove the local copy")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

func defaultRevokeTokenByID(ctx context.Context, id string) error {
	token, err := resolveDataAPIToken(ctx)
	if err != nil {
		return err
	}
	return newAPITokensClient(token).RevokeToken(ctx, id) //nolint:wrapcheck // RevokeToken already wraps with action context
}

func runAuthRevoke(
	ctx context.Context,
	outW, errW io.Writer,
	store tokenStore,
	list authTokenLister,
	revokeByID authTokenRevoker,
	revokeCurrent revokeCurrentFunc,
	clearContext clearContextFunc,
	baseURL, id string,
	current bool,
) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		return fmt.Errorf("read keychain: %w", err)
	}
	if token == "" {
		return fmt.Errorf("not logged in to %s; run 'entire login' first", baseURL)
	}

	if current {
		// Revoking our own token is just logout — reuse that path so behavior
		// stays identical (best-effort revoke + local delete + context clear).
		return runLogout(ctx, outW, errW, store, revokeCurrent, clearContext, baseURL)
	}

	if err := revokeByID(ctx, id); err != nil {
		return err
	}

	// The list endpoint requires bearer auth, so a 401 here means the id we
	// just revoked was the same one this CLI is using — the local copy is now
	// stale and would otherwise produce confusing 401s on every command, so
	// remove both the legacy keyring entry and the active context.
	if _, listErr := list(ctx); listErr != nil && api.IsHTTPErrorStatus(listErr, http.StatusUnauthorized) {
		if delErr := store.DeleteToken(baseURL); delErr != nil {
			return fmt.Errorf("revoked token %s but failed to remove local copy: %w", id, delErr)
		}
		if ctxErr := clearContext(); ctxErr != nil {
			fmt.Fprintf(errW, "Warning: revoked token %s but failed to clear current context: %v\n", id, ctxErr)
		}
		fmt.Fprintf(outW, "Revoked token %s (this was your local token; removed from keychain).\n", id)
		return nil
	}

	fmt.Fprintf(outW, "Revoked token %s.\n", id)
	return nil
}
