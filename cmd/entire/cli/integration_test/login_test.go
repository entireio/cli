//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// fakeLoginJWT builds a JWT-shaped access token with a junk signature
// (ParseClaims doesn't verify signatures) whose iss matches the test server
// origin, so login's iss cross-check passes and the context can be recorded.
// A bare opaque token is no longer enough for a --server login: with no iss
// claim there is nothing to key the login context by, and login fails.
func fakeLoginJWT(iss string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := enc.EncodeToString(fmt.Appendf(nil,
		`{"iss":%q,"sub":"user-123","exp":%d}`, iss, time.Now().Add(time.Hour).Unix()))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

func TestLogin_SavesTokenAfterApproval(t *testing.T) {
	t.Parallel()

	type state struct {
		sync.Mutex

		approved bool
		polls    int
	}

	serverState := &state{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == pathDeviceAuthorization:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-123",
				"user_code":                 "ABCD-EFGH",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=ABCD-EFGH"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == pathOAuthToken:
			serverState.Lock()
			serverState.polls++
			approved := serverState.approved
			serverState.Unlock()

			if !approved {
				writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
				return
			}

			writeJSON(t, w, http.StatusOK, map[string]any{"access_token": fakeLoginJWT("http://" + r.Host), "token_type": "Bearer", "expires_in": 3600, "scope": "cli"})
		case r.Method == http.MethodPost && r.URL.Path == "/approve":
			serverState.Lock()
			serverState.approved = true
			serverState.Unlock()
			writeJSON(t, w, http.StatusOK, map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Force the interactive device flow. The subprocess cannot read a real TTY
	// under test, so successful completion proves polling began without a key.
	proc := startLoginProcess(t, server.URL, []string{"ENTIRE_TEST_TTY=1"}, "login", "--device", "--insecure-http-auth")

	approvalURL, deviceCode := waitForLoginPrompt(t, proc.stdout)
	if deviceCode != "ABCD-EFGH" {
		t.Fatalf("device code = %q, want %q", deviceCode, "ABCD-EFGH")
	}

	if !strings.HasPrefix(approvalURL, server.URL+"/") {
		t.Fatalf("approval URL = %q, want prefix %q", approvalURL, server.URL+"/")
	}

	approveReq, reqErr := http.NewRequestWithContext(t.Context(), http.MethodPost, approvalURL, http.NoBody)
	if reqErr != nil {
		t.Fatalf("create approve request: %v", reqErr)
	}

	approveResp, doErr := http.DefaultClient.Do(approveReq)
	if doErr != nil {
		t.Fatalf("approve request failed: %v", doErr)
	}
	_ = approveResp.Body.Close()

	output, waitErr := proc.wait()
	if waitErr != nil {
		t.Fatalf("login command failed: %v\nOutput:\n%s", waitErr, output)
	}

	if !strings.Contains(output, "Waiting for approval…") {
		t.Fatalf("output missing wait message:\n%s", output)
	}

	if !strings.Contains(output, "Login complete.") {
		t.Fatalf("output missing login complete message (token save likely failed):\n%s", output)
	}

	// The login is recorded as a contexts.json context — the only
	// credential store.
	contextsPath := filepath.Join(proc.configDir, "contexts.json")
	data, readErr := os.ReadFile(contextsPath)
	if readErr != nil {
		t.Fatalf("read %s after login: %v", contextsPath, readErr)
	}
	if !strings.Contains(string(data), server.URL) {
		t.Fatalf("contexts.json does not reference login server %s:\n%s", server.URL, data)
	}

	serverState.Lock()
	polls := serverState.polls
	serverState.Unlock()
	if polls == 0 {
		t.Fatal("expected at least one poll request")
	}
}

func TestLogin_ExpiredFlow(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == pathDeviceAuthorization:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-expired",
				"user_code":                 "WXYZ-0000",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=WXYZ-0000"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == pathOAuthToken:
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "expired_token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	proc := runLoginProcess(t, server.URL)
	_, _ = waitForLoginPrompt(t, proc.stdout)

	output, err := proc.wait()
	if err == nil {
		t.Fatalf("expected login to fail for expired flow\nOutput:\n%s", output)
	}

	if !strings.Contains(output, "device authorization expired") {
		t.Fatalf("expected expired message, got:\n%s", output)
	}

	if strings.Contains(output, "Login complete.") {
		t.Fatal("output should NOT contain login complete for expired flow")
	}
}

func TestLogin_DeniedFlow(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == pathDeviceAuthorization:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-denied",
				"user_code":                 "QRST-9999",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=QRST-9999"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == pathOAuthToken:
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "access_denied"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	proc := runLoginProcess(t, server.URL)
	_, _ = waitForLoginPrompt(t, proc.stdout)

	output, err := proc.wait()
	if err == nil {
		t.Fatalf("expected login to fail for denied flow\nOutput:\n%s", output)
	}

	if !strings.Contains(output, "device authorization denied") {
		t.Fatalf("expected denied message, got:\n%s", output)
	}

	if strings.Contains(output, "Login complete.") {
		t.Fatal("output should NOT contain login complete for denied flow")
	}
}

// TestLogin_BrowserFlow_SavesToken drives the loopback authorization-code
// flow end to end: ENTIRE_TEST_TTY=1 forces the interactive browser default,
// terminal actions are disabled under test, and the test completes sign-in
// solely through the always-visible URL and loopback callback.
func TestLogin_BrowserFlow_SavesToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == pathOAuthToken {
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			if got := r.PostForm.Get("grant_type"); got != "authorization_code" {
				t.Errorf("grant_type = %q, want authorization_code", got)
			}
			if r.PostForm.Get("code_verifier") == "" {
				t.Error("token request missing code_verifier")
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"access_token": fakeLoginJWT("http://" + r.Host), "token_type": "Bearer", "expires_in": 3600, "scope": "cli offline_access",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	proc := startLoginProcess(t, server.URL, []string{"ENTIRE_TEST_TTY=1"}, "login", "--insecure-http-auth")

	authURL := waitForBrowserPrompt(t, proc.stdout)
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorization URL %q: %v", authURL, err)
	}
	q := u.Query()
	redirectURI, state := q.Get("redirect_uri"), q.Get("state")
	if redirectURI == "" || state == "" {
		t.Fatalf("authorization URL missing redirect_uri/state: %s", authURL)
	}

	cbResp, err := http.Get(redirectURI + "?" + url.Values{"code": {"auth-code-1"}, "state": {state}}.Encode()) //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET loopback callback: %v", err)
	}
	_ = cbResp.Body.Close()

	output, waitErr := proc.wait()
	if waitErr != nil {
		t.Fatalf("login command failed: %v\nOutput:\n%s", waitErr, output)
	}
	if !strings.Contains(output, "Login complete.") {
		t.Fatalf("output missing login complete message:\n%s", output)
	}
}

// TestLogin_NoDisplay_UsesDeviceFlow pins the no-display rule end to end: a
// prompt-capable Linux/BSD session (ENTIRE_TEST_TTY=1, no SSH variables) with
// no X11 or Wayland display, no $BROWSER and no WSL interop takes the
// device-code flow and says why. startLoginProcess gives every login a
// nominal DISPLAY so the browser-flow tests run on headless CI; this test
// blanks it again through extraEnv, which the harness appends last so it
// wins, the same way the harness itself blanks the SSH variables. Linux only:
// noLocalDisplay is false by construction on macOS and Windows, so there the
// same environment takes the browser flow and this assertion has no subject.
func TestLogin_NoDisplay_UsesDeviceFlow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("noLocalDisplay applies to Linux and the BSDs only")
	}
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == pathDeviceAuthorization:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":      "device-no-display",
				"user_code":        "NODI-SPLY",
				"verification_uri": serverURLWithPath(r, "/approve"),
				"expires_in":       10,
				"interval":         1,
			})
		case r.Method == http.MethodPost && r.URL.Path == pathOAuthToken:
			// Deny straight away: the assertion is about which flow was
			// chosen, and a denial ends the poll without an approval dance.
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "access_denied"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	proc := startLoginProcess(t, server.URL, []string{
		"ENTIRE_TEST_TTY=1",
		"DISPLAY=", "WAYLAND_DISPLAY=", "BROWSER=", "WSL_DISTRO_NAME=", "WSL_INTEROP=",
	}, "login", "--insecure-http-auth")

	// Fail fast if the browser flow was taken instead. waitForLoginPrompt
	// checks its deadline only between blocking reads, and the browser flow
	// prints nothing after its URL until browserLoginTimeout (five minutes, no
	// override) expires, so a regression here would block for that long. The
	// device flow's first stdout bytes are "Device code:" — runLogin writes
	// that before anything else, and runLoginAuto's explanation goes to
	// stderr — while the browser flow's are "Logging in to ". Peek does not
	// consume, so the prompt parser below still sees the whole line.
	if head, err := proc.stdout.Peek(len("Device code:")); err != nil || string(head) != "Device code:" {
		t.Fatalf("login did not take the device flow: first stdout bytes %q (%v)", head, err)
	}

	_, deviceCode := waitForLoginPrompt(t, proc.stdout)
	if deviceCode != "NODI-SPLY" {
		t.Fatalf("device code = %q, want %q", deviceCode, "NODI-SPLY")
	}

	output, err := proc.wait()
	if err == nil {
		t.Fatalf("expected login to fail once the device flow was denied\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "No graphical display detected") {
		t.Fatalf("output missing the no-display reason for taking the device flow:\n%s", output)
	}
	if !strings.Contains(output, "device authorization denied") {
		t.Fatalf("expected the device flow to run to its denial, got:\n%s", output)
	}
}

