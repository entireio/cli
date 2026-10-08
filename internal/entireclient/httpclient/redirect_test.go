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
		reject bool
	}{
		{"same HTTPS", []string{"https://host/a"}, "https://host/b", false},
		{"host policy belongs to caller", []string{"https://host/a"}, "https://other/b", false},
		{"initial HTTP", []string{"http://127.0.0.1/a"}, "http://127.0.0.1/b", false},
		{"upgrade", []string{"http://127.0.0.1/a"}, "https://127.0.0.1/b", false},
		{"downgrade", []string{"https://host/a"}, "http://host/b", true},
		{"loopback downgrade", []string{"https://127.0.0.1/a"}, "http://127.0.0.1/b", true},
		{"upgrade then downgrade", []string{"http://host/a", "https://host/b"}, "http://host/c", true},
		{"non HTTP scheme", []string{"https://host/a"}, "ftp://host/b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var via []*http.Request
			for _, raw := range tc.via {
				via = append(via, redirectRequest(t, raw))
			}
			err := CheckSecureRedirect(redirectRequest(t, tc.target), via)
			if tc.reject {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
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
	require.Error(t, err)
	require.Zero(t, called, "mandatory policy runs before a caller's callback")
	err = guarded.CheckRedirect(redirectRequest(t, "https://host/b"), via)
	if err != http.ErrUseLastResponse { //nolint:errorlint // net/http requires sentinel identity, not errors.Is
		t.Fatalf("expected unwrapped ErrUseLastResponse, got %v", err)
	}
	require.Equal(t, 1, called)
	require.ErrorIs(t, original.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	require.Equal(t, 2, called, "original policy was not replaced")
}

func TestWithSecureRedirects_HopCap(t *testing.T) {
	t.Parallel()
	for _, policy := range []func(*http.Request, []*http.Request) error{nil, func(*http.Request, []*http.Request) error { return nil }} {
		c := WithSecureRedirects(&http.Client{CheckRedirect: policy})
		req := redirectRequest(t, "https://host/a")
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = req
		}
		require.ErrorContains(t, c.CheckRedirect(req, via), "10 redirects")
	}
}

func TestWithSecureRedirects_CustomRejection(t *testing.T) {
	t.Parallel()
	denied := errors.New("custom redirect refusal")
	c := WithSecureRedirects(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return denied }})
	require.ErrorIs(t, c.CheckRedirect(redirectRequest(t, "https://host/a"), nil), denied)
}
