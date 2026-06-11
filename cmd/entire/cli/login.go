package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

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
}

// browserAuthFlow abstracts an in-progress loopback authorization-code
// login so runBrowserLogin can be unit-tested with a fake instead of a real
// listener. *auth.BrowserAuthFlow satisfies it.
type browserAuthFlow interface {
	AuthorizationURL() string
	Wait(ctx context.Context) (code string, err error)
	Exchange(ctx context.Context, code string) (accessToken, refreshToken string, err error)
	Close() error
}

func newLoginCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var useDevice bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			client := auth.NewClient(nil, insecureHTTPAuth)
			// Closure adapts the concrete *auth.BrowserAuthFlow result to the
			// browserAuthFlow interface (func types are invariant, so the
			// method value alone won't do). On error the flow is a typed nil,
			// which is fine — runLoginAuto checks err before touching it.
			startBrowser := func(ctx context.Context) (browserAuthFlow, error) {
				return client.StartBrowserAuth(ctx)
			}
			return runLoginAuto(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				client, startBrowser, openBrowser, loginFlowFacts{
					useDevice:  useDevice,
					canPrompt:  interactive.CanPromptInteractively(),
					sshSession: isSSHSession(),
				})
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	cmd.Flags().BoolVar(&useDevice, "device", false, "Use the device-code flow (enter a code in your browser) instead of the default browser redirect")
	return cmd
}

