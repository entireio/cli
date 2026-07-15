package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// mockClient implements deviceAuthClient for unit tests.
type mockClient struct {
	responses []pollResponse
	calls     int
}

type pollResponse struct {
	result *auth.DeviceAuthPoll
	err    error
}

func (m *mockClient) StartDeviceAuth(_ context.Context) (*auth.DeviceAuthStart, error) {
	return nil, errors.New("not implemented in mock")
}

func (m *mockClient) BaseURL() string {
	return "http://test"
}

func (m *mockClient) PollDeviceAuth(_ context.Context, _ string) (*auth.DeviceAuthPoll, error) {
	if m.calls >= len(m.responses) {
		return nil, errors.New("unexpected poll call")
	}
	r := m.responses[m.calls]
	m.calls++
	return r.result, r.err
}

func TestWaitForApproval_ImmediateSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-123"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-123" {
		t.Fatalf("token = %q, want %q", token, "tok-123")
	}
	if poller.calls != 1 {
		t.Fatalf("calls = %d, want 1", poller.calls)
	}
}

func TestWaitForApproval_PendingThenSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-456"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-456" {
		t.Fatalf("token = %q, want %q", token, "tok-456")
	}
	if poller.calls != 3 {
		t.Fatalf("calls = %d, want 3", poller.calls)
	}
}

func TestWaitForApproval_AccessDenied(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "access_denied"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "device authorization denied") {
		t.Fatalf("err = %v, want 'device authorization denied'", err)
	}
}

func TestWaitForApproval_ExpiredToken(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "expired_token"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "device authorization expired") {
		t.Fatalf("err = %v, want 'device authorization expired'", err)
	}
}

func TestWaitForApproval_UnknownError(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "server_error"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "server_error") {
		t.Fatalf("err = %v, want to contain 'server_error'", err)
	}
}

func TestWaitForApproval_EmptyTokenOnSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: ""}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "completed without a token") {
		t.Fatalf("err = %v, want 'completed without a token'", err)
	}
}

func TestWaitForApproval_SlowDown(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "slow_down"}},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-slow"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-slow" {
		t.Fatalf("token = %q, want %q", token, "tok-slow")
	}
}

func TestWaitForApproval_ExpiresInClamped(t *testing.T) {
	t.Parallel()

	// expiresIn=0 should use maxExpiresIn, not panic or return immediately.
	// We verify by checking the function still polls (doesn't error on first call).
	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-clamp"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 0, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-clamp" {
		t.Fatalf("token = %q, want %q", token, "tok-clamp")
	}
}

func TestWaitForApproval_NegativeExpiresInClamped(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-neg"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", -1, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-neg" {
		t.Fatalf("token = %q, want %q", token, "tok-neg")
	}
}

func TestWaitForApproval_TransientErrorRetry(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{err: errors.New("connection refused")},
		{err: errors.New("timeout")},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-retry"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-retry" {
		t.Fatalf("token = %q, want %q", token, "tok-retry")
	}
	if poller.calls != 3 {
		t.Fatalf("calls = %d, want 3", poller.calls)
	}
}

func TestWaitForApproval_TransientErrorExhausted(t *testing.T) {
	t.Parallel()

	var responses []pollResponse
	for range maxTransientErrors + 1 {
		responses = append(responses, pollResponse{err: errors.New("server error")})
	}
	poller := &mockClient{responses: responses}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "consecutive failures") {
		t.Fatalf("err = %v, want 'consecutive failures'", err)
	}
	if poller.calls != maxTransientErrors {
		t.Fatalf("calls = %d, want %d", poller.calls, maxTransientErrors)
	}
}

func TestWaitForApproval_TransientErrorCounterResets(t *testing.T) {
	t.Parallel()

	// 4 transient errors, then a pending response (resets counter), then 4 more, then success.
	var responses []pollResponse
	for range maxTransientErrors - 1 {
		responses = append(responses, pollResponse{err: errors.New("blip")})
	}
	responses = append(responses, pollResponse{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}})
	for range maxTransientErrors - 1 {
		responses = append(responses, pollResponse{err: errors.New("blip")})
	}
	responses = append(responses, pollResponse{result: &auth.DeviceAuthPoll{AccessToken: "tok-reset"}})
	poller := &mockClient{responses: responses}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-reset" {
		t.Fatalf("token = %q, want %q", token, "tok-reset")
	}
}

