package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

const testLoginComplete = "complete"

// mockClient implements deviceAuthClient for unit tests.
type mockClient struct {
	responses []pollResponse
	calls     int

	start    *auth.DeviceAuthStart
	startErr error
	baseURL  string

	gotTokenIssuer    string
	tokenIssuerCalls  int
	tokenIssuerErr    error
	pollDeviceCodeArg string
}

type pollResponse struct {
	result *auth.DeviceAuthPoll
	err    error
}

func (m *mockClient) StartDeviceAuth(_ context.Context) (*auth.DeviceAuthStart, error) {
	if m.start == nil && m.startErr == nil {
		return nil, errors.New("not implemented in mock")
	}
	return m.start, m.startErr
}

func (m *mockClient) BaseURL() string {
	if m.baseURL != "" {
		return m.baseURL
	}
	return "http://test"
}

func (m *mockClient) UseTokenIssuer(origin string) error {
	m.tokenIssuerCalls++
	if m.tokenIssuerErr != nil {
		return m.tokenIssuerErr
	}
	m.gotTokenIssuer = origin
	return nil
}

func (m *mockClient) PollDeviceAuth(_ context.Context, deviceCode string) (*auth.DeviceAuthPoll, error) {
	m.pollDeviceCodeArg = deviceCode
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
	waitRelease   <-chan struct{}
	exchAccess    string
	exchRefresh   string
	exchErr       error
	issuer        string

	gotExchangeCode  string
	gotTokenIssuer   string
	tokenIssuerCalls int
	tokenIssuerErr   error
	closed           bool
}

func (f *fakeBrowserFlow) AuthorizationURL() string { return f.authURL }

func (f *fakeBrowserFlow) Issuer() string { return f.issuer }

func (f *fakeBrowserFlow) UseTokenIssuer(origin string) error {
	f.tokenIssuerCalls++
	if f.tokenIssuerErr != nil {
		return f.tokenIssuerErr
	}
	f.gotTokenIssuer = origin
	return nil
}

