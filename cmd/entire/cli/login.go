package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/procsignal"
	"github.com/spf13/cobra"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// loginCompleteMsg is the success line for a login whose token came from the
// host the user dialled. See loginCompleteLine for the dispatched variant.
const loginCompleteMsg = "✓ Login complete."

const fallbackDeviceAuthPollInterval = time.Second
const defaultSlowDownBackoff = 5 * time.Second
const maxPollInterval = 30 * time.Second
const maxExpiresIn = 15 * time.Minute
const maxTransientErrors = 5

// browserLoginTimeout bounds how long the browser flow waits for the
// loopback redirect. The device flow is bounded by the AS's expires_in
// (capped at maxExpiresIn); without a bound here a closed browser tab
// would hang `entire login` forever.
const browserLoginTimeout = 5 * time.Minute

// browserOpenFunc is the signature for opening a URL in the user's browser.
type browserOpenFunc func(ctx context.Context, url string) error

// clipboardWriteFunc is the signature for copying a URL to the system
// clipboard. Keeping it injectable lets login tests remain parallel and avoids
// touching the developer's real clipboard.
type clipboardWriteFunc func(text string) error

type loginURLAction byte

const (
	loginURLNone loginURLAction = iota
	loginURLCopy
	loginURLOpen
)

type loginURLActionReadFunc func(ctx context.Context) (loginURLAction, error)

// clipboardCopyTimeout bounds a single clipboard write. See copyLoginURL.
const clipboardCopyTimeout = 3 * time.Second

// loginURLInteractor owns the side effects behind the interactive URL prompt.
// Production uses the controlling TTY, system clipboard, and default browser;
// tests provide deterministic functions instead. keysAvailable answers whether
// readAction can actually deliver keys, so the prompt only advertises actions
// this process can honour.
type loginURLInteractor struct {
	keysAvailable func() bool
	readAction    loginURLActionReadFunc
	copyURL       clipboardWriteFunc
	openURL       browserOpenFunc
}

func defaultLoginURLInteractor(errW io.Writer) loginURLInteractor {
	return loginURLInteractor{
		keysAvailable: loginURLKeysAvailable,
		readAction: func(ctx context.Context) (loginURLAction, error) {
			return readLoginURLAction(ctx, errW)
		},
		copyURL: clipboard.WriteAll,
		openURL: openBrowser,
	}
}

// loginURLKeysAvailable reports whether single-key actions can be read from the
// controlling terminal. It mirrors exactly the conditions under which
// readLoginURLAction gives up and returns loginURLNone, so the prompt is never
// printed for keys that would be ignored.
func loginURLKeysAvailable() bool {
	if interactive.UnderTest() {
		return false
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer tty.Close()

	return interactive.IsTerminalReader(tty)
}

// copyLoginURL bounds a clipboard write. clipboardWriteFunc takes no context —
// atotto/clipboard shells out to xclip/xsel on Linux — so a wedged helper would
// otherwise block the select loop in waitForLoginURLResult indefinitely, and
// with it a sign-in that has already completed. On timeout the goroutine is
// abandoned; the CLI is short-lived and exits soon after.
func copyLoginURL(copyURL clipboardWriteFunc, loginURL string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- copyURL(loginURL) }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("clipboard write timed out after %v", timeout)
	}
}

// chooseApprovalURL prefers verification_uri_complete (RFC 8628 §3.3.1) so the
// browser lands on a URL with the user_code already in the query string —
// most verification pages prefill the input from that param, sparing the
// user from typing. Falls back to the bare verification_uri when the AS
// didn't supply a complete form.
func chooseApprovalURL(start *auth.DeviceAuthStart) string {
	if start.VerificationURIComplete != "" {
		return start.VerificationURIComplete
	}
	return start.VerificationURI
}

// deviceAuthClient abstracts the auth client so runLogin and waitForApproval can be unit-tested.
type deviceAuthClient interface {
	StartDeviceAuth(ctx context.Context) (*auth.DeviceAuthStart, error)
	PollDeviceAuth(ctx context.Context, deviceCode string) (*auth.DeviceAuthPoll, error)
	BaseURL() string
	// UseTokenIssuer points subsequent token polls at origin instead of
	// BaseURL, for a login server that dispatched the device authorization
	// to a regional host. Callers must vet origin first (see adoptIssuer).
	UseTokenIssuer(origin string) error
}