// TestChooseApprovalURL locks in that the CLI opens the URI with the
// user_code embedded (RFC 8628 §3.3.1) when the AS supplies one, falling
// back to the bare verification_uri otherwise. Most AS verification pages
// prefill the code input from the query param in the complete form; without
// this, the user has to type the code by hand even when the AS provided a
// click-through URL.
func TestChooseApprovalURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		start *auth.DeviceAuthStart
		want  string
	}{
		{
			name: "prefers complete URI when supplied",
			start: &auth.DeviceAuthStart{
				VerificationURI:         "http://test/cli/auth",
				VerificationURIComplete: "http://test/cli/auth?user_code=ABCD-1234",
			},
			want: "http://test/cli/auth?user_code=ABCD-1234",
		},
		{
			name: "falls back to bare verification_uri",
			start: &auth.DeviceAuthStart{
				VerificationURI: "http://test/cli/auth",
			},
			want: "http://test/cli/auth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chooseApprovalURL(tc.start); got != tc.want {
				t.Errorf("chooseApprovalURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitForApproval_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
	}}

	_, _, err := waitForApproval(ctx, poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

// fakeBrowserFlow implements the browserAuthFlow interface for unit tests.
type fakeBrowserFlow struct {
	authURL       string
	waitCode      string
	waitErr       error
	waitUntilDone bool // Wait blocks until ctx is done and returns ctx.Err()
	exchAccess    string
	exchRefresh   string
	exchErr       error

	gotExchangeCode string
	closed          bool
}

func (f *fakeBrowserFlow) AuthorizationURL() string { return f.authURL }

func (f *fakeBrowserFlow) Wait(ctx context.Context) (string, error) {
	if f.waitUntilDone {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.waitCode, f.waitErr
}

func (f *fakeBrowserFlow) Exchange(_ context.Context, code string) (string, string, error) {
	f.gotExchangeCode = code
	return f.exchAccess, f.exchRefresh, f.exchErr
}

func (f *fakeBrowserFlow) Close() error {
	f.closed = true
	return nil
}

func TestShouldUseBrowserLogin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		facts loginFlowFacts
		want  bool
	}{
		{facts: loginFlowFacts{canPrompt: true}, want: true},                    // default interactive → browser
		{facts: loginFlowFacts{}, want: false},                                  // headless → fall back to device
		{facts: loginFlowFacts{canPrompt: true, sshSession: true}, want: false}, // SSH: loopback unreachable → device
		{facts: loginFlowFacts{sshSession: true}, want: false},
		{facts: loginFlowFacts{useDevice: true, canPrompt: true}, want: false}, // --device forces device
		{facts: loginFlowFacts{useDevice: true}, want: false},
		{facts: loginFlowFacts{useDevice: true, canPrompt: true, sshSession: true}, want: false},
	}
	for _, tc := range cases {
		if got := shouldUseBrowserLogin(tc.facts); got != tc.want {
			t.Errorf("shouldUseBrowserLogin(%+v) = %v, want %v", tc.facts, got, tc.want)
		}
	}
}

func TestIsSSHSession(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Setenv(v, "")
	}
	if isSSHSession() {
		t.Error("isSSHSession() = true with all SSH env vars empty")
	}

	t.Setenv("SSH_CONNECTION", "10.0.0.1 50022 10.0.0.2 22")
	if !isSSHSession() {
		t.Error("isSSHSession() = false with SSH_CONNECTION set")
	}
}

// noopOpenURL is a browserOpenFunc for tests that don't care about the
// browser actually opening.
func noopOpenURL(context.Context, string) error { return nil }

