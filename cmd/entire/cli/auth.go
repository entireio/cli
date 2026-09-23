package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/spf13/cobra"
)

// coreAuthSessionsPath is entire-core's login-session endpoint family
// (list / revoke / current) on the auth host. Sessions are OAuth
// refresh-token families; the CLI authenticates against them with its core
// JWT. Session management must target the auth host (entire-core), never the
// data host.
const coreAuthSessionsPath = "/api/auth/tokens"

// authTokenRowLabel labels the `auth status` row naming where the bearer comes
// from. Two branches render it — a stored credential and ENTIRE_TOKEN — so it
// is named once rather than spelled at each.
const authTokenRowLabel = "token"

// activeSessionsRowLabel labels the session-count row. It says "active" to
// match the section heading and `logout --everywhere`, which both describe the
// same set.
const activeSessionsRowLabel = "active sessions"

// availableContextsRowLabel labels the saved-login count. "available" rather
// than "login", because the row sits beside `context` (the one in use) and the
// pair reads as "this one, of that many".
const availableContextsRowLabel = "available contexts"

// User-visible placeholder strings. lastUsedJustNow is consumed by
// formatRelativeDuration in status.go.
const (
	placeholderDash = "-"
	lastUsedNever   = "never"
	lastUsedJustNow = "just now"
)

// applyInsecureHTTPAuth relaxes the tokenmanager's HTTP guard when the user
// passed --insecure-http-auth, and reports whether per-target TLS checks
// should be skipped. status/logout enforce TLS on the specific cores they
// dial, not on any global origin.
func applyInsecureHTTPAuth(insecureHTTPAuth bool) bool {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
	}
	return insecureHTTPAuth
}

// newAuthSessionsClient builds an api.Client for entire-core's login-session
// endpoints (coreAuthSessionsPath) on coreURL, authenticated with the
// session-scoped login JWT. coreURL is the active context's CoreURL (or the
// configured auth host when no context is active) — session management always
// targets a login server, never the data host.
func newAuthSessionsClient(coreURL, token string) *api.Client {
	return api.NewClientWithBaseURL(token, coreURL).WithAuthSessionsPath(coreAuthSessionsPath)
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
		Long:  "Authentication subcommands. Includes login, logout, status, and login-context management (contexts, switch).",
	}
	requireSubcommand(cmd)

	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newAuthStatusCmd())
	cmd.AddCommand(newAuthTokenCmd())
	cmd.AddCommand(newAuthContextsCmd())
	cmd.AddCommand(newAuthSwitchCmd())
	return cmd
}

// --- token ------------------------------------------------------------------

// newAuthTokenCmd prints an Entire bearer to stdout for scripting. By default
// that's the active control-plane bearer (resolved the same way the API client's
// is: ENTIRE_TOKEN verbatim when set, otherwise the active context's login JWT,
// refreshed if near expiry); with --jurisdiction it mints a data-plane cell
// identity token for that jurisdiction instead. The user-facing Long and Example
// carry the detail and the "treat the output as a secret" caveat; only the token
// is printed — errors and the not-logged-in hint go to stderr so command
// substitution stays clean.
func newAuthTokenCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var jurisdiction string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print an Entire bearer token — a live credential, treat as a secret",
		Long: "Print an Entire bearer token to stdout so scripts and ad-hoc curl can\n" +
			"authenticate without plumbing auth themselves.\n\n" +
			"By default it prints the control-plane bearer: the same one the API client\n" +
			"uses (ENTIRE_TOKEN verbatim when set, otherwise the active context's login\n" +
			"JWT, refreshed if near expiry), for the control-plane API (orgs, repos,\n" +
			"clusters, /me).\n\n" +
			"With --jurisdiction <slug> it instead mints a jurisdictional identity token\n" +
			"for that jurisdiction's entire-api cells (e.g.\n" +
			"https://aws-us-east-2.api.entire.io/api/v1), which reject the control-plane\n" +
			"bearer. The slug is a jurisdiction like 'us' or 'eu' (find yours with\n" +
			"'entire auth status'); the token works against any cell in that\n" +
			"jurisdiction. It is minted by exchanging your login (or ENTIRE_TOKEN, when\n" +
			"set) for the jurisdiction's audience.\n\n" +
			"The output is a live credential — treat it as a secret. Only the token is\n" +
			"printed to stdout; errors and the not-logged-in hint go to stderr so command\n" +
			"substitution stays clean.",
		Example: "  curl -H \"Authorization: Bearer $(entire auth token)\" \"https://us.console.entire.io/api/v1/clusters\"\n" +
			"  curl -H \"Authorization: Bearer $(entire auth token --jurisdiction us)\" \"https://aws-us-east-2.api.entire.io/api/v1/me/activity\"",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Refresh may exchange/refresh over the network; honor the
			// plain-HTTP opt-in before resolving so local dev cores work.
			insecure := applyInsecureHTTPAuth(insecureHTTPAuth)

			// --jurisdiction mints a data-plane cell identity token instead of the
			// control-plane bearer. JurisdictionToken performs its own TLS/exchange
			// guards and returns context-rich errors.
			if strings.TrimSpace(jurisdiction) != "" {
				token, err := auth.JurisdictionToken(cmd.Context(), insecure, jurisdiction)
				if err != nil {
					cmd.SilenceUsage = true
					if errors.Is(err, auth.ErrNotLoggedIn) {
						fmt.Fprintln(cmd.ErrOrStderr(), "Not logged in. Run 'entire login' to authenticate.")
						return NewSilentError(err)
					}
					return err //nolint:wrapcheck // JurisdictionToken already returns contextual auth errors
				}
				fmt.Fprintln(cmd.OutOrStdout(), token)
				return nil
			}

			target, err := resolveAuthStatusTarget(cmd.Context(), auth.Contexts, auth.RefreshedLoginToken)
			if err != nil {
				return err
			}
			// Don't mint/print a bearer for an insecure core unless explicitly
			// opted in — the token would otherwise be usable over plain HTTP.
			// Mirrors `auth status`.
			if !insecure && target.coreURL != "" {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					cmd.SilenceUsage = true
					return fmt.Errorf("login server URL check: %w", err)
				}
			}
			if target.token == "" {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not logged in. Run 'entire login' to authenticate.")
				return NewSilentError(errors.New("not logged in"))
			}
			// Stderr, so $(entire auth token) stays clean.
			auth.AnnounceContext(target.totalContexts, target.activeContext)
			fmt.Fprintln(cmd.OutOrStdout(), target.token)
			return nil
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	cmd.Flags().StringVarP(&jurisdiction, "jurisdiction", "j", "", "mint a jurisdictional identity token for this jurisdiction slug (e.g. us, eu) for use against that jurisdiction's entire-api cells")
	return cmd
}