func (f *fakeBrowserFlow) Wait(ctx context.Context) (string, error) {
	if f.waitUntilDone {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.waitRelease != nil {
		select {
		case <-f.waitRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
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

func noopCopyURL(string) error { return nil }

func newTestLoginURLInteractor(actions ...loginURLAction) loginURLInteractor {
	next := 0
	return loginURLInteractor{
		keysAvailable: func() bool { return true },
		readAction: func(ctx context.Context) (loginURLAction, error) {
			if next >= len(actions) {
				<-ctx.Done()
				return loginURLNone, ctx.Err()
			}
			action := actions[next]
			next++
			return action, nil
		},
		copyURL: noopCopyURL,
		openURL: noopOpenURL,
	}
}

// The prompt advertises keys the process can honour. When the terminal cannot
// deliver them the line must be suppressed, while the waiting message — and
// sign-in through the visible URL — carry on unchanged.
func TestWaitForLoginURLResult_NoKeysSuppressesPrompt(t *testing.T) {
	t.Parallel()

	interactor := newTestLoginURLInteractor()
	interactor.keysAvailable = func() bool { return false }
	interactor.readAction = func(context.Context) (loginURLAction, error) {
		return loginURLNone, nil
	}

	var out bytes.Buffer
	got, err := waitForLoginURLResult(
		context.Background(), &out, &bytes.Buffer{},
		"https://auth.test/authorize", "Waiting... ", interactor,
		func(context.Context) (string, error) { return testLoginComplete, nil },
	)
	if err != nil {
		t.Fatalf("waitForLoginURLResult() error = %v", err)
	}
	if got != testLoginComplete {
		t.Errorf("result = %q, want complete", got)
	}
	if strings.Contains(out.String(), loginURLPrompt) {
		t.Errorf("prompt advertised keys that cannot be read:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Waiting... ") {
		t.Errorf("output missing waiting message:\n%s", out.String())
	}
}

func TestCopyLoginURL(t *testing.T) {
	t.Parallel()

	// Released at cleanup so the wedged-helper case leaks no goroutine.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	tests := []struct {
		name    string
		copyURL clipboardWriteFunc
		timeout time.Duration
		wantErr string
	}{
		{
			name:    "success",
			copyURL: noopCopyURL,
			timeout: time.Minute,
		},
		{
			name:    "propagates failure",
			copyURL: func(string) error { return errors.New("clipboard unavailable") },
			timeout: time.Minute,
			wantErr: "clipboard unavailable",
		},
		{
			// A wedged xclip/xsel must not block the caller's select loop, and
			// with it a sign-in that has already completed.
			name:    "times out on a wedged helper",
			copyURL: func(string) error { <-blocked; return nil },
			timeout: 10 * time.Millisecond,
			wantErr: "clipboard write timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := copyLoginURL(tt.copyURL, "https://auth.test", tt.timeout)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("copyLoginURL() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("copyLoginURL() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForLoginURLResult_CompletesWithoutActionAndJoinsReader(t *testing.T) {
	t.Parallel()

	readerStopped := make(chan struct{})
	interactor := newTestLoginURLInteractor()
	interactor.readAction = func(ctx context.Context) (loginURLAction, error) {
		<-ctx.Done()
		close(readerStopped)
		return loginURLNone, ctx.Err()
	}

	got, err := waitForLoginURLResult(
		context.Background(), &bytes.Buffer{}, &bytes.Buffer{},
		"https://auth.test/authorize", "Waiting... ", interactor,
		func(context.Context) (string, error) { return testLoginComplete, nil },
	)
	if err != nil {
		t.Fatalf("waitForLoginURLResult() error = %v", err)
	}
	if got != testLoginComplete {
		t.Errorf("result = %q, want complete", got)
	}
	select {
	case <-readerStopped:
	default:
		t.Fatal("input reader was not joined before returning")
	}
}

func TestWaitForLoginURLResult_EnterOpensThenWaits(t *testing.T) {
	t.Parallel()

	const loginURL = "https://auth.test/authorize?state=open-me"
	completed := make(chan struct{})
	interactor := newTestLoginURLInteractor(loginURLOpen)
	var openedURL string
	interactor.openURL = func(_ context.Context, value string) error {
		openedURL = value
		close(completed)
		return nil
	}

	var out bytes.Buffer
	got, err := waitForLoginURLResult(
		context.Background(), &out, &bytes.Buffer{}, loginURL, "Waiting... ", interactor,
		func(ctx context.Context) (string, error) {
			select {
			case <-completed:
				return testLoginComplete, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	)
	if err != nil {
		t.Fatalf("waitForLoginURLResult() error = %v", err)
	}
	if openedURL != loginURL {
		t.Errorf("opened URL = %q, want %q", openedURL, loginURL)
	}
	if got != testLoginComplete {
		t.Errorf("result = %q, want complete", got)
	}
	if !strings.Contains(out.String(), "✓ Opened browser.") {
		t.Errorf("output missing browser confirmation:\n%s", out.String())
	}
}

func TestWaitForLoginURLResult_CopyKeepsWaiting(t *testing.T) {
	t.Parallel()

	const loginURL = "https://auth.test/authorize?state=copy-me"
	completed := make(chan struct{})
	interactor := newTestLoginURLInteractor(loginURLCopy)
	var copiedURL string
	interactor.copyURL = func(value string) error {
		copiedURL = value
		close(completed)
		return nil
	}

	var out, errW bytes.Buffer
	_, err := waitForLoginURLResult(
		context.Background(), &out, &errW, loginURL, "Waiting... ", interactor,
		func(ctx context.Context) (struct{}, error) {
			select {
			case <-completed:
				return struct{}{}, nil
			case <-ctx.Done():
				return struct{}{}, ctx.Err()
			}
		},
	)
	if err != nil {
		t.Fatalf("waitForLoginURLResult() error = %v", err)
	}
	if copiedURL != loginURL {
		t.Errorf("copied URL = %q, want %q", copiedURL, loginURL)
	}
	if !strings.Contains(out.String(), "✓ Copied to clipboard.") {
		t.Errorf("output missing copy confirmation:\n%s", out.String())
	}
	if got := strings.Count(out.String(), loginURLPrompt); got != 1 {
		t.Errorf("prompt count = %d, want 1 after copy:\n%s", got, out.String())
	}
	if got := strings.Count(out.String(), "Waiting... "); got != 1 {
		t.Errorf("waiting message count = %d, want 1 after copy:\n%s", got, out.String())
	}
}

func TestWaitForLoginURLResult_FailuresKeepActionsAvailable(t *testing.T) {
	t.Parallel()

	completed := make(chan struct{})
	interactor := newTestLoginURLInteractor(loginURLCopy, loginURLOpen, loginURLOpen)
	interactor.copyURL = func(string) error { return errors.New("clipboard unavailable") }
	openCalls := 0
	interactor.openURL = func(context.Context, string) error {
		openCalls++
		if openCalls == 1 {
			return errors.New("browser unavailable")
		}
		close(completed)
		return nil
	}

	var out, errW bytes.Buffer
	_, err := waitForLoginURLResult(
		context.Background(), &out, &errW, "https://auth.test", "Waiting... ", interactor,
		func(ctx context.Context) (struct{}, error) {
			select {
			case <-completed:
				return struct{}{}, nil
			case <-ctx.Done():
				return struct{}{}, ctx.Err()
			}
		},
	)
	if err != nil {
		t.Fatalf("waitForLoginURLResult() error = %v", err)
	}
	for _, want := range []string{"failed to copy login URL", "failed to open default browser"} {
		if !strings.Contains(errW.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errW.String())
		}
	}
	if got := strings.Count(out.String(), loginURLPrompt); got != 1 {
		t.Errorf("prompt count = %d, want 1 after two failures:\n%s", got, out.String())
	}
	if got := strings.Count(out.String(), "Waiting... "); got != 1 {
		t.Errorf("waiting message count = %d, want 1 after actions:\n%s", got, out.String())
	}
}

func TestWaitForLoginURLResult_Cancellation(t *testing.T) {
	t.Parallel()

	interactor := newTestLoginURLInteractor()
	interactor.readAction = func(context.Context) (loginURLAction, error) {
		return loginURLNone, context.Canceled
	}

	_, err := waitForLoginURLResult(
		context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, "https://auth.test", "Waiting... ", interactor,
		func(ctx context.Context) (struct{}, error) {
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestWaitForLoginURLResult_UnexpectedActionFails(t *testing.T) {
	t.Parallel()

	interactor := newTestLoginURLInteractor(loginURLAction(255))
	_, err := waitForLoginURLResult(
		context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, "https://auth.test", "Waiting... ", interactor,
		func(ctx context.Context) (struct{}, error) {
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		},
	)
	if err == nil || !strings.Contains(err.Error(), "unexpected login URL action: 255") {
		t.Fatalf("error = %v, want unexpected-action error", err)
	}
}

func TestLoginURLActionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		msg          tea.Msg
		wantSelected bool
		wantAction   loginURLAction
		wantErr      error
	}{
		{name: "enter", msg: tea.KeyPressMsg{Code: tea.KeyEnter}, wantSelected: true, wantAction: loginURLOpen},
		{name: "control j", msg: tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, wantSelected: true, wantAction: loginURLOpen},
		{name: "lowercase copy", msg: tea.KeyPressMsg{Code: 'c', Text: "c"}, wantSelected: true, wantAction: loginURLCopy},
		{name: "uppercase copy", msg: tea.KeyPressMsg{Code: 'C', Text: "C"}, wantSelected: true, wantAction: loginURLCopy},
		{name: "lowercase o ignored", msg: tea.KeyPressMsg{Code: 'o', Text: "o"}},
		{name: "uppercase o ignored", msg: tea.KeyPressMsg{Code: 'O', Text: "O"}},
		{name: "control c", msg: tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, wantSelected: true, wantErr: context.Canceled},
		{name: "right arrow ignored", msg: tea.KeyPressMsg{Code: tea.KeyRight}},
		{name: "F1 ignored", msg: tea.KeyPressMsg{Code: tea.KeyF1}},
		{name: "alt copy ignored", msg: tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt}},
		{name: "non-key ignored", msg: tea.WindowSizeMsg{Width: 80, Height: 24}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			updated, cmd := (loginURLActionModel{}).Update(tt.msg)
			result, ok := updated.(loginURLActionModel)
			if !ok {
				t.Fatalf("updated model type = %T, want loginURLActionModel", updated)
			}
			if result.selected != tt.wantSelected {
				t.Errorf("selected = %v, want %v", result.selected, tt.wantSelected)
			}
			if result.action != tt.wantAction {
				t.Errorf("action = %v, want %v", result.action, tt.wantAction)
			}
			if !errors.Is(result.err, tt.wantErr) {
				t.Errorf("error = %v, want %v", result.err, tt.wantErr)
			}
			if gotQuit := cmd != nil; gotQuit != tt.wantSelected {
				t.Errorf("quit command present = %v, want %v", gotQuit, tt.wantSelected)
			}
		})
	}
}

func TestRunLogin_InteractiveDeviceFlowAutoStartsPolling(t *testing.T) {
	t.Parallel()

	const approvalURL = "https://auth.test/device?code=ABCD-EFGH"
	client := &mockClient{
		start: &auth.DeviceAuthStart{
			DeviceCode:              "device-123",
			UserCode:                "ABCD-EFGH",
			VerificationURIComplete: approvalURL,
			ExpiresIn:               60,
		},
		responses: []pollResponse{{result: &auth.DeviceAuthPoll{Error: "access_denied"}}},
	}
	var out bytes.Buffer
	err := runLogin(context.Background(), &out, &bytes.Buffer{}, client, newTestLoginURLInteractor(), true)
	if err == nil || !strings.Contains(err.Error(), "device authorization denied") {
		t.Fatalf("error = %v, want device authorization denied", err)
	}
	for _, want := range []string{"Device code: ABCD-EFGH", deviceLoginURLLabel + "\n  " + approvalURL, loginURLPrompt} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRenderLoginURL(t *testing.T) {
	t.Parallel()

	const loginURL = "https://auth.test/authorize?state=visible"
	if got := renderLoginURL(loginURL, false); got != loginURL {
		t.Errorf("plain URL = %q, want %q", got, loginURL)
	}

	linked := renderLoginURL(loginURL, true)
	if !strings.Contains(linked, "\x1b]8;") || !strings.Contains(linked, loginURL) {
		t.Errorf("linked URL missing OSC 8 target or literal text: %q", linked)
	}
	if got := renderLoginURL("file:///tmp/not-http", true); got != "file:///tmp/not-http" {
		t.Errorf("non-HTTP URL should remain plain, got %q", got)
	}
}

func TestDeviceLoginURLLabel_MatchesBrowserFlow(t *testing.T) {
	t.Parallel()

	if deviceLoginURLLabel != browserLoginURLLabel {
		t.Errorf("device login URL label = %q, want %q", deviceLoginURLLabel, browserLoginURLLabel)
	}
}

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
		startBrowserStub(&browserCalls, flow, nil), newTestLoginURLInteractor(),
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
		startBrowserStub(&browserCalls, nil, nil), newTestLoginURLInteractor(),
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
		startBrowserStub(&browserCalls, nil, nil), newTestLoginURLInteractor(),
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
		startBrowserStub(&browserCalls, nil, errors.New("listen tcp 127.0.0.1:0: operation not permitted")), newTestLoginURLInteractor(),
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
		startBrowserStub(&browserCalls, nil, nil), newTestLoginURLInteractor(),
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

func TestRunBrowserLogin_AutoWaitsWithoutOpeningBrowser(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize?x=1", waitErr: errors.New("stop")}

	var opened bool
	openURL := func(_ context.Context, _ string) error {
		opened = true
		return nil
	}
	interactor := newTestLoginURLInteractor()
	interactor.openURL = openURL

	var out bytes.Buffer
	// The stubbed Wait returns an error, so runBrowserLogin stops before
	// persistLogin (which would hit the real keyring); we assert on the
	// side effects up to that point.
	if err := runBrowserLogin(context.Background(), &out, &bytes.Buffer{}, flow, "https://auth.test", interactor, browserLoginTimeout); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if opened {
		t.Error("auto-started wait must not open the browser")
	}
	if !strings.Contains(out.String(), "Logging in to https://auth.test") {
		t.Errorf("output missing login destination line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Login URL:\n  "+flow.authURL) {
		t.Errorf("output missing full authorization URL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), loginURLPrompt) {
		t.Errorf("output missing URL action prompt:\n%s", out.String())
	}
	if !flow.closed {
		t.Error("flow was not closed")
	}
}

func TestRunBrowserLogin_OpenActionOpensAuthorizationURL(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	flow := &fakeBrowserFlow{
		authURL:     "https://auth.test/authorize?x=1",
		waitErr:     errors.New("stop"),
		waitRelease: release,
	}

	var openedURL string
	interactor := newTestLoginURLInteractor(loginURLOpen)
	interactor.openURL = func(_ context.Context, u string) error {
		openedURL = u
		close(release)
		return nil
	}

	if err := runBrowserLogin(
		context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow,
		"https://auth.test", interactor, browserLoginTimeout,
	); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if openedURL != flow.authURL {
		t.Errorf("opened URL = %q, want %q", openedURL, flow.authURL)
	}
}

func TestRunBrowserLogin_OpenBrowserFallback(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	flow := &fakeBrowserFlow{
		authURL:     "https://auth.test/authorize",
		waitErr:     errors.New("stop"),
		waitRelease: release,
	}
	interactor := newTestLoginURLInteractor(loginURLOpen, loginURLOpen)
	openCalls := 0
	interactor.openURL = func(context.Context, string) error {
		openCalls++
		if openCalls == 1 {
			return errors.New("no browser")
		}
		close(release)
		return nil
	}

	var out, errW bytes.Buffer
	if err := runBrowserLogin(context.Background(), &out, &errW, flow, "https://auth.test", interactor, browserLoginTimeout); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if !strings.Contains(errW.String(), "failed to open default browser") {
		t.Errorf("stderr missing warning:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), flow.authURL) {
		t.Errorf("stdout missing authorization URL:\n%s", out.String())
	}
	if got := strings.Count(out.String(), loginURLPrompt); got != 1 {
		t.Errorf("prompt count = %d, want 1 after failed open:\n%s", got, out.String())
	}
}

func TestRunBrowserLogin_WaitError(t *testing.T) {
	t.Parallel()

	denied := errors.New("access_denied")
	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: denied}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", newTestLoginURLInteractor(), browserLoginTimeout)
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

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", newTestLoginURLInteractor(), browserLoginTimeout)
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

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", newTestLoginURLInteractor(), 50*time.Millisecond)
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

	err := runBrowserLogin(ctx, &bytes.Buffer{}, &bytes.Buffer{}, flow, "https://auth.test", newTestLoginURLInteractor(), time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want wrapped context.Canceled", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancellation must not be reported as a timeout: %v", err)
	}
}
