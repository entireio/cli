package coreapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// renewingSource hands out tok-1, tok-2, ... and records 401 reports.
type renewingSource struct {
	mu           sync.Mutex
	n            int
	unauthorized []string
}

func (s *renewingSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("tok-%d", s.n), nil
}

func (s *renewingSource) Unauthorized(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unauthorized = append(s.unauthorized, token)
}

// A control-plane client for a service account secret asks its source for
// every request and reports a 401 for the bearer that request carried.
func TestNewWithTokenSource_RenewsAndReports401(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		if token == "tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":401,"title":"Unauthorized"}`)) //nolint:errcheck // test server
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden"}`)) //nolint:errcheck // test server
	}))
	defer server.Close()

	source := &renewingSource{}
	client, err := newWithTokenSource(server.URL, source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.GetMe(context.Background()); err == nil {
			t.Fatal("GetMe succeeded against an error-only server")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen, ","); got != "tok-1,tok-2" {
		t.Errorf("bearers = %s, want one fresh token per request", got)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.unauthorized, ","); got != "tok-1" {
		t.Errorf("401 reports = %q, want only the refused token", got)
	}
}

// testSecret is Core's v1 secret test vector (jurisdiction us), the same one
// the auth package's tests use.
const testSecret = "ent_svc_us_01JABCDEFGHJKMNPQRSTVWXYZ0_01JZYXWVTSRQPNMKJHGFEDCBA9_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq3MriFT"

// shortLivedExchange answers the secret exchange with a distinct token each
// time. expires_in is below the cache margin, so nothing is cached and every
// bearer request exchanges again.
type shortLivedExchange struct{ n atomic.Int32 }

func (s *shortLivedExchange) RoundTrip(r *http.Request) (*http.Response, error) {
	n := s.n.Add(1)
	enc := base64.RawURLEncoding
	claims := fmt.Sprintf(`{"sub":"svc_01JABCDEFGHJKMNPQRSTVWXYZ0","aud":"https://us.auth.entire.io","exp":%d,"jti":"%d"}`, time.Now().Add(time.Hour).Unix(), n)
	jwt := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(claims)) + "." + enc.EncodeToString([]byte("sig"))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":60}`, jwt))),
		Request:    r,
	}, nil
}

// handlerTransport serves every request from h in process, whatever its host.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// New with a service account secret in ENTIRE_TOKEN resolves the bearer per
// request instead of pinning the first exchanged token.
//
// Not parallel: sets ENTIRE_TOKEN and the exchange seam (process-global).
func TestNew_EnvSecretRenewsBearer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Cleanup(auth.UseSecretExchangeTransportForTesting(&shortLivedExchange{}))
	t.Setenv(auth.EnvTokenVar, testSecret)

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.CoreOrigin(); got != "https://us.auth.entire.io" {
		t.Fatalf("CoreOrigin = %q, want the secret's jurisdiction core", got)
	}
	var mu sync.Mutex
	var seen []string
	c.cfg.Client = &http.Client{Transport: handlerTransport{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden"}`)) //nolint:errcheck // test server
	})}}
	for range 2 {
		if _, err := c.GetMe(context.Background()); err == nil {
			t.Fatal("GetMe succeeded against an error-only server")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] == seen[1] || !strings.HasPrefix(seen[0], "Bearer ") {
		t.Errorf("bearers = %v, want a freshly resolved token per request", seen)
	}
}