// --- status -----------------------------------------------------------------

func newAuthStatusCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var showSessions bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   cmdStatus,
		Short: "Show authentication status",
		// `auth status sessions` is a natural guess now that --sessions
		// exists; without this it prints the collapsed default and drops the
		// word, which reads as the flag having no effect.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := resolveAuthStatusTarget(cmd.Context(), auth.Contexts, auth.RefreshedLoginToken)
			if err != nil {
				return err
			}
			// We send the session token to target.coreURL; enforce TLS on it.
			if !applyInsecureHTTPAuth(insecureHTTPAuth) && target.coreURL != "" {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					return fmt.Errorf("context login server URL check: %w", err)
				}
			}
			opts := authStatusOptions{Sessions: showSessions, JSON: asJSON}
			return runAuthStatus(cmd.Context(), cmd.OutOrStdout(), defaultFetchProfile, defaultListAuthSessions, target, opts)
		},
	}
	cmd.Flags().BoolVar(&showSessions, "sessions", false, "List every active login session")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
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
	// Jurisdiction is the caller's home jurisdiction slug (e.g. "eu"), used to
	// pick the default mirror cluster for that jurisdiction. May be empty.
	//
	// This comes from /me's global.homeJurisdiction (the account's own home),
	// NOT the top-level jurisdiction field, which is the jurisdiction of
	// whichever core answered. The two differ whenever a login was dispatched
	// to a non-home region.
	Jurisdiction string
	// ForeignRegion is true when the core that served /me is not the
	// account's home region — /me signals this with a regionalUnavailable
	// block. It reaches `auth status --json` as foreign_region and is not
	// rendered in the text view; the display name and email that core withholds
	// are read by setup_identity, which is not this view.
	ForeignRegion bool
}

// profileFetcher fetches a user's profile via GET /me on coreURL, authenticated
// with token. Injected so status stays unit-testable without a live core.
type profileFetcher func(ctx context.Context, coreURL, token string) (*authProfile, error)

// authSessionLister lists the active login sessions on coreURL (the user's
// refresh-token families). Injected for testability; production wires
// defaultListAuthSessions.
type authSessionLister func(ctx context.Context, coreURL, token string) ([]api.AuthSession, error)

// contextsProvider returns the stored login contexts and the active context
// name. Injected for testability; production wires auth.Contexts.
type contextsProvider func() ([]*contexts.Context, string, error)

// loginTokenResolver returns a usable login JWT for a context, transparently
// re-minting an expired one from the stored refresh token. Injected so status
// tests don't reach the network; production wires auth.RefreshedLoginToken.
type loginTokenResolver func(ctx context.Context, c *contexts.Context) (string, error)

// statusTarget is the core `auth status` reports on: the active
// context's CoreURL + its session token. Zero coreURL/token means not
// logged in.
//
// envToken marks the target as resolved from ENTIRE_TOKEN rather than a stored
// context: the bearer is the env token itself, sent verbatim to its own aud,
// and there is no stored session to manage — so status renders it without the
// context/keychain/session lines.
type statusTarget struct {
	coreURL       string
	token         string
	activeContext string
	totalContexts int
	// distinctServers counts the login servers the saved contexts are spread
	// across. It is what decides whether naming the host tells the reader
	// anything: with every login on one server the host is the same fact
	// repeated, and only a split across servers makes it the thing that
	// separates one login from another.
	distinctServers int
	envToken        bool
}

// resolveAuthStatusTarget picks the target for `entire auth status`, honouring
// ENTIRE_TOKEN: when it is set the request dials the token's own aud (exactly
// as coreapi.New does), so status must report that core, not a stored context
// that the request never touches. `logout` does not use this: it sweeps
// every stored login, and an ephemeral env token is not one of them.
func resolveAuthStatusTarget(ctx context.Context, listContexts contextsProvider, resolveLogin loginTokenResolver) (statusTarget, error) {
	if raw, ok := os.LookupEnv(auth.EnvTokenVar); ok {
		return resolveEnvTokenStatusTarget(raw)
	}
	return resolveStatusTarget(ctx, listContexts, resolveLogin)
}