// startBrowserStub returns a startBrowser func that records invocations and
// returns the given flow/error.
func startBrowserStub(calls *int, flow browserAuthFlow, err error) func(context.Context) (browserAuthFlow, error) {
	return func(context.Context) (browserAuthFlow, error) {
		*calls++
		return flow, err
	}
}

func TestRunLoginAuto_Interactive_UsesBrowserFlow(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: errors.New("stop")}
	var browserCalls int

	err := runLoginAuto(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, &mockClient{},
		startBrowserStub(&browserCalls, flow, nil), noopOpenURL,
		loginFlowFacts{canPrompt: true})

	if browserCalls != 1 {
		t.Errorf("startBrowser calls = %d, want 1", browserCalls)
	}
	// The stubbed Wait errors, so the browser flow is entered and fails there.
	if err == nil || !strings.Contains(err.Error(), "complete login") {
		t.Fatalf("err = %v, want browser-flow 'complete login' error", err)
	}
}

func TestRunLoginAuto_SSHSession_FallsBackToDevice(t *testing.T) {
	t.Parallel()

	var browserCalls int

	var errW bytes.Buffer
	err := runLoginAuto(context.Background(), &bytes.Buffer{}, &errW, &mockClient{},
		startBrowserStub(&browserCalls, nil, nil), noopOpenURL,
		loginFlowFacts{canPrompt: true, sshSession: true})

	if browserCalls != 0 {
		t.Errorf("startBrowser calls = %d, want 0 (SSH must skip the browser flow)", browserCalls)
	}
	if !strings.Contains(errW.String(), "SSH session detected") {
		t.Errorf("stderr missing SSH explanation:\n%s", errW.String())
	}
	// mockClient.StartDeviceAuth errors — proof the device flow was attempted.
	if err == nil || !strings.Contains(err.Error(), "not implemented in mock") {
		t.Fatalf("err = %v, want device-flow start error from mock", err)
	}
}

func TestRunLoginAuto_Headless_FallsBackToDevice(t *testing.T) {
	t.Parallel()

	var browserCalls int

	var errW bytes.Buffer
	err := runLoginAuto(context.Background(), &bytes.Buffer{}, &errW, &mockClient{},
		startBrowserStub(&browserCalls, nil, nil), noopOpenURL,
		loginFlowFacts{})

	if browserCalls != 0 {
		t.Errorf("startBrowser calls = %d, want 0", browserCalls)
	}
	if !strings.Contains(errW.String(), "No interactive terminal detected") {
		t.Errorf("stderr missing headless explanation:\n%s", errW.String())
	}
	if err == nil || !strings.Contains(err.Error(), "not implemented in mock") {
		t.Fatalf("err = %v, want device-flow start error from mock", err)
	}
}

func TestRunLoginAuto_BrowserStartFails_FallsBackToDevice(t *testing.T) {
	t.Parallel()

	var browserCalls int

	var errW bytes.Buffer
	err := runLoginAuto(context.Background(), &bytes.Buffer{}, &errW, &mockClient{},
		startBrowserStub(&browserCalls, nil, errors.New("listen tcp 127.0.0.1:0: operation not permitted")), noopOpenURL,
		loginFlowFacts{canPrompt: true})

	if browserCalls != 1 {
		t.Errorf("startBrowser calls = %d, want 1", browserCalls)
	}
	if !strings.Contains(errW.String(), "could not start browser sign-in") {
		t.Errorf("stderr missing fallback warning:\n%s", errW.String())
	}
	// mockClient.StartDeviceAuth errors — proof the device flow was attempted.
	if err == nil || !strings.Contains(err.Error(), "not implemented in mock") {
		t.Fatalf("err = %v, want device-flow start error from mock", err)
	}
}

