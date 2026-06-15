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
		case r.Method == http.MethodPost && r.URL.Path == "/device_authorization":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-123",
				"user_code":                 "ABCD-EFGH",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=ABCD-EFGH"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
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

	proc := runLoginProcess(t, server.URL)

	approvalURL, deviceCode := waitForLoginPrompt(t, proc.stdout)
	if deviceCode != "ABCD-EFGH" {
		t.Fatalf("device code = %q, want %q", deviceCode, "ABCD-EFGH")
	}

	if !strings.HasPrefix(approvalURL, server.URL+"/") {
		t.Fatalf("approval URL = %q, want prefix %q", approvalURL, server.URL+"/")
	}

	approveReq, reqErr := http.NewRequest(http.MethodPost, approvalURL, http.NoBody)
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

	if !strings.Contains(output, "Waiting for approval...") {
		t.Fatalf("output missing wait message:\n%s", output)
	}

	if !strings.Contains(output, "Login complete.") {
		t.Fatalf("output missing login complete message (token save likely failed):\n%s", output)
	}

	// A --server login is recorded as a contexts.json context (the legacy
	// keyring entry is only written for the default login server, whose key
	// is the only one legacy readers consult).
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
		case r.Method == http.MethodPost && r.URL.Path == "/device_authorization":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-expired",
				"user_code":                 "WXYZ-0000",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=WXYZ-0000"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
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
		case r.Method == http.MethodPost && r.URL.Path == "/device_authorization":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_code":               "device-denied",
				"user_code":                 "QRST-9999",
				"verification_uri":          serverURLWithPath(r, "/approve"),
				"verification_uri_complete": serverURLWithPath(r, "/approve?code=QRST-9999"),
				"expires_in":                10,
				"interval":                  1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
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
// flow end to end: ENTIRE_TEST_TTY=1 forces the interactive (browser)
// default, openBrowser reports failure under test (no usable browser on a
// headless host) so the flow prints the fallback URL, and the test plays
// the role of the browser by parsing that URL and GETting the loopback
// callback with a code + the state from it.
func TestLogin_BrowserFlow_SavesToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/token" {
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

	// ENTIRE_AUTH_BASE_URL is retired (commands reject it when set at all);
	// --server is how a login targets the test server instead of the
	// production default.
	args = append(args, "--server", apiBaseURL)
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+env.GeminiProjectDir,
		"ENTIRE_TEST_OPENCODE_PROJECT_DIR="+env.OpenCodeProjectDir,
		"ENTIRE_API_BASE_URL="+apiBaseURL,
		"ENTIRE_TEST_AUTH_STORE_FILE="+filepath.Join(env.RepoDir, ".entire-test-auth-store.json"),
		// A --server login records its credential in contexts.json and the
		// token store; point both at the test sandbox so the spawned binary
		// can't touch the real ~/.config/entire or the OS keychain.
		"ENTIRE_CONFIG_DIR="+configDir,
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+filepath.Join(env.RepoDir, ".entire-test-tokens.json"),
		// Blank the SSH_* vars inherited from os.Environ(): a developer
		// running tests over SSH would otherwise flip the subprocess'
		// isSSHSession() detection and route browser-flow tests to the
		// device flow. extraEnv is appended after, so a test can still
		// set them deliberately.
		"SSH_CONNECTION=", "SSH_CLIENT=", "SSH_TTY=",
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

	for time.Now().Before(deadline) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("read login output: %v", err)
		}

		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Device code: "):
			deviceCode = strings.TrimPrefix(line, "Device code: ")
		case strings.HasPrefix(line, "Login URL:"):
			approvalURL = strings.TrimSpace(strings.TrimPrefix(line, "Login URL:"))
		}

		if approvalURL != "" && deviceCode != "" {
			return approvalURL, deviceCode
		}
	}

	t.Fatal("timed out waiting for login prompt output")
	return "", ""
}

// waitForBrowserPrompt reads login stdout until it finds the
// "Open this URL in your browser to sign in: <url>" fallback line and
// returns the URL. Under test openBrowser reports failure (no usable
// browser on a headless host), so the browser flow always prints this
// fallback — which is how the test recovers the ephemeral callback URL.
func waitForBrowserPrompt(t *testing.T, stdout *bufio.Reader) string {
	t.Helper()

	const prefix = "Open this URL in your browser to sign in: "
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("read login output: %v", err)
		}
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return after
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