// resolveEnvTokenStatusTarget builds the status target from ENTIRE_TOKEN via the
// shared auth.ParseEnvToken — the same trim/blank/aud validation coreapi.New
// applies — so status reports exactly the core a request would dial. The token
// is the bearer; fail-closed (a blank or malformed value errors, never falls
// back to a stored context).
func resolveEnvTokenStatusTarget(raw string) (statusTarget, error) {
	coreURL, token, err := auth.ParseEnvToken(raw)
	if err != nil {
		return statusTarget{}, err //nolint:wrapcheck // auth.ParseEnvToken already prefixes with EnvTokenVar
	}
	return statusTarget{coreURL: coreURL, token: token, envToken: true}, nil
}

// resolveStatusTarget picks the core + token for `entire auth status` from
// the active contexts.json context (so `auth switch` retargets status onto
// that login server). No active context means not logged in — the
// zero-token target renders the `entire login` hint.
//
// The token is resolved through resolveLogin, which transparently re-mints
// an expired login JWT from the stored refresh token: an
// expired-but-refreshable session must report "logged in", not "re-login".
// When refresh fails (revoked family, network, opaque token), the raw stored
// token is used and the /me liveness probe is the arbiter — preserving the
// accurate "no longer valid" outcome for a genuinely dead session.
//
// A genuine contexts.json read/parse error is surfaced, not swallowed — a
// missing file reads as "no contexts" (no error), so an error here means the
// file is corrupt or unreadable, which the user must see.
func resolveStatusTarget(ctx context.Context, listContexts contextsProvider, resolveLogin loginTokenResolver) (statusTarget, error) {
	all, current, err := listContexts()
	if err != nil {
		// A bad --context/$ENTIRE_CONTEXT is not a load failure, and prefixing it
		// with one ("load contexts: --context selected …") describes the wrong
		// problem. Its own message is already complete.
		var unknown *contexts.UnknownContextError
		if errors.As(err, &unknown) {
			return statusTarget{}, err
		}
		return statusTarget{}, fmt.Errorf("load contexts: %w", err)
	}
	total := len(all)
	servers := make(map[string]struct{}, total)
	for _, c := range all {
		if host := authServerHost(c.CoreURL); host != "" {
			servers[host] = struct{}{}
		}
	}
	distinct := len(servers)
	for _, c := range all {
		if c.Name != current || c.CoreURL == "" {
			continue
		}
		if tok, terr := resolveLogin(ctx, c); terr == nil && tok != "" {
			return statusTarget{coreURL: c.CoreURL, token: tok, activeContext: c.Name, totalContexts: total, distinctServers: distinct}, nil
		}
		if tok, terr := auth.LoginTokenForContext(c); terr == nil && tok != "" {
			return statusTarget{coreURL: c.CoreURL, token: tok, activeContext: c.Name, totalContexts: total, distinctServers: distinct}, nil
		}
		// Active context with no readable token: report against its core so
		// the not-logged-in message names the right login server.
		return statusTarget{coreURL: c.CoreURL, activeContext: c.Name, totalContexts: total, distinctServers: distinct}, nil
	}
	return statusTarget{totalContexts: total, distinctServers: distinct}, nil
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
	// global.homeJurisdiction is the account's own home region. The top-level
	// me.Jurisdiction field is the serving node's region — reading that one is
	// what used to make a geo-routed login report the wrong slug.
	p.Jurisdiction, _ = me.Global.HomeJurisdiction.Get()
	if ru, ok := me.RegionalUnavailable.Get(); ok {
		p.ForeignRegion = true
		// The block also carries a homeCoreUrl and a ready-made message, both
		// deliberately ignored: that URL points at a console SPA that was
		// retired (a bare "/" now redirects by role, so it lands on the admin
		// bundle or the marketing site, never a profile page), and the message
		// tells the user to open it. Take only the slug, which is sound.
		if home := strings.TrimSpace(ru.Jurisdiction); home != "" && p.Jurisdiction == "" {
			p.Jurisdiction = home
		}
	}
	if reg, ok := me.Regional.Get(); ok {
		p.DisplayName, _ = reg.DisplayName.Get()
		p.Email, _ = reg.Email.Get()
	}
	return p, nil
}

// defaultListAuthSessions lists the user's active login sessions on coreURL.
func defaultListAuthSessions(ctx context.Context, coreURL, token string) ([]api.AuthSession, error) {
	return newAuthSessionsClient(coreURL, token).ListAuthSessions(ctx) //nolint:wrapcheck // ListAuthSessions already wraps with action context
}

// authStatusOptions selects what `entire auth status` renders. A struct rather
// than positional bools so a call site says which view it wants.
type authStatusOptions struct {
	// Sessions expands the session count into the full listing.
	Sessions bool
	// JSON emits the machine-readable envelope instead of the styled block.
	JSON bool
}

// authStatusData is everything `auth status` resolved, independent of how it is
// rendered. The text and JSON views both derive from this one value, so the two
// cannot drift into disagreeing about the same login.
type authStatusData struct {
	target statusTarget
	// loggedIn is false both when there is no token and when the core rejected
	// the one we have; invalid distinguishes the two.
	loggedIn bool
	invalid  bool
	profile  *authProfile
	sessions []api.AuthSession
	// sessionErr is non-fatal: the token is already known good, so a listing
	// failure is reported rather than raised.
	sessionErr error
	// current indexes sessions, or -1 when the caller could not be identified.
	current int
	// revoked means the login was ended somewhere else and cannot be renewed:
	// the caller holds a working bearer for a session that is no longer listed.
	revoked bool
	// tokenExpiry is when the bearer in hand lapses. Only meaningful alongside
	// revoked: normally the token is renewed long before this and the number
	// would say nothing about being logged out.
	tokenExpiry time.Time
}

