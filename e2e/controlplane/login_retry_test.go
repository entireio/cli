//go:build e2e

package controlplane

import (
	"context"
	"fmt"
	"os"
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
	// The between-attempt logout's budget.
	retryLogoutTimeout = 30 * time.Second
	// The redacted diagnosis kept per attempt, in bytes.
	diagnosisLimit = 6000
)

// loginWithRetry runs deviceLogin up to loginAttempts times; see retryLogin.
func loginWithRetry(ctx context.Context, dir, runDir, username, password, totpSecret string) error {
	policy := retryPolicy{attempts: loginAttempts, attemptTimeout: loginAttemptTimeout, delay: loginRetryDelay, runDir: runDir, log: os.Stderr}
	login := func(ctx context.Context) (string, error) {
		return deviceLogin(ctx, dir, username, password, totpSecret)
	}
	// A failed attempt may still have stored a session (for example, a
	// completion line the check did not recognize); revoke it rather than
	// leave it live on the shared account, and say so when that fails.
	logout := func(ctx context.Context) {
		logoutCtx, cancel := context.WithTimeout(ctx, retryLogoutTimeout)
		defer cancel()
		cmd := execx.NonInteractive(logoutCtx, entire.BinPath(), "logout")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "control-plane e2e: logout before retry failed: %v\n%s", err, out)
		}
	}
	return retryLogin(ctx, policy, login, logout)
}

// diagnose describes the page the approver gave up on: where the login went,
// GitHub's alerts, the buttons on offer, embedded frames (a captcha loads in
// one, outside the body text), and the visible text. It is text only: this
// repository's CI artifacts are public and a screenshot cannot be redacted.
// The result is unredacted and untruncated; deviceLogin redacts it, then
// truncates it.
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
