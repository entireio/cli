package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/auth-go/tokens"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// useSecretClock installs a settable secretNow for the test.
func useSecretClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now()
	prev := secretNow
	secretNow = func() time.Time { return now }
	t.Cleanup(func() { secretNow = prev })
	return &now
}

// bearerRecorder is a data API that records each request's bearer and
// answers 401 for the bearers in reject.
type bearerRecorder struct {
	mu      sync.Mutex
	bearers []string
	reject  map[string]bool
}

func (b *bearerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	b.bearers = append(b.bearers, token)
	rejected := b.reject[token]
	b.mu.Unlock()
	if rejected {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (b *bearerRecorder) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bearers...)
}

func getOK(t *testing.T, c *api.Client, want int) {
	t.Helper()
	resp, err := c.Get(context.Background(), "/api/v1/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
}

// A data API client built once from a secret keeps working after the
// exchanged token expires: each request asks the cache, which exchanges again
// near expiry. This is what `entire trail watch` needs, since it reconnects
// with one client for as long as it runs.
// Not parallel: sets process-wide environment and the clock seam.
func TestDataAPIClientRenewsSecretToken(t *testing.T) {
	core := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, core)
	now := useSecretClock(t)
	start := *now
	rec := &bearerRecorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(EnvTokenVar, testServiceAccountSecret)
	t.Setenv(api.BaseURLEnvVar, server.URL)

	target, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	if target.Source == nil {
		t.Fatal("a secret must resolve to a renewing token source")
	}
	client := target.Client()

	getOK(t, client, http.StatusOK)
	*now = start.Add(20 * time.Minute) // long past the 10-minute token
	getOK(t, client, http.StatusOK)

	bearers := rec.seen()
	if len(bearers) != 2 || bearers[0] == bearers[1] {
		t.Fatalf("bearers = %v, want two different tokens", bearers)
	}
	if bearers[0] != target.Token {
		t.Errorf("first request sent %q, want the token resolved up front", bearers[0])
	}
	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (one up front, one after expiry)", n)
	}
}

// A 401 drops the cached token, so the next request exchanges again instead
// of reusing a token the server refused until the cache cap runs out.
// Not parallel: sets process-wide environment.
func TestDataAPIClientDropsRefusedSecretToken(t *testing.T) {
	core := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, core)
	now := useSecretClock(t)
	rec := &bearerRecorder{reject: map[string]bool{}}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(EnvTokenVar, testServiceAccountSecret)
	t.Setenv(api.BaseURLEnvVar, server.URL)

	target, err := ResolveDataAPI(context.Background())
	if err != nil {
		t.Fatalf("ResolveDataAPI: %v", err)
	}
	rec.mu.Lock()
	rec.reject[target.Token] = true
	rec.mu.Unlock()
	client := target.Client()

	getOK(t, client, http.StatusUnauthorized)
	*now = now.Add(time.Second) // the next token differs from the refused one
	getOK(t, client, http.StatusOK)

	if n := core.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (the refused token was not reused)", n)
	}
	if bearers := rec.seen(); len(bearers) != 2 || bearers[1] == target.Token {
		t.Errorf("bearers = %v, want a fresh token after the 401", bearers)
	}
}

// A 401 for a token another process already replaced must not discard the
// replacement.
// Not parallel: sets process-wide environment.
func TestSecretSourceKeepsReplacedToken(t *testing.T) {
	core := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, core)
	useSecretClock(t)

	source := EnvTokenSource(testServiceAccountSecret)
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source.Unauthorized("an.older.token")
	if !secretCacheEntryHolds(t, token) {
		t.Fatal("a 401 for another token dropped the cached one")
	}
	source.Unauthorized(token)
	if secretCacheEntryHolds(t, token) {
		t.Fatal("a 401 for the cached token left it cached")
	}
}

func secretCacheEntryHolds(t *testing.T, token string) bool {
	t.Helper()
	subj, err := secretExchangeSubject(testServiceAccountSecret)
	if err != nil {
		t.Fatal(err)
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := osroot.ReadFileNoFollow(root, secretCacheFile(subj))
	if err != nil {
		return false
	}
	var c cachedSecretToken
	return json.Unmarshal(data, &c) == nil && c.Token == token
}

// Not parallel: sets process-wide environment.
func TestEnvTokenSourceOnlyForSecrets(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)

	if EnvTokenSource("a.jwt.token") != nil {
		t.Error("a raw bearer must stay fixed: no token source")
	}
	if EnvTokenSource("  "+testServiceAccountSecret+"\n") == nil {
		t.Error("a secret (with surrounding whitespace) must get a token source")
	}
	if n := core.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want 0: building a source must not exchange", n)
	}
}