// resolveAuthStatus reports auth state against the target core: GET /me
// validates the token and supplies the identity, and the active sessions
// (refresh-token families) on that core are listed so the effect of `logout` /
// `logout --everywhere` is visible.
//
// The listing is fetched whether or not the caller asked for it, because even
// the collapsed view needs the count and the current session's expiry.
func resolveAuthStatus(ctx context.Context, fetchProfile profileFetcher, listSessions authSessionLister, t statusTarget) (authStatusData, error) {
	d := authStatusData{target: t, current: -1}
	if t.token == "" {
		return d, nil
	}

	profile, err := fetchProfile(ctx, t.coreURL, t.token)
	if err != nil {
		if isKeychainTokenRejected(err) {
			d.invalid = true
			return d, nil
		}
		return d, fmt.Errorf("validate token: %w", err)
	}

	// Last resort for the home jurisdiction: the login token carries it as a
	// home_jurisdiction claim, and that claim is what jurisdictional calls
	// route on (see auth.HomeJurisdictionFromLoginJWT). /me's
	// global.homeJurisdiction is authoritative and normally wins; this covers a
	// core too old to send it.
	if profile.Jurisdiction == "" {
		if juris, jerr := auth.HomeJurisdictionFromLoginJWT(t.token); jerr == nil && juris != "" {
			profile.Jurisdiction = juris
		}
	}
	d.loggedIn = true
	d.profile = profile

	// ENTIRE_TOKEN mode: the bearer is the env var itself, with no stored
	// context, keychain slot, or revocable session family behind it. There is
	// nothing to list and no expiry to report.
	if t.envToken {
		return d, nil
	}

	d.sessions, d.sessionErr = listSessions(ctx, t.coreURL, t.token)
	if d.sessionErr == nil {
		sortAuthSessionsByRecency(d.sessions)
		d.current = currentSessionIndex(t.token, d.sessions)
		d.revoked, d.tokenExpiry = detectRevokedLogin(t.token, d.sessions, d.current)
	}
	return d, nil
}

// detectRevokedLogin reports whether this login was ended elsewhere, and when
// the bearer in hand lapses.
//
// /me accepted the token moments ago, so the bearer is live; what is gone is
// the session behind it. An access token outlives its family's revocation by
// its own lifetime, so the login keeps working for minutes and then stops with
// nothing to renew it — which is worth saying, since "Logged in" is true and
// about to silently become false.
//
// Both signals are required. A fid naming no listed session could be a
// truncated listing; a failed refresh could be a network blip. Neither alone
// earns a claim this alarming. A token with no fid claim at all (a core too old
// to mint one) is not evidence of anything and stays quiet.
func detectRevokedLogin(token string, sessions []api.AuthSession, current int) (bool, time.Time) {
	if current >= 0 {
		return false, time.Time{}
	}
	// An unreadable expiry is fine: the claim is about the session being gone,
	// and the deadline only sharpens it. A zero time renders no "expires".
	expiry, err := auth.LoginTokenExpiry(token)
	if err != nil {
		expiry = time.Time{}
	}

	// An empty listing settles it alone. The endpoint includes the caller's own
	// session — that is how a matched fid finds itself — so none listed means
	// none exist, the caller's included. No truncation explains zero.
	if len(sessions) == 0 {
		return true, expiry
	}

	// With sessions listed but the caller's absent, absence is the only
	// evidence and a short listing could in principle explain it, so require a
	// fid to have actually named something. A core too old to mint one tells us
	// nothing and stays quiet.
	fid, ferr := auth.SessionFamilyIDFromLoginJWT(token)
	if ferr != nil || fid == "" {
		return false, time.Time{}
	}
	return true, expiry
}

func runAuthStatus(ctx context.Context, w io.Writer, fetchProfile profileFetcher, listSessions authSessionLister, t statusTarget, opts authStatusOptions) error {
	d, err := resolveAuthStatus(ctx, fetchProfile, listSessions, t)
	if err != nil {
		if !opts.JSON {
			return err
		}
		// A caller that asked for JSON gets a parseable object naming the
		// failure rather than empty stdout — `--json | jq .logged_in` should
		// report false, not fail to parse. The exit stays non-zero, and the
		// reason is already on stdout, so the error itself is silent.
		if perr := printJSON(w, authStatusJSON{Error: err.Error(), Server: authServerHost(t.coreURL)}); perr != nil {
			return perr
		}
		return NewSilentError(err)
	}
	if opts.JSON {
		return printJSON(w, buildAuthStatusJSON(d, opts))
	}
	writeAuthStatusText(w, d, opts)
	return nil
}

