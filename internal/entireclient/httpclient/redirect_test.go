package httpclient

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckSecureRedirect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		via    []string
		target string
		want   error
	}{
		{"same HTTPS", []string{"https://host/a"}, "https://host/b", nil},
		{"host policy belongs to caller", []string{"https://host/a"}, "https://other/b", nil},
		{"initial HTTP", []string{"http://127.0.0.1/a"}, "http://127.0.0.1/b", nil},
		{"upgrade", []string{"http://127.0.0.1/a"}, "https://127.0.0.1/b", nil},
		{"downgrade", []string{"https://host/a"}, "http://host/b", ErrInsecureRedirect},
		{"loopback downgrade", []string{"https://127.0.0.1/a"}, "http://127.0.0.1/b", ErrInsecureRedirect},
		{"upgrade then downgrade", []string{"http://host/a", "https://host/b"}, "http://host/c", ErrInsecureRedirect},
		{"non HTTP scheme", []string{"https://host/a"}, "ftp://host/b", ErrInsecureRedirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var via []*http.Request
			for _, raw := range tc.via {
				via = append(via, redirectRequest(t, raw))
			}
			err := CheckSecureRedirect(redirectRequest(t, tc.target), via)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, ErrRequestPolicy)
		})
	}
}

func TestRedirectErrors(t *testing.T) {
	t.Parallel()
	// A loop is an availability failure: callers must treat it as a node
	// failure, not a security rejection.
	require.NotErrorIs(t, ErrTooManyRedirects, ErrRequestPolicy)
	require.ErrorIs(t, ErrInsecureRedirect, ErrRedirectPolicy)
	require.Equal(t, "refusing redirect from HTTPS to an insecure scheme", ErrInsecureRedirect.Error(), "a chained sentinel must not repeat its parents' text")
}

func TestWithSecureRedirects_SharedPolicyWinsOverCallbackError(t *testing.T) {
	t.Parallel()
	denied := errors.New("custom redirect refusal")
	c := WithSecureRedirects(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return denied }})
	via := []*http.Request{redirectRequest(t, "https://host/a")}
	require.ErrorIs(t, c.CheckRedirect(redirectRequest(t, "http://host/b"), via), ErrInsecureRedirect)
}

func TestCheckSecureRedirect_HopCap(t *testing.T) {
	t.Parallel()
	req := redirectRequest(t, "https://host/a")
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = req
	}
	require.ErrorIs(t, CheckSecureRedirect(req, via), ErrTooManyRedirects)
	require.NoError(t, CheckSecureRedirect(req, via[1:]))
}

func redirectRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
	require.NoError(t, err)
	return req
}

func TestWithSecureRedirects_PreservesClientAndPolicy(t *testing.T) {
	t.Parallel()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	called := 0
	original := &http.Client{Timeout: time.Second, Jar: jar, Transport: http.DefaultTransport, CheckRedirect: func(*http.Request, []*http.Request) error {
		called++
		return http.ErrUseLastResponse
	}}
	guarded := WithSecureRedirects(original)
	require.NotSame(t, original, guarded)
	require.Equal(t, original.Timeout, guarded.Timeout)
	require.Same(t, original.Jar, guarded.Jar)
	require.Same(t, original.Transport, guarded.Transport)
	via := []*http.Request{redirectRequest(t, "https://host/a")}
	err = guarded.CheckRedirect(redirectRequest(t, "http://host/b"), via)
	if err != http.ErrUseLastResponse { //nolint:errorlint // net/http requires sentinel identity
		t.Fatalf("stopping an insecure redirect must preserve ErrUseLastResponse, got %v", err)
	}
	require.Equal(t, 1, called)
	err = guarded.CheckRedirect(redirectRequest(t, "https://host/b"), via)
	if err != http.ErrUseLastResponse { //nolint:errorlint // net/http requires sentinel identity, not errors.Is
		t.Fatalf("expected unwrapped ErrUseLastResponse, got %v", err)
	}
	require.Equal(t, 2, called)
	require.ErrorIs(t, original.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	require.Equal(t, 3, called, "original policy was not replaced")
}

func TestWithSecureRedirects_HopCap(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]func(*http.Request, []*http.Request) error{
		"no callback":         nil,
		"permissive callback": func(*http.Request, []*http.Request) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := WithSecureRedirects(&http.Client{CheckRedirect: policy})
			req := redirectRequest(t, "https://host/a")
			via := make([]*http.Request, maxRedirects)
			for i := range via {
				via[i] = req
			}
			require.ErrorIs(t, c.CheckRedirect(req, via), ErrTooManyRedirects)
		})
	}
}

type redirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (f redirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestWithSecureRedirects_RechecksCallbackURL(t *testing.T) {
	t.Parallel()
	denied := errors.New("custom redirect refusal")
	for _, tc := range []struct {
		name        string
		callbackErr error
		startHTTP   bool
	}{
		{"callback allows modified URL", nil, false},
		{"callback stops redirect", http.ErrUseLastResponse, false},
		{"callback rejects redirect", denied, false},
		{"callback undoes first HTTPS upgrade", nil, true},
		{"callback stops first HTTPS upgrade", http.ErrUseLastResponse, true},
		{"callback rejects first HTTPS upgrade", denied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls, callbacks := 0, 0
			c := WithSecureRedirects(&http.Client{
				Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					code := http.StatusTemporaryRedirect
					if calls > 1 {
						code = http.StatusOK
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Location": {"https://service.example/redirect"}}, Body: http.NoBody, Request: req}, nil
				}),
				CheckRedirect: func(req *http.Request, _ []*http.Request) error {
					callbacks++
					req.URL.Scheme = "http"
					return tc.callbackErr
				},
			})
			req := redirectRequest(t, "https://service.example/start")
			if tc.startHTTP {
				req.URL.Scheme = SchemeHTTP
			}
			req.Header.Set("Authorization", "Bearer test-token")
			resp, err := c.Do(req)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			switch {
			case tc.callbackErr == nil:
				require.ErrorIs(t, err, ErrInsecureRedirect)
			case errors.Is(tc.callbackErr, http.ErrUseLastResponse):
				require.NoError(t, err, "ErrUseLastResponse must retain its identity")
				require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
			default:
				require.ErrorIs(t, err, tc.callbackErr)
			}
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, calls, "callback-rewritten HTTP destination must receive no request")
		})
	}
}

func TestWithSecureRedirects_CustomRejection(t *testing.T) {
	t.Parallel()
	denied := errors.New("custom redirect refusal")
	c := WithSecureRedirects(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return denied }})
	require.ErrorIs(t, c.CheckRedirect(redirectRequest(t, "https://host/a"), nil), denied)
}