// browserAuthFlow abstracts an in-progress loopback authorization-code
// login so runBrowserLogin can be unit-tested with a fake instead of a real
// listener. *auth.BrowserAuthFlow satisfies it.
type browserAuthFlow interface {
	AuthorizationURL() string
	Wait(ctx context.Context) (code string, err error)
	// Issuer is the RFC 9207 iss parameter from the loopback callback, or
	// "" when the login server sent none. Unvalidated — see adoptIssuer.
	Issuer() string
	// UseTokenIssuer redeems the authorization code at origin instead of
	// the dialled login server. Callers must vet origin first.
	UseTokenIssuer(origin string) error
	Exchange(ctx context.Context, code string) (accessToken, refreshToken string, err error)
	Close() error
}

func newLoginCmd() *cobra.Command {
	var (
		insecureHTTPAuth bool
		useDevice        bool
		server           string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			loginServer, err := parseLoginServer(server)
			if err != nil {
				return fmt.Errorf("invalid --server: %w", err)
			}
			if err := requireSecureLoginServer(loginServer, insecureHTTPAuth); err != nil {
				return err
			}
			client := auth.NewClient(loginServer, nil, insecureHTTPAuth)
			// Closure adapts the concrete *auth.BrowserAuthFlow result to the
			// browserAuthFlow interface (func types are invariant, so the
			// method value alone won't do). On error the flow is a typed nil,
			// which is fine — runLoginAuto checks err before touching it.
			startBrowser := func(ctx context.Context) (browserAuthFlow, error) {
				return client.StartBrowserAuth(ctx)
			}
			return runLoginAuto(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				client, startBrowser, defaultLoginURLInteractor(cmd.ErrOrStderr()), loginFlowFacts{
					useDevice:  useDevice,
					canPrompt:  interactive.CanPromptInteractively(),
					sshSession: isSSHSession(),
				})
		},
	}
	cmd.Flags().StringVar(&server, "server", api.DefaultAuthBaseURL,
		"login server to authenticate against (rarely needed; the default serves all standard accounts)")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	cmd.Flags().BoolVar(&useDevice, "device", false, "Use the device-code flow (enter a code in your browser) instead of the default browser redirect")
	return cmd
}

// parseLoginServer validates and canonicalises the --server value: an
// http(s) origin with nothing but scheme and host. Userinfo, path, query,
// and fragment are rejected rather than silently dropped — the value
// becomes the OAuth issuer, the token-exchange target, and the keyring
// key, so surprising rewrites would surface as confusing auth failures
// much later. A lone trailing slash is tolerated (normalised away).
func parseLoginServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty server URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	// Error messages echo u.Redacted(), not raw: the URL may carry
	// userinfo (that's one of the rejection cases), and stderr often ends
	// up in CI logs where a password must not appear.
	switch {
	case u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP:
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Redacted())
	case u.Host == "":
		return "", fmt.Errorf("missing host in %q", u.Redacted())
	case u.User != nil:
		return "", fmt.Errorf("userinfo not allowed in %q", u.Redacted())
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("path not allowed in %q (use the bare origin)", u.Redacted())
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("query/fragment not allowed in %q", u.Redacted())
	}
	return api.NormalizeOriginURL(raw), nil
}

// requireSecureLoginServer enforces TLS for the chosen login server — the
// only host login dials. --insecure-http-auth opts in to http:// (and
// enables it process-wide for the token save path).
func requireSecureLoginServer(server string, insecureHTTPAuth bool) error {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
		return nil
	}
	if err := api.RequireSecureURL(server); err != nil {
		return fmt.Errorf("login server check: %w", err)
	}
	return nil
}

func runLogin(ctx context.Context, outW, errW io.Writer, client deviceAuthClient, urlInteractor loginURLInteractor, canPrompt bool) error {
	start, err := client.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	// The apex login server redirects /device_authorization to a regional
	// one and serves no token endpoint itself, so the poll has to follow the
	// device code to whichever host actually answered. Do this before any
	// output: nothing is shown to the user until we know the poll target.
	issuer, err := adoptIssuer(start.ResponseOrigin, client.BaseURL())
	if err != nil {
		return err
	}
	if issuer != "" {
		if err := client.UseTokenIssuer(issuer); err != nil {
			return fmt.Errorf("target login server %s: %w", issuer, err)
		}
	}

	approvalURL := chooseApprovalURL(start)
	fmt.Fprintf(outW, "Device code: %s\n", start.UserCode)
	printLoginURL(outW, deviceLoginURLLabel, approvalURL)

	wait := func(waitCtx context.Context) (loginTokens, error) {
		accessToken, refreshToken, waitErr := waitForApproval(
			waitCtx,
			client,
			start.DeviceCode,
			start.ExpiresIn,
			time.Duration(start.Interval)*time.Second,
			defaultSlowDownBackoff,
		)
		return loginTokens{accessToken: accessToken, refreshToken: refreshToken}, waitErr
	}

	var result loginTokens
	if canPrompt {
		result, err = waitForLoginURLResult(ctx, outW, errW, approvalURL, "Waiting for approval… ", urlInteractor, wait)
	} else {
		fmt.Fprint(outW, "Waiting for approval… ")
		result, err = wait(ctx)
	}
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, client.BaseURL(), issuer, result.accessToken, result.refreshToken)
}

