// Package httpclienttest provides redirect regression fixtures for HTTP clients.
package httpclienttest

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// CheckRejectsDowngrade drives the caller's real client construction and request
// path. Only the scheme changes: different httptest listener ports would let a
// host guard hide a missing TLS guard. No real credentials or network are used.
//
// It runs one parallel subtest per redirect status and starting origin. Each
// calls run with the start origin and a fake transport; run must build its
// client with that transport, issue a single credential-bearing request to
// the origin, and return the resulting error. The transport redirects HTTP to
// HTTPS and HTTPS to HTTP, so the error must match ErrInsecureRedirect and the
// plaintext destination after HTTPS must receive no request.
func CheckRejectsDowngrade(t *testing.T, run func(*testing.T, string, http.RoundTripper) error) {
	t.Helper()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, start := range []string{"https://service.example", "https://127.0.0.1", "http://127.0.0.1"} {
			t.Run(fmt.Sprintf("%d/%s", status, start), func(t *testing.T) {
				t.Parallel()
				calls, leaks := 0, 0
				secure := false
				rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 && r.Header.Get("Authorization") == "" {
						t.Error("fixture did not send a credential")
					}
					code := status
					header := make(http.Header)
					if secure && r.URL.Scheme != httpclient.SchemeHTTPS {
						leaks++
						code = http.StatusOK
					} else {
						target := *r.URL
						if r.URL.Scheme == httpclient.SchemeHTTPS {
							secure = true
							target.Scheme = httpclient.SchemeHTTP
						} else {
							target.Scheme = httpclient.SchemeHTTPS
						}
						header.Set("Location", target.String())
					}
					return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
				})
				err := run(t, start, rt)
				if !errors.Is(err, httpclient.ErrInsecureRedirect) {
					t.Errorf("expected ErrInsecureRedirect, got %v", err)
				}
				if leaks != 0 {
					t.Errorf("plaintext destination received %d requests", leaks)
				}
				if !secure {
					t.Error("fixture never reached HTTPS")
				}
			})
		}
	}
}