// DataBaseURL only needs the core, which a secret names offline.
// Not parallel: sets process-wide environment.
func TestDataBaseURLDoesNotExchangeSecret(t *testing.T) {
	core := &fakeSecretCore{t: t, status: http.StatusBadRequest}
	useFakeSecretCore(t, core)
	t.Setenv(EnvTokenVar, testServiceAccountSecretEU)
	t.Setenv(api.BaseURLEnvVar, "")

	got, err := DataBaseURL()
	if err != nil {
		t.Fatalf("DataBaseURL: %v", err)
	}
	want, err := dataBaseURLForCore("https://eu.auth.entire.io")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("DataBaseURL = %q, want %q", got, want)
	}
	if n := core.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want 0", n)
	}

	if core, err := EnvTokenCoreURL(testServiceAccountSecretEU); err != nil || core != "https://eu.auth.entire.io" {
		t.Errorf("EnvTokenCoreURL = %q, %v", core, err)
	}
	if _, err := EnvTokenCoreURL("ent_svc_us_malformed"); err == nil {
		t.Error("a malformed secret must fail offline")
	}
}

// expiryCheckingAPI answers 200 only for a bearer whose exp is still ahead of
// the secretNow clock, like a server that verifies tokens; otherwise 401. It
// records the bearers it saw.
type expiryCheckingAPI struct {
	t       *testing.T
	mu      sync.Mutex
	bearers []string
	body    string
}

func (e *expiryCheckingAPI) serve(r *http.Request) int {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	e.mu.Lock()
	e.bearers = append(e.bearers, token)
	e.mu.Unlock()
	claims, err := tokens.ParseClaims(token)
	if err != nil || !claims.ExpiresAt.After(secretNow()) {
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

func (e *expiryCheckingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(e.serve(r))
}

// RoundTrip serves the cluster catalog at the secret's core.
func (e *expiryCheckingAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "us.auth.entire.io" || r.URL.Path != clustersAPIPath {
		e.t.Errorf("unexpected catalog request %s", r.URL)
	}
	status := e.serve(r)
	body := e.body
	if status != http.StatusOK {
		body = "{}"
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func (e *expiryCheckingAPI) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.bearers...)
}

// A cell client factory built from a secret and reused past the exchanged
// token's expiry still routes (the catalog listing) and authenticates (the
// cell request): both ask the source for a fresh token.
// Not parallel: sets process-wide environment, seams and the clock.
func TestCellClientFactoryRenewsSecretToken(t *testing.T) {
	core := &fakeSecretCore{t: t, expiresIn: 600}
	useFakeSecretCore(t, core)
	now := useSecretClock(t)
	start := *now
	cell := &expiryCheckingAPI{t: t}
	cellServer := httptest.NewServer(cell)
	t.Cleanup(cellServer.Close)
	catalog := &expiryCheckingAPI{t: t, body: `{"clusters":[{"jurisdiction":"us","isDefault":true,"apiUrl":"` + cellServer.URL + `"}]}`}
	t.Cleanup(SetCellExchangeTransportForTest(t, catalog))
	t.Setenv(EnvTokenVar, testServiceAccountSecret)
	t.Setenv(api.BaseURLEnvVar, "")

	factory, err := NewEntireAPICellClientFactory(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClientFactory: %v", err)
	}
	target := &CellTarget{Jurisdiction: "us"}
	for _, at := range []time.Duration{0, 20 * time.Minute} {
		*now = start.Add(at)
		client, err := factory.ClientFor(context.Background(), target)
		if err != nil {
			t.Fatalf("ClientFor at +%s: %v", at, err)
		}
		getOK(t, client, http.StatusOK)
	}

	for name, bearers := range map[string][]string{"catalog": catalog.seen(), "cell": cell.seen()} {
		if len(bearers) != 2 || bearers[0] == bearers[1] {
			t.Errorf("%s bearers = %v, want a renewed token after expiry", name, bearers)
		}
	}
}

// A raw bearer's cell subject keeps its fixed token.
// Not parallel: sets process-wide environment.
func TestCellSubjectFromRawBearerIsFixed(t *testing.T) {
	core := &fakeSecretCore{t: t}
	useFakeSecretCore(t, core)

	jwt := secretTokenJWT(t, "svc_01JABCDEFGHJKMNPQRSTVWXYZ0", "https://us.auth.entire.io", time.Now().Add(time.Hour))
	subject, err := resolveEnvTokenCellSubject(jwt, false)
	if err != nil {
		t.Fatalf("resolveEnvTokenCellSubject: %v", err)
	}
	if subject.source != nil {
		t.Error("a raw bearer's cell subject must keep its fixed token")
	}
}