type loginTokens struct {
	accessToken  string
	refreshToken string
}

// loginFlowFacts carries the environment facts that pick between the
// browser and device-code flows. Detection happens once at the command
// entry point; the decision logic below only consumes these values.
type loginFlowFacts struct {
	useDevice  bool // --device flag
	canPrompt  bool // interactive terminal present
	sshSession bool // running inside an SSH session
}

// runLoginAuto picks between the browser (loopback authorization-code) and
// device-code flows and runs the chosen one. The browser flow is the
// default — no code to type, no poll latency — but it needs a browser that
// can reach this machine's 127.0.0.1, so headless terminals (CI, piped
// stdin), SSH sessions, and a loopback listener that fails to start all
// fall back to the device flow with a one-line explanation; the same
// both-flows-with-fallback shape gh / gcloud / aws sso ship. --device
// forces the device flow without commentary.
func runLoginAuto(ctx context.Context, outW, errW io.Writer, deviceClient deviceAuthClient, startBrowser func(context.Context) (browserAuthFlow, error), urlInteractor loginURLInteractor, facts loginFlowFacts) error {
	if shouldUseBrowserLogin(facts) {
		flow, err := startBrowser(ctx)
		if err != nil {
			// Binding the loopback listener can fail (sandboxing, firewall,
			// exhausted ports); that shouldn't strand the user — warn and
			// use the device flow instead.
			fmt.Fprintf(errW, "Warning: could not start browser sign-in (%v); falling back to the device-code flow.\n", err)
			return runLogin(ctx, outW, errW, deviceClient, urlInteractor, facts.canPrompt)
		}
		return runBrowserLogin(ctx, outW, errW, flow, deviceClient.BaseURL(), urlInteractor, browserLoginTimeout)
	}
	switch {
	case facts.useDevice:
		// Explicitly requested; no explanation needed.
	case !facts.canPrompt:
		fmt.Fprintln(errW, "No interactive terminal detected; using device-code flow.")
	case facts.sshSession:
		fmt.Fprintln(errW, "SSH session detected; using device-code flow (a browser opened here couldn't reach this machine).")
	}
	return runLogin(ctx, outW, errW, deviceClient, urlInteractor, facts.canPrompt)
}

// shouldUseBrowserLogin reports whether `entire login` should use the
// loopback authorization-code (browser) flow. The browser flow is the
// default but needs a local browser + reachable 127.0.0.1, so it's only
// chosen when --device wasn't passed, an interactive terminal is present,
// and we're not inside an SSH session (where the loopback listener binds
// on the remote host, out of the user's browser's reach); otherwise the
// caller falls back to the device flow.
func shouldUseBrowserLogin(f loginFlowFacts) bool {
	return !f.useDevice && f.canPrompt && !f.sshSession
}

// isSSHSession reports whether this process is running inside an SSH
// session: sshd sets SSH_CONNECTION/SSH_CLIENT for every session and
// SSH_TTY for interactive ones.
func isSSHSession() bool {
	return os.Getenv("SSH_CONNECTION") != "" ||
		os.Getenv("SSH_CLIENT") != "" ||
		os.Getenv("SSH_TTY") != ""
}