// writeAuthStatusText renders the human view: a verdict line, an aligned
// label/value block, and — when asked — the session table.
func writeAuthStatusText(w io.Writer, d authStatusData, opts authStatusOptions) {
	sty := newStatusStyles(w)
	t := d.target

	if !d.loggedIn {
		if d.invalid {
			fmt.Fprintln(w, sty.render(sty.red, "✕")+" "+sty.render(sty.bold, "Login for "+authServerHost(t.coreURL)+" is no longer valid"))
			fmt.Fprintln(w, sty.render(sty.dim, "Run 'entire login' to re-authenticate."))
			return
		}
		headline := "Not logged in"
		if host := authServerHost(t.coreURL); host != "" {
			// Naming the server is the whole point of this message: the user
			// may well be logged in to a different one. Bare host, as every
			// other row spells it.
			headline += " to " + host
		}
		fmt.Fprintln(w, sty.render(sty.red, "○")+" "+sty.render(sty.bold, headline))
		fmt.Fprintln(w, sty.render(sty.dim, "Run 'entire login' to authenticate."))
		return
	}

	if t.envToken {
		fmt.Fprintln(w, sty.render(sty.green, "●")+" "+sty.render(sty.bold, "Logged in"))
		fmt.Fprintln(w)
		rows := authProfileRows(d.profile)
		// With no context to name the server, say it outright — otherwise
		// env-token mode names no server anywhere.
		rows = append(rows,
			explainRow{Label: "server", Value: authServerHost(t.coreURL)},
			explainRow{Label: authTokenRowLabel, Value: auth.EnvTokenVar + " environment variable"},
		)
		fmt.Fprint(w, sty.metadataRows(rows))
		return
	}

	// The verdict line answers both halves of "what is my login doing": am I
	// in, and for how much longer. The expiry is the current session family's,
	// so it is stated only when that session was actually identified — showing
	// some other session's expiry as yours would be worse than showing none.
	headline := sty.render(sty.green, "●") + " " + sty.render(sty.bold, "Logged in")
	var deadline string
	switch {
	case d.current >= 0:
		deadline = authDeadlineClause(parseAuthTimestamp(d.sessions[d.current].ExpiresAt))
	case d.revoked:
		// The bearer's own expiry, not a session's. Here they mean the same
		// thing — there is nothing left to renew it, so this is when the user
		// is logged out.
		deadline = authDeadlineClause(d.tokenExpiry)
	}
	if deadline != "" {
		headline += sty.render(sty.dim, " · ") + deadline
	}
	fmt.Fprintln(w, headline)
	if d.revoked {
		fmt.Fprintln(w, sty.render(sty.yellow, "  ! this login was ended elsewhere and cannot be renewed")+
			sty.render(sty.dim, " · run 'entire login'"))
	}
	fmt.Fprintln(w)

	rows := authProfileRows(d.profile)
	// Both context rows are a way to say "this login, not the others", so both
	// wait until there are others. With a sole login there is nothing to
	// distinguish it from, and naming it describes a choice the user does not
	// have.
	if t.totalContexts > 1 {
		if t.activeContext != "" {
			rows = append(rows, authContextRow(sty, t.activeContext, t.coreURL, t.distinctServers > 1))
		}
		rows = append(rows, authContextsCountRow(sty, t.totalContexts))
	}
	rows = append(rows, explainRow{Label: authTokenRowLabel, Value: tokenstore.BackendDescription()})
	if row, ok := authSessionsRow(sty, d.sessions, d.sessionErr, opts.Sessions, d.current, deadline != ""); ok {
		rows = append(rows, row)
	}
	fmt.Fprint(w, sty.metadataRows(rows))

	if opts.Sessions && d.sessionErr == nil && len(d.sessions) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, sty.sectionRule("Active Sessions", sty.width))
		fmt.Fprintln(w)
		renderAuthSessionsTable(w, newAuthTableStyles(w), d.sessions, d.current)
		fmt.Fprintln(w, sty.horizontalRule(sty.width))
		fmt.Fprintln(w, sty.render(sty.dim, fmt.Sprintf("%d %s", len(d.sessions), pluralize("session", len(d.sessions)))))
	}

	// Nothing to offer when the caller's own session is already gone: the
	// sessions that are listed belong to the login that replaced this one, so
	// there is nothing here worth ending. The banner's `entire login` is the
	// action.
	if d.sessionErr == nil && len(d.sessions) > 0 && !d.revoked {
		// --everywhere is offered only alongside the table. It ends every
		// session at once — browser logins included — and in the collapsed view
		// those sessions are a count the reader cannot inspect. The count row
		// already says how to bring them on screen; once they are, the bulk
		// action arrives with its subject attached.
		hint := "Run 'entire logout' to end every CLI session."
		if opts.Sessions && len(d.sessions) > 1 {
			// No count: logout sweeps every saved login on every login server,
			// while these rows are one server's. Naming the smaller number
			// beside the wider command understates what it destroys.
			hint += " Add --everywhere to end browser and web sessions too."
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, hint)
	}
}

