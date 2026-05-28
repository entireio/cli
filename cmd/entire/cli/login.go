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

	"github.com/atotto/clipboard"
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

// clipboardWriteFunc is the signature for copying text to the user's clipboard.
type clipboardWriteFunc func(text string) error

// deviceAuthClient abstracts the auth client so runLogin and waitForApproval can be unit-tested.
type deviceAuthClient interface {
	StartDeviceAuth(ctx context.Context) (*auth.DeviceAuthStart, error)
	PollDeviceAuth(ctx context.Context, deviceCode string) (*auth.DeviceAuthPoll, error)
	BaseURL() string
}

func newLoginCmd() *cobra.Command {
	var insecureHTTPAuth bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			return runLogin(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), auth.NewClient(nil, insecureHTTPAuth), openBrowser, copyToClipboard)
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

func runLogin(ctx context.Context, outW, errW io.Writer, client deviceAuthClient, openURL browserOpenFunc, writeClipboard clipboardWriteFunc) error {
	start, err := client.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	fmt.Fprintf(outW, "Device code: %s\n", start.UserCode)

	approvalURL := chooseApprovalURL(start)

	if interactive.CanPromptInteractively() {
		fmt.Fprintf(outW, "Press Enter to copy the code to your clipboard, open %s in your browser, and enter the generated device code...", approvalURL)

		// Read from /dev/tty so we get a real keypress and don't consume piped stdin.
		if err := waitForEnter(ctx); err != nil {
			return fmt.Errorf("wait for input: %w", err)
		}

		fmt.Fprintln(outW)
		if copied := copyDeviceCodeToClipboard(errW, start.UserCode, writeClipboard); copied {
			fmt.Fprintln(outW, "Device code copied to clipboard.")
		}

		if err := openURL(ctx, approvalURL); err != nil {
			fmt.Fprintf(errW, "Warning: failed to open browser: %v\n", err)
			fmt.Fprintf(outW, "Open the approval URL in your browser to continue and enter the generated device code: %s\n", approvalURL)
		}
	} else {
		fmt.Fprintf(outW, "Approval URL: %s\n", approvalURL)
	}

	fmt.Fprintln(outW, "Waiting for approval...")

	token, err := waitForApproval(ctx, client, start.DeviceCode, start.ExpiresIn, time.Duration(start.Interval)*time.Second, defaultSlowDownBackoff)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	if err := validateReceivedToken(token, client.BaseURL(), time.Now()); err != nil {
		return fmt.Errorf("reject login token: %w", err)
	}

	store := auth.NewStore()

	// Login deliberately uses the legacy SaveToken (string, string)
	// surface — we only have an access-token string at this point;
	// the deviceflow client doesn't return a TokenSet here.
	if err := store.SaveToken(client.BaseURL(), token); err != nil {
		return fmt.Errorf("save auth token: %w", err)
	}

	fmt.Fprintln(outW, "Login complete.")
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

func copyDeviceCodeToClipboard(errW io.Writer, userCode string, writeClipboard clipboardWriteFunc) bool {
	if writeClipboard == nil {
		return false
	}
	if err := writeClipboard(userCode); err != nil {
		fmt.Fprintf(errW, "Warning: failed to copy device code to clipboard: %v\n", err)
		return false
	}
	return true
}

func copyToClipboard(text string) error {
	if err := clipboard.WriteAll(text); err != nil {
		return fmt.Errorf("write clipboard: %w", err)
	}
	return nil
}

func waitForApproval(ctx context.Context, poller deviceAuthClient, deviceCode string, expiresIn int, interval, slowDownBackoff time.Duration) (string, error) {
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
			return "", errors.New("device authorization expired")
		}

		result, err := poller.PollDeviceAuth(ctx, deviceCode)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxTransientErrors {
				return "", fmt.Errorf("poll approval status (after %d consecutive failures): %w", consecutiveErrors, err)
			}
			// Transient error — wait and retry.
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("wait for approval: %w", ctx.Err())
			case <-time.After(pollInterval):
			}
			continue
		}
		consecutiveErrors = 0

		switch result.Error {
		case "":
			if result.AccessToken == "" {
				return "", errors.New("device authorization completed without a token")
			}
			return result.AccessToken, nil
		case "authorization_pending":
			// no-op, will sleep and retry below
		case "slow_down":
			pollInterval += slowDownBackoff
			if pollInterval > maxPollInterval {
				pollInterval = maxPollInterval
			}
		case "access_denied":
			return "", errors.New("device authorization denied")
		case "expired_token":
			return "", errors.New("device authorization expired")
		default:
			if result.ErrorDescription != "" {
				return "", fmt.Errorf("device authorization failed: %s: %s", result.Error, result.ErrorDescription)
			}
			return "", fmt.Errorf("device authorization failed: %s", result.Error)
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for approval: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// waitForEnter reads a line from /dev/tty, blocking until the user presses Enter.
// If /dev/tty cannot be opened (e.g. on Windows), it returns immediately.
// Returns ctx.Err() if the context is cancelled before the user presses Enter.
func waitForEnter(ctx context.Context) error {
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
