//go:build e2e

package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/e2e/entire"
	"github.com/mxschmitt/playwright-go"
)

// deviceLogin runs `entire login --device` against production and completes
// the approval in a headless browser: GitHub sign-in as the test user with
// its password and authenticator code, then the Authorize button on Entire's
// device page. The login runs in dir, outside any repository checkout. The
// returned error never carries the password, the TOTP secret, the device
// code, or the approval URL.
func deviceLogin(ctx context.Context, dir, username, password, totpSecret string) error {
	cmd := execx.NonInteractive(ctx, entire.BinPath(), "login", "--device")
	cmd.Dir = dir
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start entire login: %w", err)
	}

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	exited := make(chan struct{})
	reader := bufio.NewReader(stdoutPipe)

	code, approvalURL, err := readDevicePrompt(reader)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("%w\nstderr:\n%s", err, stderr.String())
	}
	redact := strings.NewReplacer(code, "[device code]", approvalURL, "[approval url]", password, "[password]", totpSecret, "[totp secret]")

	go func() {
		// Drain before Wait: Wait closes the pipe.
		rest, _ := io.ReadAll(reader)
		done <- result{output: string(rest), err: cmd.Wait()}
		close(exited)
	}()

	approveErr := approveInBrowser(ctx, exited, approvalURL, code, username, password, totpSecret)
	if approveErr != nil {
		_ = cmd.Process.Kill()
	}
	res := <-done
	if approveErr != nil {
		return fmt.Errorf("browser approval: %s", redact.Replace(approveErr.Error()))
	}
	if res.err != nil {
		return fmt.Errorf("entire login: %w\n%s", res.err, redact.Replace(res.output+stderr.String()))
	}
	// The success mark shares its physical line with "Waiting for approval… ",
	// and reads "Login complete (signed in at <core>)." when the login server
	// dispatched to a regional core.
	if !strings.Contains(res.output, "✓ Login complete") {
		return fmt.Errorf("entire login exited 0 without reporting completion:\n%s", redact.Replace(res.output+stderr.String()))
	}
	fmt.Println("control-plane e2e: logged in")
	return nil
}

// readDevicePrompt reads the device code and the approval URL from the login
// output. The CLI prints "Device code: <code>", then "Login URL:" followed by
// the URL on the next line.
func readDevicePrompt(stdout *bufio.Reader) (code, approvalURL string, err error) {
	wantURL := false
	for {
		line, err := stdout.ReadString('\n')
		if err != nil {
			return "", "", fmt.Errorf("entire login ended before printing the device prompt: %w", err)
		}
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Device code: "):
			code = strings.TrimPrefix(line, "Device code: ")
		case line == "Login URL:":
			wantURL = true
		case wantURL && line != "":
			approvalURL = line
			wantURL = false
		}
		if code != "" && approvalURL != "" {
			return code, approvalURL, nil
		}
	}
}

// approveInBrowser drives the approval page until the login process exits.
func approveInBrowser(ctx context.Context, exited <-chan struct{}, approvalURL, code, username, password, totpSecret string) error {
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("start playwright (install it with `go run github.com/mxschmitt/playwright-go/cmd/playwright install chromium`): %w", err)
	}
	defer func() { _ = pw.Stop() }()
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		return fmt.Errorf("launch chromium: %w", err)
	}
	defer func() { _ = browser.Close() }()
	page, err := browser.NewPage()
	if err != nil {
		return fmt.Errorf("open page: %w", err)
	}
	page.SetDefaultTimeout(10_000)
	if _, err := page.Goto(approvalURL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return fmt.Errorf("open approval page: %w", err)
	}

	a := &approver{page: page, code: code, username: username, password: password, totpSecret: totpSecret}
	tick := time.NewTicker(750 * time.Millisecond)
	defer tick.Stop()
	// A page the stepper cannot advance (a sign-in form shown again, an
	// authenticator prompt that reloaded without an error) is reported by
	// name instead of surfacing as the context deadline.
	const stallAfter = time.Minute
	lastURL, lastChange := "", time.Now()
	for {
		select {
		case <-exited:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("login did not complete: %w", ctx.Err())
		case <-tick.C:
		}
		if u := a.page.URL(); u != lastURL {
			lastURL, lastChange = u, time.Now()
		} else if time.Since(lastChange) > stallAfter {
			return fmt.Errorf("login stalled on %s for %s", pageName(u), stallAfter)
		}
		if err := a.step(); err != nil {
			return err
		}
	}
}

// pageName is a page URL without its query, which on the device page carries
// the code. A host-less URL such as about:blank is named by scheme and opaque
// part instead of the raw string, which could carry parameters.
func pageName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "an unparseable page"
	}
	if u.Host == "" && u.Path == "" {
		return u.Scheme + ":" + u.Opaque
	}
	return u.Host + u.Path
}

// approver takes one action per step on whichever page the login flow is
// showing; the caller waits between steps for redirects to settle.
type approver struct {
	page                           playwright.Page
	code                           string
	username, password, totpSecret string
	submitted, otpSubmitted        bool
}