// authStatusJSON is the `entire auth status --json` envelope.
//
// Timestamps stay in the wire's RFC3339 rather than the relative form the text
// view shows: the humanised "in 30d" is a reading aid, and a machine reader
// wants the instant.
type authStatusJSON struct {
	LoggedIn bool `json:"logged_in"`
	// Error names a non-fatal condition that stopped a full answer, in-band
	// rather than as an exit code, so a script always gets a parseable object.
	Error string `json:"error,omitempty"`
	// Server is the login server's bare host; Context is the local name for it.
	Server  string `json:"server,omitempty"`
	Context string `json:"context,omitempty"`
	// User is the provider-qualified handle, the spelling `entire grant` takes.
	// The bare handle and provider are deliberately not split out: one field
	// that is directly usable beats two a caller has to rejoin.
	User          string `json:"user,omitempty"`
	Jurisdiction  string `json:"jurisdiction,omitempty"`
	ForeignRegion bool   `json:"foreign_region,omitempty"`
	// TokenSource is the same description the text view prints; EnvToken is the
	// field to branch on, since an env bearer has no revocable session.
	TokenSource string `json:"token_source,omitempty"`
	EnvToken    bool   `json:"env_token,omitempty"`
	// CurrentSessionID and ExpiresAt are present only when the caller's own
	// session was identified; absent means unidentified, never "no expiry".
	CurrentSessionID string `json:"current_session_id,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	// LoginRevoked reports a login ended elsewhere: the bearer still works but
	// nothing can renew it, so the caller is logged out when TokenExpiresAt
	// passes. Absent unless established.
	LoginRevoked   bool   `json:"login_revoked,omitempty"`
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
	// ActiveSessions is a pointer so a failed listing is absent rather than
	// reported as zero sessions.
	ActiveSessions *int   `json:"active_sessions,omitempty"`
	SessionsError  string `json:"sessions_error,omitempty"`
	// Sessions is populated only when --sessions was passed, mirroring the text
	// view; ActiveSessions is the authoritative count either way. It is a
	// pointer so that asking for the list and getting none emits an explicit
	// [], distinguishable from the collapsed default where the key is absent.
	Sessions *[]authSessionJSON `json:"sessions,omitempty"`
	// AvailableContexts is a pointer for the same reason ActiveSessions is: a
	// genuine zero must be emitted, while "not counted" (ENTIRE_TOKEN mode
	// never reads contexts.json) must be absent.
	AvailableContexts *int `json:"available_contexts,omitempty"`
}

// authSessionJSON is one login session (an OAuth refresh-token family).
type authSessionJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Scope      string `json:"scope,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	// Current marks the session this command is running under.
	Current bool `json:"current"`
}

func buildAuthStatusJSON(d authStatusData, opts authStatusOptions) authStatusJSON {
	t := d.target
	out := authStatusJSON{
		LoggedIn: d.loggedIn,
		Server:   authServerHost(t.coreURL),
		Context:  t.activeContext,
	}
	// ENTIRE_TOKEN mode never reads contexts.json, so its zero means "not
	// counted" rather than "none saved"; every other path counted for real,
	// zero included.
	if !t.envToken {
		total := t.totalContexts
		out.AvailableContexts = &total
	}
	// Where a bearer came from is settled before /me is consulted, so it is
	// reported whatever /me said of it. A script told only
	// `{"logged_in":false}` has no way to see that ENTIRE_TOKEN supplied the
	// rejected token and is still winning over every stored context — which is
	// also why the text view's "run entire login" cannot help there.
	//
	// Gated on a bearer actually existing: with no token there is no source to
	// name, and naming the keychain would assert a token is filed there.
	if t.token != "" {
		if t.envToken {
			out.EnvToken = true
			out.TokenSource = auth.EnvTokenVar + " environment variable"
		} else {
			out.TokenSource = tokenstore.BackendDescription()
		}
	}
	if d.invalid {
		out.Error = "login is no longer valid; run 'entire login' to re-authenticate"
	}
	if !d.loggedIn {
		return out
	}

	out.ForeignRegion = d.profile.ForeignRegion
	out.Jurisdiction = d.profile.Jurisdiction
	out.User = authIdentityLabel(d.profile)

	if t.envToken {
		return out
	}

	if d.sessionErr != nil {
		out.SessionsError = d.sessionErr.Error()
		return out
	}
	if d.revoked {
		out.LoginRevoked = true
		if !d.tokenExpiry.IsZero() {
			out.TokenExpiresAt = d.tokenExpiry.UTC().Format(time.RFC3339)
		}
	}
	count := len(d.sessions)
	out.ActiveSessions = &count
	if d.current >= 0 {
		out.CurrentSessionID = d.sessions[d.current].ID
		out.ExpiresAt = d.sessions[d.current].ExpiresAt
	}
	if opts.Sessions {
		listed := make([]authSessionJSON, 0, len(d.sessions))
		for i, sess := range d.sessions {
			row := authSessionJSON{
				ID:        sess.ID,
				Name:      sess.Name,
				Scope:     sess.Scope,
				CreatedAt: sess.CreatedAt,
				ExpiresAt: sess.ExpiresAt,
				Current:   i == d.current,
			}
			if sess.LastUsedAt != nil {
				row.LastUsedAt = *sess.LastUsedAt
			}
			listed = append(listed, row)
		}
		// Assigned after the loop: an explicit [] when the listing was asked
		// for and came back empty, never a nil the encoder would drop.
		out.Sessions = &listed
	}
	return out
}

// authIdentityLabel names the account the way `entire grant` takes it —
// "github:alice" — falling back to the provider's own user id when the account
// carries no handle.
//
// /me's handle is optional, and an account without one would otherwise be
// described by nothing at all: a verdict line, a jurisdiction and a token
// backend, with no way to tell whose login this is. The provider id is not a
// grantee spelling, but it identifies the account, which is the row's job.
func authIdentityLabel(p *authProfile) string {
	if p.Handle != "" {
		return formatQualifiedHandle(p.Provider, p.Handle)
	}
	if p.ProviderUserID != "" {
		return formatQualifiedHandle(p.Provider, p.ProviderUserID)
	}
	return ""
}

