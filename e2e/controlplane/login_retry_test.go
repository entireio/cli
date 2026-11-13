//go:build e2e

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/e2e/entire"
	"github.com/mxschmitt/playwright-go"
)

const (
	// GitHub occasionally leaves a headless sign-in from a CI runner on a page
	// the approver cannot advance; a fresh browser usually gets through.
	loginAttempts = 3
	// Each attempt can spend up to a minute on a stalled page before giving up.
	loginAttemptTimeout = 3 * time.Minute
	// Long enough for a new TOTP window: GitHub refuses a code it has
	// already accepted.
	loginRetryDelay = 30 * time.Second
	// The body text kept in a diagnosis, in bytes.
	diagnosisBodyLimit = 4000
)

// errLoginPermanent marks a login failure that another attempt cannot fix.
var errLoginPermanent = errors.New("not retried")

// loginWithRetry runs deviceLogin up to loginAttempts times. Every failed
// attempt that reached the browser leaves a description of the page it gave
// up on in runDir/login-attempt-<n>.txt and on stderr.
func loginWithRetry(ctx context.Context, dir, runDir, username, password, totpSecret string) error {
	var errs []error
	for attempt := 1; attempt <= loginAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return errors.Join(append(errs, ctx.Err())...)
			case <-time.After(loginRetryDelay):
			}
			// A failed attempt may still have stored a session (for example, a
			// completion line the check did not recognize); revoke it rather
			// than leave it live on the shared account.
			logoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			logout := execx.NonInteractive(logoutCtx, entire.BinPath(), "logout")
			logout.Dir = dir
			_ = logout.Run()
			cancel()
		}

		attemptCtx, cancel := context.WithTimeout(ctx, loginAttemptTimeout)
		diagnosis, err := deviceLogin(attemptCtx, dir, username, password, totpSecret)
		cancel()
		if err == nil {
			return nil
		}
		err = fmt.Errorf("attempt %d/%d: %w", attempt, loginAttempts, err)
		errs = append(errs, err)
		fmt.Fprintf(os.Stderr, "control-plane e2e: login %v\n", err)
		if diagnosis != "" {
			fmt.Fprintf(os.Stderr, "control-plane e2e: page at failure:\n%s\n", diagnosis)
			path := filepath.Join(runDir, fmt.Sprintf("login-attempt-%d.txt", attempt))
			if writeErr := os.WriteFile(path, []byte(err.Error()+"\n\n"+diagnosis), 0o644); writeErr != nil {
				fmt.Fprintf(os.Stderr, "control-plane e2e: write %s: %v\n", path, writeErr)
			}
		}
		if errors.Is(err, errLoginPermanent) {
			break
		}
	}
	return errors.Join(errs...)
}

// diagnose describes the page the approver gave up on: where the login went,
// GitHub's alerts, the buttons on offer, embedded frames (a captcha loads in
// one, outside the body text), and the visible text. It is text only: this
// repository's CI artifacts are public and a screenshot cannot be redacted.
// The result is unredacted; deviceLogin redacts it.
func (a *approver) diagnose() string {
	page := a.page
	var b strings.Builder
	fmt.Fprintf(&b, "page: %s\n", pageName(page.URL()))
	if title, err := page.Title(); err == nil {
		fmt.Fprintf(&b, "title: %s\n", title)
	}
	writeSection(&b, "pages visited", a.visited)
	if alerts, err := page.Locator(`[role="alert"], .flash, .flash-error, .js-flash-alert`).AllInnerTexts(); err == nil {
		writeSection(&b, "alerts", alerts)
	}
	writeSection(&b, "visible buttons", visibleButtons(page))
	var frames []string
	for _, f := range page.Frames() {
		if f != page.MainFrame() {
			frames = append(frames, pageName(f.URL()))
		}
	}
	writeSection(&b, "frames", frames)
	body, err := page.Locator("body").InnerText(playwright.LocatorInnerTextOptions{Timeout: playwright.Float(2000)})
	if err != nil {
		body = "(unreadable: " + err.Error() + ")"
	}
	if len(body) > diagnosisBodyLimit {
		body = body[:diagnosisBodyLimit] + "\n[truncated]"
	}
	fmt.Fprintf(&b, "\nbody text:\n%s\n", body)
	return b.String()
}

// visibleButtons names the visible buttons, including GitHub's
// <input type="submit"> sign-in button, which has no inner text.
func visibleButtons(page playwright.Page) []string {
	raw, err := page.Locator(`button:visible, input[type="submit"]:visible`).
		EvaluateAll(`els => els.map(e => (e.innerText || e.value || e.getAttribute("aria-label") || "").trim())`)
	if err != nil {
		return nil
	}
	items, _ := raw.([]interface{})
	names := make([]string, 0, len(items))
	for _, item := range items {
		if name, ok := item.(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func writeSection(b *strings.Builder, title string, lines []string) {
	fmt.Fprintf(b, "\n%s:\n", title)
	if len(lines) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, line := range lines {
		fmt.Fprintf(b, "  %s\n", strings.Join(strings.Fields(line), " "))
	}
}