type loginProcess struct {
	stdout *bufio.Reader
	// configDir is the sandboxed ENTIRE_CONFIG_DIR the spawned binary writes
	// contexts.json into; tests can assert on its contents after login.
	configDir string
	waitFn    func() (string, error)
}

func runLoginProcess(t *testing.T, apiBaseURL string) *loginProcess {
	t.Helper()
	// No ENTIRE_TEST_TTY: NonInteractive + non-interactive default routes
	// `entire login` to the device-code flow.
	return startLoginProcess(t, apiBaseURL, nil, "login", "--insecure-http-auth")
}

func startLoginProcess(t *testing.T, apiBaseURL string, extraEnv []string, args ...string) *loginProcess {
	t.Helper()

	env := NewTestEnv(t)
	configDir := filepath.Join(env.RepoDir, ".entire-test-config")

	// --server pins the login at the in-process test server instead of the
	// production default. The login lands in contexts.json + the file token
	// store, both sandboxed below so the test never touches the developer's
	// real config or OS keychain.
	args = append(args, "--server", apiBaseURL)
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+env.GeminiProjectDir,
		"ENTIRE_TEST_OPENCODE_PROJECT_DIR="+env.OpenCodeProjectDir,
		"ENTIRE_API_BASE_URL="+apiBaseURL,
		// The login records its credential in contexts.json and the token
		// store; point both at the test sandbox so the spawned binary can't
		// touch the real ~/.config/entire or the OS keychain.
		"ENTIRE_CONFIG_DIR="+configDir,
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+filepath.Join(env.RepoDir, ".entire-test-tokens.json"),
		// Blank the SSH_* vars inherited from os.Environ(): a developer
		// running tests over SSH would otherwise flip the subprocess'
		// isSSHSession() detection and route browser-flow tests to the
		// device flow. extraEnv is appended after, so a test can still
		// set them deliberately.
		"SSH_CONNECTION=", "SSH_CLIENT=", "SSH_TTY=",
		// And give it a display: noLocalDisplay() routes a Linux/BSD process
		// with no DISPLAY or WAYLAND_DISPLAY to the device flow, which is
		// exactly what a headless CI runner looks like. The browser flow
		// under test never opens a browser (terminal actions are off under
		// test), so a nominal DISPLAY is enough to model the desktop these
		// tests simulate. Harmless for the device-flow tests, which are
		// routed by the absence of a TTY before the display is consulted.
		"DISPLAY=:0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v", err)
	}

	reader := bufio.NewReader(stdoutPipe)

	return &loginProcess{
		stdout:    reader,
		configDir: configDir,
		waitFn: func() (string, error) {
			stdoutBytes, readErr := io.ReadAll(reader)
			waitErr := cmd.Wait()
			return string(stdoutBytes) + stderr.String(), errors.Join(readErr, waitErr)
		},
	}
}

