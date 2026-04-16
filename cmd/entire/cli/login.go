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
			client := auth.NewClient(nil)

			if !insecureHTTPAuth {
				if err := api.RequireSecureURL(client.BaseURL()); err != nil {
					return fmt.Errorf("base URL check: %w", err)
				}
			}

			return runLogin(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), client, openBrowser)
		},
	}

	cmd.Flags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false, "Allow authentication over plain HTTP (insecure, for local development only)")
	if err := cmd.Flags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	return cmd
}

func runLogin(ctx context.Context, outW, errW io.Writer, client deviceAuthClient, openURL browserOpenFunc) error {
	start, err := client.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	fmt.Fprintf(outW, "Device code: %s\n", start.UserCode)

	approvalURL := start.VerificationURI

	if interactive.CanPromptInteractively() {
		fmt.Fprintf(outW, "Press Enter to open %s in your browser and enter the generated device code...", approvalURL)

		// Read from /dev/tty so we get a real keypress and don't consume piped stdin.
		if err := waitForEnter(ctx); err != nil {
			return fmt.Errorf("wait for input: %w", err)
		}

		fmt.Fprintln(outW)

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

	store := auth.NewStore()

	if err := store.SaveToken(client.BaseURL(), token); err != nil {
		return fmt.Errorf("save auth token: %w", err)
	}

	fmt.Fprintln(outW, "Login complete.")
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