func runLogin(ctx context.Context, outW, errW io.Writer, client deviceAuthClient, openURL browserOpenFunc, canPrompt bool) error {
	start, err := client.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	fmt.Fprintf(outW, "Device code: %s\n", start.UserCode)

	approvalURL := chooseApprovalURL(start)

	if canPrompt {
		// chooseApprovalURL prefers the code-embedded verification_uri_complete,
		// so opening the URL is usually all the user needs to do. The device
		// code is printed above regardless, so it's still available to confirm
		// against the page (RFC 8628 §3.3.1) or to enter on the bare-URI fallback.
		fmt.Fprintf(outW, "Login URL:   %s\n\n", approvalURL)
		fmt.Fprintf(outW, "Press Enter to open in browser...")

		// Read from /dev/tty so we get a real keypress and don't consume piped stdin.
		if err := waitForEnter(ctx); err != nil {
			return fmt.Errorf("wait for input: %w", err)
		}

		fmt.Fprintln(outW)
		if err := openURL(ctx, approvalURL); err != nil {
			fmt.Fprintf(errW, "Warning: failed to open browser: %v\n", err)
			fmt.Fprintf(outW, "Open this URL in your browser to approve this login: %s\n", approvalURL)
		}
	} else {
		fmt.Fprintf(outW, "Login URL:   %s\n\n", approvalURL)
	}

	fmt.Fprint(outW, "Waiting for approval... ")

	token, refreshToken, err := waitForApproval(ctx, client, start.DeviceCode, start.ExpiresIn, time.Duration(start.Interval)*time.Second, defaultSlowDownBackoff)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, errW, client.BaseURL(), token, refreshToken)
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
func runLoginAuto(ctx context.Context, outW, errW io.Writer, deviceClient deviceAuthClient, startBrowser func(context.Context) (browserAuthFlow, error), openURL browserOpenFunc, facts loginFlowFacts) error {
	if shouldUseBrowserLogin(facts) {
		flow, err := startBrowser(ctx)
		if err != nil {
			// Binding the loopback listener can fail (sandboxing, firewall,
			// exhausted ports); that shouldn't strand the user — warn and
			// use the device flow instead.
			fmt.Fprintf(errW, "Warning: could not start browser sign-in (%v); falling back to the device-code flow.\n", err)
			return runLogin(ctx, outW, errW, deviceClient, openURL, facts.canPrompt)
		}
		return runBrowserLogin(ctx, outW, errW, flow, deviceClient.BaseURL(), openURL, browserLoginTimeout)
	}
	switch {
	case facts.useDevice:
		// Explicitly requested; no explanation needed.
	case !facts.canPrompt:
		fmt.Fprintln(errW, "No interactive terminal detected; using device-code flow.")
	case facts.sshSession:
		fmt.Fprintln(errW, "SSH session detected; using device-code flow (a browser opened here couldn't reach this machine).")
	}
	return runLogin(ctx, outW, errW, deviceClient, openURL, facts.canPrompt)
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
// already-started flow: open the authorization URL in the user's browser,
// wait up to waitTimeout for the redirect back to the local listener, then
// exchange the code for tokens. Shares the token validation + persistence
// tail with runLogin via persistLogin.
func runBrowserLogin(ctx context.Context, outW, errW io.Writer, flow browserAuthFlow, baseURL string, openURL browserOpenFunc, waitTimeout time.Duration) error {
	// Wait tears the listener down on return, but Close is idempotent and
	// covers the error paths before Wait runs.
	defer func() { _ = flow.Close() }()

	// Mirror the device flow's interactive shape: show the URL, pause on
	// Enter before opening the browser, then wait on the same line so
	// persistLogin's "Login complete." reads "Waiting for sign-in...
	// Login complete." runBrowserLogin is only reached interactively (see
	// shouldUseBrowserLogin), so the Enter prompt is unconditional here.
	authURL := flow.AuthorizationURL()
	// Show the auth host, not the full authorize URL — the PKCE challenge +
	// loopback redirect make it long and unreadable, and the browser is
	// opened for the user anyway. The full URL is only printed below as a
	// fallback when the browser can't be opened.
	fmt.Fprintf(outW, "Logging in to: %s\n\n", baseURL)
	fmt.Fprint(outW, "Press Enter to open in browser...")

	// Read from /dev/tty so we get a real keypress and don't consume piped stdin.
	if err := waitForEnter(ctx); err != nil {
		return fmt.Errorf("wait for input: %w", err)
	}
	fmt.Fprintln(outW)

	if err := openURL(ctx, authURL); err != nil {
		fmt.Fprintf(errW, "Warning: failed to open browser: %v\n", err)
		fmt.Fprintf(outW, "Open this URL in your browser to sign in: %s\n", authURL)
	}

	fmt.Fprint(outW, "Waiting for sign-in... ")

	// The clock starts here, after the Enter prompt, so time spent reading
	// the prompt isn't counted against the sign-in itself.
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	code, err := flow.Wait(waitCtx)
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for sign-in after %v; run `entire login` again, or use `entire login --device`", waitTimeout)
		}
		return fmt.Errorf("complete login: %w", err)
	}

	token, refreshToken, err := flow.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, errW, baseURL, token, refreshToken)
}

// persistLogin validates the freshly-issued access token, saves it to the
// keyring, and dual-writes the shared contexts.json credential model.
// Shared by the device-code and browser flows.
func persistLogin(outW, errW io.Writer, baseURL, token, refreshToken string) error {
	if err := validateReceivedToken(token, baseURL, time.Now()); err != nil {
		return fmt.Errorf("reject login token: %w", err)
	}

	store := auth.NewStore()

	// Login deliberately uses the legacy SaveToken (string, string)
	// surface — we only have an access-token string at this point;
	// neither flow's client returns a TokenSet here.
	if err := store.SaveToken(baseURL, token); err != nil {
		return fmt.Errorf("save auth token: %w", err)
	}

	// Dual-write the shared contexts.json credential model so the git
	// remote helper (and entiredb's CLIs) can authenticate against any
	// entitled cluster from this login. Best-effort: the legacy entry
	// above remains the control-plane source of truth, so a failure here
	// must not fail the login — warn and continue.
	if _, err := auth.RecordLoginContext(token, refreshToken, true); err != nil {
		fmt.Fprintf(errW, "Warning: logged in, but could not record a shareable context (clone via entire:// may need a re-login): %v\n", err)
	}

	fmt.Fprintln(outW, "✓ Login complete.")
	return nil
}

