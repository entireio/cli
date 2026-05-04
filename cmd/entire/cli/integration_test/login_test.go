//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

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
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/device/code":
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

			writeJSON(t, w, http.StatusOK, map[string]any{"access_token": "local-token", "token_type": "Bearer", "expires_in": 3600, "scope": "cli"})
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
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/device/code":
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
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/device/code":
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

type loginProcess struct {
	stdout *bufio.Reader
	waitFn func() (string, error)
}

func runLoginProcess(t *testing.T, apiBaseURL string) *loginProcess {
	t.Helper()

	env := NewTestEnv(t)

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "login", "--insecure-http-auth")
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+env.GeminiProjectDir,
		"ENTIRE_TEST_OPENCODE_PROJECT_DIR="+env.OpenCodeProjectDir,
		"ENTIRE_API_BASE_URL="+apiBaseURL,
	)

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
		stdout: reader,
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
		case strings.HasPrefix(line, "Approval URL: "):
			approvalURL = strings.TrimPrefix(line, "Approval URL: ")
		}

		if approvalURL != "" && deviceCode != "" {
			return approvalURL, deviceCode
		}
	}

	t.Fatal("timed out waiting for login prompt output")
	return "", ""
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