func TestRunLoginAuto_DeviceFlag_NoExplanation(t *testing.T) {
	t.Parallel()

	var browserCalls int

	var errW bytes.Buffer
	err := runLoginAuto(context.Background(), &bytes.Buffer{}, &errW, &mockClient{},
		startBrowserStub(&browserCalls, nil, nil), noopOpenURL,
		loginFlowFacts{useDevice: true, canPrompt: true})

	if browserCalls != 0 {
		t.Errorf("startBrowser calls = %d, want 0", browserCalls)
	}
	// mockClient.StartDeviceAuth errors — proof the device flow was attempted.
	if err == nil || !strings.Contains(err.Error(), "not implemented in mock") {
		t.Fatalf("err = %v, want device-flow start error from mock", err)
	}
	if errW.String() != "" {
		t.Errorf("--device should produce no fallback commentary, got:\n%s", errW.String())
	}
}

func TestRunBrowserLogin_OpensAuthorizationURL(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize?x=1", waitErr: errors.New("stop")}

	var openedURL string
	openURL := func(_ context.Context, u string) error {
		openedURL = u
		return nil
	}

	var out bytes.Buffer
	// The stubbed Wait returns an error, so runBrowserLogin stops before
	// persistLogin (which would hit the real keyring); we assert on the
	// side effects up to that point.
	if err := runBrowserLogin(context.Background(), &out, &bytes.Buffer{}, flow, "https://auth.test", openURL, browserLoginTimeout); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if openedURL != flow.authURL {
		t.Errorf("opened URL = %q, want %q", openedURL, flow.authURL)
	}
	// Happy path shows the auth host, not the full authorize URL, and
	// doesn't print the URL at all (the browser opened fine).
	if !strings.Contains(out.String(), "Logging in to:") {
		t.Errorf("output missing 'Logging in to:' line:\n%s", out.String())
	}
	if strings.Contains(out.String(), flow.authURL) {
		t.Errorf("happy path should not print the full authorize URL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Press Enter to open in browser...") {
		t.Errorf("output missing enter-to-open prompt:\n%s", out.String())
	}
	if !flow.closed {
		t.Error("flow was not closed")
	}
}

func TestRunBrowserLogin_OpenBrowserFallback(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: errors.New("stop")}
	failOpen := func(context.Context, string) error { return errors.New("no browser") }

	var out, errW bytes.Buffer
	if err := runBrowserLogin(context.Background(), &out, &errW, flow, "https://auth.test", failOpen, browserLoginTimeout); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if !strings.Contains(errW.String(), "failed to open browser") {
		t.Errorf("stderr missing warning:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), flow.authURL) {
		t.Errorf("stdout missing fallback URL:\n%s", out.String())
	}
}

func TestRunBrowserLogin_WaitError(t *testing.T) {
	t.Parallel()

	denied := errors.New("access_denied")
	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: denied}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", noopOpenURL, browserLoginTimeout)
	if !errors.Is(err, denied) {
		t.Fatalf("err = %v, want wrapped %v", err, denied)
	}
}

func TestRunBrowserLogin_ExchangeError(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{
		authURL:  "https://auth.test/authorize",
		waitCode: "the-code",
		exchErr:  errors.New("invalid_grant"),
	}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", noopOpenURL, browserLoginTimeout)
	if err == nil || !strings.Contains(err.Error(), "complete login") {
		t.Fatalf("err = %v, want complete login error", err)
	}
	if flow.gotExchangeCode != "the-code" {
		t.Errorf("Exchange got code %q, want the-code", flow.gotExchangeCode)
	}
}

func TestRunBrowserLogin_WaitTimeout(t *testing.T) {
	t.Parallel()

	// The fake blocks until the wait context expires — the deadline must
	// come from runBrowserLogin's own timeout, or this test would hang.
	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitUntilDone: true}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", noopOpenURL, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for sign-in") {
		t.Fatalf("err = %v, want sign-in timeout", err)
	}
	if !strings.Contains(err.Error(), "--device") {
		t.Errorf("timeout error should point at the --device escape hatch, got: %v", err)
	}
	if !flow.closed {
		t.Error("flow was not closed")
	}
}

func TestRunBrowserLogin_ParentCancelNotReportedAsTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // user hit Ctrl-C before the redirect arrived

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitUntilDone: true}

	err := runBrowserLogin(ctx, &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", noopOpenURL, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want wrapped context.Canceled", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancellation must not be reported as a timeout: %v", err)
	}
}