// validateReceivedToken runs minimum-trust checks on the access token
// the AS handed us before we persist it. The server is the authority
// on signature/exp; this is defense in depth aimed at catching gross
// misbehaviour by a compromised or misconfigured AS (e.g. handing back
// a token from a different issuer than the one we asked, or one whose
// claims are already-expired).
//
// Opaque (non-JWT) tokens are permitted — the AS may not issue JWTs at
// all. Only when we can parse the token as a JWT do we cross-check the
// claims. Unsigned (alg:none) JWTs are always rejected via
// tokens.ErrUnsignedJWT; every other parse failure (3-segment-but-not-
// base64, payload-not-JSON, header-not-JSON, etc.) is treated as opaque
// and accepted, so a server issuing dot-bearing non-JWT bearer tokens
// can still log in.
func validateReceivedToken(rawToken, issuerURL string, now time.Time) error {
	claims, err := tokens.ParseClaims(rawToken)
	if errors.Is(err, tokens.ErrUnsignedJWT) {
		return err //nolint:wrapcheck // sentinel surfaces verbatim for caller's errors.Is
	}
	if err != nil {
		return nil //nolint:nilerr // any parse failure other than alg:none means the token isn't a JWT — opaque tokens are valid
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

// issMatches reports whether claimed equals expected after stripping path/
// query/fragment via api.OriginOnly, so "https://issuer/" and "https://issuer"
// match. Returns nil on match or when the iss claim is empty (some servers
// omit it — the server still does the real check on every request).
func issMatches(claimed, expected string) error {
	if claimed == "" {
		return nil
	}
	normClaimed := api.OriginOnly(claimed)
	normExpected := api.OriginOnly(expected)
	if normClaimed != normExpected {
		return fmt.Errorf("iss mismatch: token claims %q, expected %q", normClaimed, normExpected)
	}
	return nil
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

// waitForEnter reads a line from /dev/tty, blocking until the user presses Enter.
// If /dev/tty cannot be opened (e.g. on Windows), it returns immediately.
// Returns ctx.Err() if the context is cancelled before the user presses Enter.
func waitForEnter(ctx context.Context) error {
	// Under test (in-process go test, or a child with ENTIRE_TEST_TTY set)
	// don't block on a real /dev/tty read — tests that force interactive
	// mode still need this prompt to return. Mirrors openBrowser's guard.
	if interactive.UnderTest() {
		return nil
	}

	tty, err := os.Open("/dev/tty")
	if err != nil {
		return nil //nolint:nilerr // tty unavailable (e.g. Windows) — skip prompt silently
	}

	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(tty)
		_, err := reader.ReadString('\n')
		done <- err
	}()

	select {
	case <-ctx.Done():
		// Close tty to unblock the reading goroutine.
		_ = tty.Close()
		return fmt.Errorf("interrupted: %w", ctx.Err())
	case <-done:
		_ = tty.Close()
		return nil
	}
}

func openBrowser(ctx context.Context, browserURL string) error {
	u, err := url.Parse(browserURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("refusing to open non-HTTP URL: %s", browserURL)
	}

	// Under test there's no usable browser, and we must not spawn a real one
	// on a dev/CI host. Report failure so the caller takes the "here's the
	// URL" fallback — exactly the path a genuinely headless machine hits, and
	// what lets an integration test recover the loopback callback URL from
	// stdout. URL validation above still applies.
	if interactive.UnderTest() {
		return errors.New("browser unavailable under test")
	}

	var command string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{browserURL}
	case "linux":
		command = "xdg-open"
		args = []string{browserURL}
	case "windows":
		command = "cmd"
		args = []string{"/c", "start", "", browserURL}
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command %q: %w", command, err)
	}

	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release browser process: %w", err)
	}

	return nil
}