// authProfileRows renders the user identity from GET /me, omitting any field
// the server didn't populate.
//
// The handle is provider-qualified ("github:alice") because that is the
// grantee spelling every `entire grant` command accepts and `grant … list`
// prints — so what status shows is a value the user can paste into the next
// command, rather than a display form unique to this one.
//
// A foreign-region login gets no note here. The note this replaces existed
// mostly to explain a display name and email that a foreign core withholds,
// and neither is rendered any more; what was left restated the `jurisdiction`
// and `context` rows it sat between. The condition still reaches machine
// readers as the JSON `foreign_region` flag.
func authProfileRows(p *authProfile) []explainRow {
	var rows []explainRow
	if user := authIdentityLabel(p); user != "" {
		rows = append(rows, explainRow{Label: "user", Value: user})
	}
	// The home jurisdiction slug is what 'entire auth token --jurisdiction'
	// takes; surface it so it's discoverable non-interactively.
	if p.Jurisdiction != "" {
		rows = append(rows, explainRow{Label: "jurisdiction", Value: p.Jurisdiction})
	}
	return rows
}

// authContextRow names the active login context, appending the login server's
// host when that host is what tells this login apart from the others.
//
// splitServers is the caller's count of distinct servers across the saved
// contexts, and it gates the host for the same reason the row itself is gated
// on there being more than one context: several logins that all live on one
// server are distinguished by their names, and printing the shared host beside
// one of them says nothing about which login this is. Spread across servers,
// the host is precisely the distinguishing fact.
//
// A name that already spells the host is never doubled — context names default
// to the issuer host, so the common multi-server case needs no suffix either.
func authContextRow(sty statusStyles, name, coreURL string, splitServers bool) explainRow {
	value := name
	if host := authServerHost(coreURL); splitServers && host != "" && !strings.EqualFold(name, host) {
		value += sty.render(sty.dim, " · ") + host
	}
	return explainRow{Label: "context", Value: value}
}

// authSessionsRow reports how many login sessions exist, and how to see them.
// A listing failure is reported in the row rather than raised: the token is
// already known good, so the rest of the status is still worth printing.
//
// The second return is false when the row should be dropped: a single session
// that IS the caller's is already described by the verdict line's expiry, so
// counting it adds a row and no information. Zero still reports — logged in
// with no sessions is a contradiction worth seeing.
//
// headlineDeadline is that premise, passed rather than assumed. ExpiresAt is a
// plain string with no omitempty, so a session can arrive with none, and an
// unreadable one is dropped from the headline too; dropping the row as well
// would leave the default view with no count, no expiry and no route to
// --sessions.
//
// current gates that drop, and must not be assumed. A caller whose session was
// not identified gets no expiry on the verdict line, so dropping the row there
// would leave the default view with no count, no expiry and no route to
// --sessions — and the one listed session is precisely the one worth looking
// at, being some other login than the token in hand. That happens when a
// family is revoked while its access token is still inside its own lifetime:
// resolveStatusTarget falls back to the stale bearer, /me still honours it, and
// fid names a family the listing no longer contains.
func authSessionsRow(sty statusStyles, sessions []api.AuthSession, listErr error, showSessions bool, current int, headlineDeadline bool) (explainRow, bool) {
	if listErr != nil {
		return explainRow{Label: activeSessionsRowLabel, Value: fmt.Sprintf("(unavailable: %v)", listErr)}, true
	}
	if len(sessions) == 1 && current >= 0 && headlineDeadline {
		return explainRow{}, false
	}
	value := strconv.Itoa(len(sessions))
	if !showSessions && len(sessions) > 0 {
		value += sty.render(sty.dim, " · ") + "run 'entire auth status --sessions' to list them"
	}
	return explainRow{Label: activeSessionsRowLabel, Value: value}, true
}

// authContextsCountRow reports how many saved logins exist, and how to see
// them — the same count-plus-hint shape as the session row. Callers gate on
// total > 1, which is also when the context row above it appears: a count of
// one would be counting the only thing there is.
func authContextsCountRow(sty statusStyles, total int) explainRow {
	return explainRow{
		Label: availableContextsRowLabel,
		Value: strconv.Itoa(total) + sty.render(sty.dim, " · ") + "run 'entire auth contexts' to list them",
	}
}

// authServerHost reduces a login server URL to its bare host for display,
// falling back to the raw value when it will not parse — a host we cannot
// extract is still better named than not named at all.
func authServerHost(coreURL string) string {
	host, err := hostFromPublicURL(coreURL)
	if err != nil {
		return strings.TrimSpace(coreURL)
	}
	return host
}

// currentSessionIndex finds the session this CLI is authenticating with by
// matching the login token's fid (refresh-token family id) claim against the
// listed session ids — a login session IS a refresh-token family, so fid
// identifies it.
//
// Returns -1 when the claim is absent, unreadable, or names no listed session.
// The caller then shows no marker and no expiry rather than guessing a row:
// every action reachable from here (logout, revoke) ends a session, and ending
// someone else's is worse than saying nothing.
func currentSessionIndex(token string, sessions []api.AuthSession) int {
	fid, err := auth.SessionFamilyIDFromLoginJWT(token)
	if err != nil || fid == "" {
		return -1
	}
	for i, s := range sessions {
		if s.ID == fid {
			return i
		}
	}
	return -1
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
	s.header = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Bold(true)
	s.id = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Warning)) // yellow
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
	for _, line := range alignTableLines(header, rows) {
		fmt.Fprintln(w, line)
	}
}

// alignTableLines lays header and rows out in left-aligned columns and returns
// one string per line, header first, with no trailing newline.
//
// Separate from renderAlignedTable because the context picker needs the same
// columns as strings rather than written out: huh takes each row as an option
// label and the header as the field description, so there is no writer to
// render into. Trailing padding is trimmed, which matters there — huh styles
// the whole label, so padding on the end would widen the selected row's
// highlight past its text.
func alignTableLines(header []string, rows [][]string) []string {
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

	lines := make([]string, 0, len(rows)+1)
	for _, cells := range append([][]string{header}, rows...) {
		lines = append(lines, rowLine(cells, widths))
	}
	return lines
}