// runBrowserLogin runs the loopback authorization-code flow on an
// already-started flow: show the authorization URL and explicit actions, wait
// up to waitTimeout for the redirect back to the local listener, then exchange
// the code for tokens. Shares the token validation + persistence tail with
// runLogin via persistLogin.
func runBrowserLogin(ctx context.Context, outW, errW io.Writer, flow browserAuthFlow, baseURL string, urlInteractor loginURLInteractor, waitTimeout time.Duration) error {
	// Wait tears the listener down on return, but Close is idempotent and
	// covers the error paths before Wait runs.
	defer func() { _ = flow.Close() }()

	authURL := flow.AuthorizationURL()
	fmt.Fprintf(outW, "Logging in to %s\n\n", baseURL)
	printLoginURL(outW, browserLoginURLLabel, authURL)

	// Start the deadline as soon as the URL is visible. Waiting for the
	// loopback redirect and reading URL actions then proceed concurrently, so
	// clicking or pasting the URL can complete login without a terminal key.
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	code, err := waitForLoginURLResult(
		waitCtx,
		outW,
		errW,
		authURL,
		"Waiting for sign-in… ",
		urlInteractor,
		flow.Wait,
	)
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for sign-in after %v; run `entire login` again, or use `entire login --device`", waitTimeout)
		}
		return fmt.Errorf("complete login: %w", err)
	}

	// RFC 9207: a dispatching login server redirected the browser to the
	// region that issued this code, and named itself on the callback. Only
	// that region will redeem the code — the apex serves no token endpoint —
	// so the exchange has to follow it, once the host clears adoptIssuer.
	issuer, err := adoptIssuer(flow.Issuer(), baseURL)
	if err != nil {
		return err
	}
	if issuer != "" {
		if err := flow.UseTokenIssuer(issuer); err != nil {
			return fmt.Errorf("target login server %s: %w", issuer, err)
		}
	}

	token, refreshToken, err := flow.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, baseURL, issuer, token, refreshToken)
}

// persistLogin validates the freshly-issued access token and records it in
// the shared contexts.json credential model. Shared by the device-code and
// browser flows.
//
// dialled is the host the user asked for; adoptedIssuer is the origin a
// dispatching login server handed the flow off to, or "" when there was no
// handoff. The token's iss is validated against the handoff target when there
// was one, and against the dialled host otherwise — the same rule that gated
// the handoff, now applied to the token it produced.
//
// Scoping to the handoff target matters because iss names the *signer*, and a
// verifier resolves JWKS from it. Only the region that minted the token can
// claim it, and that is the region that answered the handoff — so a token
// arriving from us.auth.entire.io while claiming eu.auth.entire.io is
// misconfiguration, even though both sit under the dialled apex. Validating
// against the apex alone would wave that through and leave a context whose
// every later refresh fails against a region that never issued it.
//
// A jurisdiction the account is not homed in can legitimately mint (the apex
// geo-routes the device flow, so an AU-homed account can be served by EU); the
// home region travels in the home_jurisdiction claim, not in iss.
func persistLogin(outW io.Writer, dialled, adoptedIssuer, token, refreshToken string) error {
	expected := dialled
	if adoptedIssuer != "" {
		expected = adoptedIssuer
	}
	if err := validateReceivedToken(token, expected, time.Now()); err != nil {
		return fmt.Errorf("reject login token: %w", err)
	}

	// Record the login in the shared contexts.json credential model — the
	// single store every consumer (control plane, data API, git remote
	// helper, entiredb's CLIs) resolves against.
	if _, err := auth.RecordLoginContext(token, refreshToken, true); err != nil {
		return fmt.Errorf("save login: %w", withHeadlessStoreHint(err))
	}

	fmt.Fprintln(outW, loginCompleteLine(token, dialled))
	return nil
}

// loginCompleteLine names the issuer when it differs from the host the user
// asked for. With a dispatching login server the two legitimately differ,
// and the issuer — not the dialled host — is what gets persisted as the
// context's CoreURL and targeted by every later refresh and token exchange,
// so it's worth one line of confirmation. Falls back to the plain message
// when the claims don't parse; validateReceivedToken has already accepted
// the token by this point, so this is display only.
func loginCompleteLine(token, dialled string) string {
	claims, err := tokens.ParseClaims(token)
	if err != nil {
		return loginCompleteMsg
	}
	if iss := api.OriginOnly(claims.Issuer); iss != "" && iss != api.OriginOnly(dialled) {
		return fmt.Sprintf("✓ Login complete (signed in at %s).", iss)
	}
	return loginCompleteMsg
}

// withHeadlessStoreHint appends file-token-store guidance to a credential
// store write failure. The default backend is the OS keyring, which locked
// or keyring-less machines (CI, containers, minimal server VMs) can't use —
// the raw store error gives those users no way forward (#1036). The hint is
// skipped when ENTIRE_TOKEN_STORE=file is already set (suggesting it again
// would be nonsense) and for failures the file store wouldn't help with.
func withHeadlessStoreHint(err error) error {
	if !errors.Is(err, auth.ErrCredentialStoreWrite) || tokenstore.FileBackendSelected() {
		return err
	}

	return fmt.Errorf("%w\n\nIf this machine has no usable OS keyring (headless server, container, CI), store tokens in a file instead:\n\n  %s=file entire login\n\nTokens are then written with 0600 permissions to %s (override the location with %s)",
		err, tokenstore.BackendEnvVar, tokenstore.FileBackendPath(), tokenstore.PathEnvVar)
}