var (
	gitHubBadCredentials = regexp.MustCompile(`(?i)incorrect username or password`)
	gitHubVerification   = regexp.MustCompile(`(?i)verify your identity|enter.*(authentication|verification) code|captcha`)
	gitHubVerifyPaths    = regexp.MustCompile(`/sessions/verified-device|/sudo`)
	gitHubOTPRejected    = regexp.MustCompile(`(?i)two-factor authentication failed`)
	gitHubSignIn         = regexp.MustCompile(`(?i)^sign in$`)
	gitHubVerify         = regexp.MustCompile(`(?i)^verify$`)
	gitHubAuthenticator  = regexp.MustCompile(`(?i)authenticator app`)
	gitHubAuthorizeApp   = regexp.MustCompile(`(?i)^authorize .*entire`)
)

func (a *approver) step() error {
	current, err := url.Parse(a.page.URL())
	if err != nil {
		return fmt.Errorf("page url: %w", err)
	}
	var stepErr error
	switch {
	case current.String() == "about:blank":
		return nil
	case current.Scheme == "https" && current.Host == "github.com":
		stepErr = a.stepGitHub(current)
	case current.Scheme == "https" && (current.Host == "entire.io" || strings.HasSuffix(current.Host, ".entire.io")):
		stepErr = a.stepEntire()
	default:
		return fmt.Errorf("login redirected outside GitHub or Entire: %s", current.Host)
	}
	// A redirect can replace the document between inspecting and acting on it;
	// the next tick sees the new page.
	if stepErr != nil && isContextDestroyed(stepErr) {
		return nil
	}
	return stepErr
}

func (a *approver) stepGitHub(current *url.URL) error {
	body, err := a.page.Locator("body").InnerText()
	if err != nil {
		return err
	}
	if gitHubBadCredentials.MatchString(body) {
		return errors.New("GitHub rejected the username or password")
	}
	if strings.HasPrefix(current.Path, "/sessions/two-factor") {
		return a.stepTwoFactor(body)
	}
	if gitHubVerifyPaths.MatchString(current.Path) || gitHubVerification.MatchString(body) {
		return errors.New("GitHub requires additional verification; username/password cannot complete this login")
	}

	login := a.page.Locator(`input[name="login"]`)
	if visible, _ := login.IsVisible(); visible && !a.submitted {
		if err := login.Fill(a.username); err != nil {
			return err
		}
		if err := a.page.Locator(`input[name="password"]`).Fill(a.password); err != nil {
			return err
		}
		a.submitted = true
		return a.page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: gitHubSignIn}).Click()
	}
	// First-time OAuth consent for the Entire app.
	authorize := a.page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: gitHubAuthorizeApp}).First()
	if visible, _ := authorize.IsVisible(); visible {
		if enabled, _ := authorize.IsEnabled(); enabled {
			return authorize.Click()
		}
	}
	return nil
}

// stepTwoFactor answers GitHub's authenticator-app prompt with the current
// TOTP code. GitHub auto-submits a complete code, so the Verify button is
// only clicked when it is still there.
func (a *approver) stepTwoFactor(body string) error {
	if gitHubOTPRejected.MatchString(body) {
		return errors.New("GitHub rejected the one-time code; check the TOTP secret and the clock")
	}
	otp := a.page.Locator(`input[name="app_otp"], input[autocomplete="one-time-code"]`).First()
	if visible, _ := otp.IsVisible(); !visible {
		// GitHub offered another second factor first; switch to the app.
		app := a.page.GetByRole(*playwright.AriaRoleLink, playwright.PageGetByRoleOptions{Name: gitHubAuthenticator}).
			Or(a.page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: gitHubAuthenticator})).First()
		if visible, _ := app.IsVisible(); visible {
			return app.Click()
		}
		return nil
	}
	if a.otpSubmitted {
		return nil
	}
	code, err := totpCode(a.totpSecret, time.Now())
	if err != nil {
		return err
	}
	if err := otp.Fill(code); err != nil {
		return err
	}
	a.otpSubmitted = true
	verify := a.page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: gitHubVerify})
	if visible, _ := verify.IsVisible(); visible {
		if enabled, _ := verify.IsEnabled(); enabled {
			return verify.Click()
		}
	}
	return nil
}

func (a *approver) stepEntire() error {
	// The device page comes back from GitHub with the code prefilled; fill it
	// only when the inputs are visibly empty.
	part1 := a.page.GetByLabel("Code part 1")
	if visible, _ := part1.IsVisible(); visible {
		if value, _ := part1.InputValue(); value == "" {
			left, right, _ := strings.Cut(a.code, "-")
			if err := part1.Fill(left); err != nil {
				return err
			}
			if err := a.page.GetByLabel("Code part 2").Fill(right); err != nil {
				return err
			}
		}
	}
	authorize := a.page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: "Authorize", Exact: playwright.Bool(true)})
	if visible, _ := authorize.IsVisible(); visible {
		if enabled, _ := authorize.IsEnabled(); enabled {
			return authorize.Click()
		}
	}
	return nil
}

func isContextDestroyed(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Execution context was destroyed") || strings.Contains(msg, "Cannot find context")
}
