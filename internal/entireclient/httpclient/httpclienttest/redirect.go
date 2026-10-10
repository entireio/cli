// Package httpclienttest provides redirect regression fixtures for HTTP clients.
package httpclienttest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const schemeHTTPS = "https"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// CheckRejectsDowngrade drives the caller's real client construction and request
// path. Only the scheme changes: different httptest listener ports would let a
// host guard hide a missing TLS guard. No real credentials or network are used.
func CheckRejectsDowngrade(t *testing.T, run func(*testing.T, string, http.RoundTripper) error) {
	t.Helper()
	for _, status := range []int{301, 302, 303, 307, 308} {
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
					if secure && r.URL.Scheme != schemeHTTPS {
						leaks++
						code = http.StatusOK
					} else {
						target := *r.URL
						if r.URL.Scheme == schemeHTTPS {
							secure = true
							target.Scheme = "http"
						} else {
							target.Scheme = schemeHTTPS
						}
						header.Set("Location", target.String())
					}
					return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
				})
				err := run(t, start, rt)
				if err == nil {
					t.Error("expected redirect rejection")
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