func rowLine(cells []string, widths []int) string {
	var b strings.Builder
	for i, c := range cells {
		b.WriteString(c)
		if i < len(cells)-1 {
			b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(c)+2))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// orDash renders an empty table cell as placeholderDash, so a column keeps its
// width and a missing value reads as absent rather than as a blank gap. Shared
// by the auth, context, and mirror tables — a whitespace-only value counts as
// empty, since a cell of spaces would silently break column alignment.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return placeholderDash
	}
	return s
}

// currentSessionMarker labels the row belonging to the caller's own session.
// The contexts table marks its active row the same way, with an unheaded
// trailing column, so both auth tables say "you are here" identically.
const currentSessionMarker = "(current)"

// renderAuthSessionsTable prints the active login sessions as an aligned table.
// No id column: there's no per-session CLI action (revoke-by-id is gone), so
// NAME/CREATED/LAST USED/EXPIRES is what's useful, plus a marker naming the
// session this command is running under. current is an index into sessions, or
// -1 when the caller could not be identified.
func renderAuthSessionsTable(w io.Writer, sty authTableStyles, sessions []api.AuthSession, current int) {
	header := []string{
		sty.render(sty.header, "NAME"),
		sty.render(sty.header, "CREATED"),
		sty.render(sty.header, "LAST USED"),
		sty.render(sty.header, "EXPIRES"),
		"", // marker column: the cells label themselves
	}
	rows := make([][]string, 0, len(sessions))
	for i, s := range sessions {
		marker := ""
		if i == current {
			marker = sty.render(sty.id, currentSessionMarker)
		}
		rows = append(rows, []string{
			sty.render(sty.name, orDash(s.Name)),
			sty.render(sty.value, formatAuthTimestamp(s.CreatedAt)),
			sty.render(sty.value, formatLastUsed(s.LastUsedAt)),
			sty.render(sty.value, formatSessionExpiry(s.ExpiresAt)),
			marker,
		})
	}
	renderAlignedTable(w, header, rows)
}

// sortAuthSessionsByRecency orders sessions most-recently-used first, then most
// recently created, then by id — a fully specified order independent of the
// server's response ordering.
func sortAuthSessionsByRecency(sessions []api.AuthSession) {
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

func lastUsedSortKey(s api.AuthSession) string {
	if s.LastUsedAt == nil {
		return ""
	}
	return *s.LastUsedAt
}

// formatSessionExpiry renders the session table's EXPIRES cell: the remaining
// time ("in 27d") while the session is live, a flat "expired" once it is not.
//
// The column header already supplies the verb, so the cell carries only the
// time — and "19h ago" under a heading reading EXPIRES states its tense by
// implication alone, which a reader scanning the column for a dead session will
// not pick up. CREATED and LAST USED keep the plain relative formatter, where
// the past IS the tense being reported.
//
// An unreadable value still reaches the cell verbatim: a column of its own is
// exactly where a value the server sent and this code could not parse should be
// shown, which is the distinction authDeadlineClause draws for the prose line.
func formatSessionExpiry(s string) string {
	if s == "" {
		return placeholderDash
	}
	ts := parseAuthTimestamp(s)
	if ts.IsZero() {
		return s
	}
	if !ts.After(time.Now()) {
		return "expired"
	}
	return timeAgo(ts)
}

// parseAuthTimestamp reads an RFC3339 instant, reporting the zero time for a
// value that is missing or unreadable. Both are "no deadline to state" to the
// verdict line, which is the only distinction it needs.
func parseAuthTimestamp(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// authDeadlineClause renders a deadline for the verdict line: "expires in 30d"
// while it is ahead, a flat "expired" once it has passed, and nothing at all
// for an instant that is missing or unreadable.
//
// Tense matters more here than anywhere else in the output, because the clause
// shares its line with the word "Logged in": rendering a lapsed deadline as
// "expires 19h ago" contradicts the verdict beside it. A past instant is
// genuinely reachable — a session's ExpiresAt is server-supplied and
// independent of the bearer /me has just accepted, so clock skew or a family
// that lapsed mid-command lands here.
//
// An unreadable value yields nothing rather than the raw wire string, which in
// the middle of a sentence reads as corruption of the line rather than of the
// field. The session table still shows it verbatim in a cell of its own, and
// --json carries it untouched.
func authDeadlineClause(deadline time.Time) string {
	switch {
	case deadline.IsZero():
		return ""
	case !deadline.After(time.Now()):
		return "expired"
	default:
		return "expires " + timeAgo(deadline)
	}
}

// formatAuthTimestamp renders an RFC3339 timestamp as a relative duration
// ("3h ago" for the past, "in 30d" for the future), falling back to a dash
// (empty) or the raw value (unparseable). The raw fallback belongs to the
// table, where a cell of its own is the right place to show a value the server
// sent and this code could not read; the verdict line drops it instead (see
// authDeadlineClause). Relative rather than absolute because a session table is
// read for recency: three rows all stamped the same day say nothing about which
// one is live.
func formatAuthTimestamp(s string) string {
	if s == "" {
		return placeholderDash
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return timeAgo(ts)
}

func formatLastUsed(s *string) string {
	if s == nil || *s == "" {
		return lastUsedNever
	}
	return formatAuthTimestamp(*s)
}