func (p *loginProcess) wait() (string, error) {
	return p.waitFn()
}

func waitForLoginPrompt(t *testing.T, stdout *bufio.Reader) (string, string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var approvalURL string
	var deviceCode string
	wantURL := false

	for time.Now().Before(deadline) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("read login output: %v", err)
		}

		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Device code: "):
			deviceCode = strings.TrimPrefix(line, "Device code: ")
		case line == "Login URL:":
			wantURL = true
		case wantURL && line != "":
			approvalURL = line
			wantURL = false
		}

		if approvalURL != "" && deviceCode != "" {
			return approvalURL, deviceCode
		}
	}

	t.Fatal("timed out waiting for login prompt output")
	return "", ""
}

// waitForBrowserPrompt reads login stdout until it finds the always-visible
// full browser authorization URL and returns it.
func waitForBrowserPrompt(t *testing.T, stdout *bufio.Reader) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	wantURL := false
	for time.Now().Before(deadline) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("read login output: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "Login URL:" {
			wantURL = true
			continue
		}
		if wantURL && line != "" {
			return line
		}
	}

	t.Fatal("timed out waiting for browser login prompt")
	return ""
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func serverURLWithPath(r *http.Request, path string) string {
	return fmt.Sprintf("http://%s%s", r.Host, path)
}