// validateReceivedToken runs minimum-trust checks on the access token
// the AS handed us before we persist it. The server is the authority
// on signature/exp; this is defense in depth aimed at catching gross
// misbehaviour by a compromised or misconfigured AS (e.g. handing back
// a token from a different issuer than the one we asked, or one whose
// claims are already-expired).
//
// It also enforces what the contexts model needs up front:
// RecordLoginContext — the sole persistence path — keys the context and
// keychain slot on the token's iss and handle/sub claims, so a token
// without parseable claims can never complete a login. Rejecting it here
// names the requirement instead of surfacing a parse error from the save
// step. Entire-core always issues claim-bearing JWTs; opaque-token-only
// servers are not supported.
func validateReceivedToken(rawToken, issuerURL string, now time.Time) error {
	claims, err := tokens.ParseClaims(rawToken)
	if errors.Is(err, tokens.ErrUnsignedJWT) {
		return err //nolint:wrapcheck // sentinel surfaces verbatim for caller's errors.Is
	}
	if err != nil {
		return fmt.Errorf("login server issued a token without parseable JWT claims (claim-bearing JWTs are required): %w", err)
	}
	if claims.Issuer == "" {
		return errors.New("token has no iss claim; cannot record a login context")
	}
	if claims.Handle == "" && claims.Subject == "" {
		return errors.New("token has no handle or sub claim; cannot record a login context")
	}

	// iss check: the token must claim to come from the issuer we sent
	// the device-code request to. A mismatch means either the AS is
	// misconfigured or someone's playing games.
	if issErr := issMatches(claims.Issuer, issuerURL); issErr != nil {
		return issErr
	}

	// exp sanity: a token that's already expired before we even store
	// it is a smell. Don't reject if exp is unset (some servers omit).
	if !claims.ExpiresAt.IsZero() && !now.Before(claims.ExpiresAt) {
		return fmt.Errorf("token already expired (exp=%s, now=%s)",
			claims.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	return nil
}

// issMatches reports whether an issuer identifier is one this CLI accepts
// for a login dialled at expected. Both sides are stripped to a bare origin
// via api.OriginOnly first, so "https://issuer/" and "https://issuer" match.
// The caller has already rejected an empty iss claim.
//
// Two shapes are accepted:
//
//   - exact origin equality (the single-region case, and any dev/loopback
//     login server), and
//   - claimed is a strict subdomain of expected over https — the production
//     shape, where https://auth.entire.io is a dispatcher that hands the
//     login off to a regional login server such as
//     https://us.auth.entire.io, which is what actually mints the token.
//
// The subdomain rule is a delegation rule, not a string-prefix one:
// "auth.entire.io.evil.com" does not end in ".auth.entire.io", and a sibling
// like "entire.io" is not below it either. https is required so a
// dispatching host can never widen trust to a plaintext origin, and the port
// must match so ":443" and ":8443" stay distinct issuers.
func issMatches(claimed, expected string) error {
	normClaimed := api.OriginOnly(claimed)
	normExpected := api.OriginOnly(expected)
	if normClaimed == normExpected {
		return nil
	}
	if isDelegatedIssuer(normClaimed, normExpected) {
		return nil
	}
	// Only offer the delegation shape when this origin could ever delegate.
	// A plaintext dev/loopback login server never can, so naming a subdomain
	// there would send the reader after a host that gets rejected too.
	if canDelegate(normExpected) {
		return fmt.Errorf("iss mismatch: token claims %q, expected %q or a subdomain of it", normClaimed, normExpected)
	}
	return fmt.Errorf("iss mismatch: token claims %q, expected %q", normClaimed, normExpected)
}

// canDelegate reports whether expected is an origin isDelegatedIssuer could
// accept any subdomain of at all — that is, an https origin with a host.
func canDelegate(expected string) bool {
	u, err := url.Parse(expected)
	return err == nil && u.Scheme == schemeHTTPS && u.Hostname() != ""
}

// isDelegatedIssuer reports whether claimed is an https origin whose host
// sits strictly below expected's host, on the same port. Both arguments are
// already api.OriginOnly-normalised (lowercase, default port dropped, no
// path/query/fragment).
func isDelegatedIssuer(claimed, expected string) bool {
	c, cErr := url.Parse(claimed)
	e, eErr := url.Parse(expected)
	if cErr != nil || eErr != nil {
		return false
	}
	// https only, on both sides: a dispatcher reached over TLS must not be
	// able to redirect the token exchange to a plaintext origin, and a
	// plaintext (dev/loopback) login server gets no delegation at all.
	if c.Scheme != schemeHTTPS || e.Scheme != schemeHTTPS {
		return false
	}
	if c.Port() != e.Port() {
		return false
	}
	claimedHost, expectedHost := c.Hostname(), e.Hostname()
	if claimedHost == "" || expectedHost == "" {
		return false
	}
	// The leading dot is what makes this a delegation check: it forces a
	// label boundary, so neither "xauth.entire.io" nor "auth.entire.io.evil.com"
	// qualifies under "auth.entire.io".
	return strings.HasSuffix(claimedHost, "."+expectedHost)
}

// adoptIssuer decides whether a login started against dialled may be
// completed at the issuer the login server named, and returns the origin the
// token request should go to ("" meaning "no change").
//
// This is the security-critical half of the dispatching-login-server
// support: the returned origin receives the authorization code (or the
// device code) and hands back the user's tokens, so it is gated by exactly
// the same rule as the iss claim on the token itself.
func adoptIssuer(claimed, dialled string) (string, error) {
	normClaimed := api.OriginOnly(strings.TrimSpace(claimed))
	if normClaimed == "" || normClaimed == api.OriginOnly(dialled) {
		return "", nil
	}
	if err := issMatches(normClaimed, dialled); err != nil {
		return "", fmt.Errorf("login server %s handed off to an unacceptable issuer: %w", api.OriginOnly(dialled), err)
	}
	return normClaimed, nil
}

func waitForApproval(ctx context.Context, poller deviceAuthClient, deviceCode string, expiresIn int, interval, slowDownBackoff time.Duration) (accessToken, refreshToken string, err error) {
	expiry := time.Duration(expiresIn) * time.Second
	if expiry <= 0 || expiry > maxExpiresIn {
		expiry = maxExpiresIn
	}
	deadline := time.Now().Add(expiry)
	pollInterval := interval
	if pollInterval <= 0 {
		pollInterval = fallbackDeviceAuthPollInterval
	}

	consecutiveErrors := 0

	for {
		if time.Now().After(deadline) {
			return "", "", errors.New("device authorization expired")
		}

		result, err := poller.PollDeviceAuth(ctx, deviceCode)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxTransientErrors {
				return "", "", fmt.Errorf("poll approval status (after %d consecutive failures): %w", consecutiveErrors, err)
			}
			// Transient error — wait and retry.
			select {
			case <-ctx.Done():
				return "", "", fmt.Errorf("wait for approval: %w", ctx.Err())
			case <-time.After(pollInterval):
			}
			continue
		}
		consecutiveErrors = 0

		switch result.Error {
		case "":
			if result.AccessToken == "" {
				return "", "", errors.New("device authorization completed without a token")
			}
			return result.AccessToken, result.RefreshToken, nil
		case "authorization_pending":
			// no-op, will sleep and retry below
		case "slow_down":
			pollInterval += slowDownBackoff
			if pollInterval > maxPollInterval {
				pollInterval = maxPollInterval
			}
		case "access_denied":
			return "", "", errors.New("device authorization denied")
		case "expired_token":
			return "", "", errors.New("device authorization expired")
		default:
			if result.ErrorDescription != "" {
				return "", "", fmt.Errorf("device authorization failed: %s: %s", result.Error, result.ErrorDescription)
			}
			return "", "", fmt.Errorf("device authorization failed: %s", result.Error)
		}

		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("wait for approval: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

const loginURLPrompt = "[Enter] Open browser  [c] Copy URL"

const (
	browserLoginURLLabel = "Login URL:"
	deviceLoginURLLabel  = browserLoginURLLabel
)

// printLoginURL always leaves the literal URL visible. On a styled terminal it
// also wraps that same text and target in OSC 8, giving capable terminals an
// explicit hyperlink without taking away the plain-text fallback.
func printLoginURL(outW io.Writer, urlLabel, loginURL string) {
	fmt.Fprintf(outW, "%s\n  %s\n\n", urlLabel, renderLoginURL(loginURL, interactive.ShouldStyle(outW)))
}

func renderLoginURL(loginURL string, hyperlink bool) string {
	if !hyperlink {
		return loginURL
	}

	u, err := url.Parse(loginURL)
	if err != nil || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return loginURL
	}

	return lipgloss.NewStyle().Hyperlink(loginURL).Render(loginURL)
}

type loginURLActionResult struct {
	action loginURLAction
	err    error
}

type loginURLWaitResult[T any] struct {
	value T
	err   error
}

// waitForLoginURLResult runs authentication and terminal input concurrently.
// A completed authentication cancels and joins the input reader before
// returning, which guarantees Bubble Tea restores the terminal from raw mode.
// Copy leaves both actions available; a successful browser open stops reading
// keys and waits only for authentication to finish.
func waitForLoginURLResult[T any](
	ctx context.Context,
	outW, errW io.Writer,
	loginURL, waitingMessage string,
	interactor loginURLInteractor,
	wait func(context.Context) (T, error),
) (T, error) {
	flowCtx, cancelFlow := context.WithCancel(ctx)
	defer cancelFlow()

	actionCtx, cancelAction := context.WithCancel(flowCtx)
	defer cancelAction()

	waitCh := make(chan loginURLWaitResult[T], 1)
	go func() {
		value, err := wait(flowCtx)
		waitCh <- loginURLWaitResult[T]{value: value, err: err}
	}()

	actionCh := make(chan loginURLActionResult, 1)
	actionActive := false
	startActionRead := func() {
		actionActive = true
		go func() {
			action, err := interactor.readAction(actionCtx)
			actionCh <- loginURLActionResult{action: action, err: err}
		}()
	}
	finishActionRead := func() {
		cancelAction()
		if actionActive {
			<-actionCh
			actionActive = false
		}
	}

	statusLineOpen := false
	renderInitialPrompt := func() {
		// Only advertise the keys when the terminal can actually deliver them.
		// Authentication still completes through the always-visible URL.
		if interactor.keysAvailable() {
			fmt.Fprintln(outW, loginURLPrompt)
		}
		fmt.Fprint(outW, waitingMessage)
		statusLineOpen = true
	}
	finishStatusLine := func() {
		if statusLineOpen {
			fmt.Fprintln(outW)
			statusLineOpen = false
		}
	}
	renderInitialPrompt()
	startActionRead()

	for {
		select {
		case result := <-waitCh:
			finishActionRead()
			finishStatusLine()
			return result.value, result.err
		case result := <-actionCh:
			actionActive = false
			finishStatusLine()

			// Authentication may have completed at the same time as the key
			// arrived. Prefer that result over launching a stale side effect.
			select {
			case waitResult := <-waitCh:
				cancelAction()
				return waitResult.value, waitResult.err
			default:
			}

			if result.err != nil {
				cancelFlow()
				<-waitCh
				var zero T
				return zero, result.err
			}

			switch result.action {
			case loginURLNone:
				// A controlling TTY disappeared between prompt detection and
				// use (or tests disabled real terminal input). Authentication
				// still runs and can complete through the visible URL.
				cancelAction()
				result := <-waitCh
				return result.value, result.err
			case loginURLCopy:
				if err := copyLoginURL(interactor.copyURL, loginURL, clipboardCopyTimeout); err != nil {
					fmt.Fprintf(errW, "Warning: failed to copy login URL: %v\n", err)
				} else {
					fmt.Fprintln(outW, "✓ Copied to clipboard.")
				}
				startActionRead()
			case loginURLOpen:
				if err := interactor.openURL(flowCtx, loginURL); err != nil {
					fmt.Fprintf(errW, "Warning: failed to open default browser: %v\n", err)
					startActionRead()
					continue
				}

				cancelAction()
				fmt.Fprintln(outW, "✓ Opened browser.")
				result := <-waitCh
				return result.value, result.err
			default:
				cancelFlow()
				<-waitCh
				var zero T
				return zero, fmt.Errorf("unexpected login URL action: %d", result.action)
			}
		}
	}
}

// readLoginURLAction reads a single key from the controlling terminal without
// consuming piped stdin. Missing TTYs disable key actions while authentication
// continues through the always-visible URL. Anything unexpected is reported to
// errW rather than swallowed, so a user whose keys stopped working can tell why.
func readLoginURLAction(ctx context.Context, errW io.Writer) (loginURLAction, error) {
	// In-process and forced-interactive subprocess tests must never touch a
	// developer's real terminal. Authentication itself continues concurrently.
	if interactive.UnderTest() {
		return loginURLNone, nil
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return loginURLNone, nil //nolint:nilerr // no controlling TTY; continue without key actions
	}

	return readLoginURLActionFromTTY(ctx, errW, tty)
}

// readLoginURLActionFromTTY takes ownership of tty. Bubble Tea handles raw mode,
// escape-sequence decoding, and terminal restoration. If the terminal cannot
// provide single-key input, disable key actions.
func readLoginURLActionFromTTY(ctx context.Context, errW io.Writer, tty *os.File) (loginURLAction, error) {
	closeTTY := true
	defer func() {
		if closeTTY {
			_ = tty.Close()
		}
	}()

	if !interactive.IsTerminalReader(tty) {
		return loginURLNone, nil
	}

	// Fast path: don't put the terminal in raw mode for a read we would abandon.
	if err := ctx.Err(); err != nil {
		return loginURLNone, fmt.Errorf("interrupted: %w", err)
	}

	program := tea.NewProgram(
		loginURLActionModel{},
		tea.WithInput(tty),
		tea.WithOutput(io.Discard),
		tea.WithoutSignalHandler(),
	)

	// Cancellation must reach Bubble Tea as a Quit, never as its context: Run
	// treats a cancelled context as a kill, and shutdown(kill=true) skips
	// waitForReadLoop() before closing its cancelreader — closing tty, and
	// cancelreader's own cancel-signal pipe, under a live reader. A QuitMsg leaves
	// `killed` false, so the read loop is joined before anything closes.
	stopQuit := context.AfterFunc(ctx, program.Quit)
	defer stopQuit()

	finalModel, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		// Bubble Tea reports any event-loop error this way, input-stream failures
		// included, and a killed Run skipped waitForReadLoop — so the reader may
		// still hold tty. Let process exit reclaim the fd rather than race for it.
		closeTTY = false
	}
	if ctx.Err() != nil {
		return loginURLNone, fmt.Errorf("interrupted: %w", ctx.Err())
	}
	if err != nil {
		// Raw mode or the input reader failed. Sign-in continues through the
		// visible URL, but say so — the prompt already offered the keys.
		fmt.Fprintf(errW, "Warning: keyboard actions unavailable (%v); open the login URL above to continue.\n", err)
		return loginURLNone, nil
	}

	// Defensive: unreachable as configured — a nil error means a graceful QuitMsg,
	// and both sources are handled above (the model's tea.Quit sets selected; the
	// cancellation Quit is caught by the ctx.Err() check). Degrade anyway: a future
	// keybinding or filter could arm this, and losing keys must never kill a
	// sign-in the visible URL can still complete.
	result, ok := finalModel.(loginURLActionModel)
	if !ok || !result.selected {
		fmt.Fprintln(errW, "Warning: keyboard actions unavailable; open the login URL above to continue.")
		return loginURLNone, nil
	}
	// Bubble Tea puts the TTY in raw mode, so Ctrl-C arrives as a keypress
	// instead of SIGINT. Record the equivalent process signal before returning
	// context.Canceled so main preserves the CLI's normal quiet SIGINT/130 exit
	// semantics (including breaking an enclosing shell loop).
	if errors.Is(result.err, context.Canceled) {
		procsignal.Store(os.Interrupt)
	}
	return result.action, result.err
}

type loginURLActionModel struct {
	action   loginURLAction
	selected bool
	err      error
}

func (loginURLActionModel) Init() tea.Cmd { return nil }

func (m loginURLActionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}

	switch key.String() {
	case "enter", "ctrl+j":
		m.action = loginURLOpen
	case "c", "C":
		m.action = loginURLCopy
	case "ctrl+c":
		m.err = context.Canceled
	default:
		return m, nil
	}

	m.selected = true
	return m, tea.Quit
}

func (loginURLActionModel) View() tea.View { return tea.NewView("") }

// openBrowser opens browserURL in the user's default browser. The URL is
// handed to the platform launcher as a single argument and must never reach a
// shell — see openBrowserPlatform in browser_open_windows.go for why.
//
// The context is unused: launching is fire-and-forget on every platform. The
// parameter stays to satisfy browserOpenFunc.
func openBrowser(_ context.Context, browserURL string) error {
	u, err := url.Parse(browserURL)
	if err != nil || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return fmt.Errorf("refusing to open non-HTTP URL: %s", browserURL)
	}

	// Under test there's no usable browser, and we must not spawn a real one
	// on a dev/CI host. URL validation above still applies.
	if interactive.UnderTest() {
		return errors.New("browser unavailable under test")
	}

	return openBrowserPlatform(browserURL)
}
