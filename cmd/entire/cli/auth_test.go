package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/auth-go/sts"
	"github.com/entireio/auth-go/tokenmanager"
	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/auth-go/tokenstore"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

const (
	testBaseURL = "https://entire.io"
	testAuthTok = "tok"
)

// --- status -----------------------------------------------------------------

// okProfile is a profileFetcher returning a fully-populated profile, for the
// happy-path status tests.
func okProfile(context.Context) (*authProfile, error) {
	return &authProfile{
		Handle:         "alice",
		DisplayName:    "Alice Smith",
		Email:          "alice@example.com",
		Provider:       "github",
		ProviderUserID: "alice",
	}, nil
}

// noContexts is a contextsProvider with nothing stored.
func noContexts() ([]*contexts.Context, string, error) { return nil, "", nil }

// unusedProfile is a profileFetcher that fails the test if called — for the
// not-logged-in path, where the local token check short-circuits before /me.
func unusedProfile(t *testing.T) profileFetcher {
	return func(context.Context) (*authProfile, error) {
		t.Helper()
		t.Fatal("/me should not be called when no token is stored")
		return nil, errors.New("unreachable")
	}
}

func TestRunAuthStatus_NotLoggedIn(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, store, unusedProfile(t), noContexts, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Not logged in to "+testBaseURL) {
		t.Fatalf("output = %q, want 'Not logged in' message", out.String())
	}
}

func TestRunAuthStatus_LoggedIn(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	ctxs := func() ([]*contexts.Context, string, error) {
		return []*contexts.Context{{Name: "us.auth.entire.io"}}, "us.auth.entire.io", nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, store, okProfile, ctxs, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Logged in to "+testBaseURL) {
		t.Fatalf("output = %q, want 'Logged in' message", got)
	}
	if !strings.Contains(got, "Alice Smith") || !strings.Contains(got, "@alice") || !strings.Contains(got, "<alice@example.com>") {
		t.Fatalf("output = %q, want profile header (name/@handle/<email>)", got)
	}
	if !strings.Contains(got, "github/alice") {
		t.Fatalf("output = %q, want provider identity", got)
	}
	if !strings.Contains(got, "us.auth.entire.io") {
		t.Fatalf("output = %q, want active-context line", got)
	}
	// Status must no longer list server-side sessions.
	if strings.Contains(got, "Active sessions") {
		t.Fatalf("output = %q, should not list server-side sessions", got)
	}
}

func TestRunAuthStatus_MultipleContextsHint(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	ctxs := func() ([]*contexts.Context, string, error) {
		return []*contexts.Context{{Name: "a"}, {Name: "b"}, {Name: "c"}}, "a", nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, store, okProfile, ctxs, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "3 login contexts saved") {
		t.Fatalf("output = %q, want multi-context hint", out.String())
	}
}

func TestRunAuthStatus_TokenInvalid(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	// A 401 from GET /me arrives as *coreapi.ErrorModelStatusCode, not
	// api.HTTPError — exercise isKeychainTokenRejected's core-API branch.
	fetchProfile := func(context.Context) (*authProfile, error) {
		return nil, &coreapi.ErrorModelStatusCode{StatusCode: http.StatusUnauthorized}
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, store, fetchProfile, noContexts, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "no longer valid") {
		t.Fatalf("output = %q, want invalid-token message", out.String())
	}
	if !strings.Contains(out.String(), "entire login") {
		t.Fatalf("output = %q, want re-auth hint", out.String())
	}
}

// TestRunAuthStatus_STSRejectionRendersInvalidMessage: in split-host setups,
// STS rejection happens before /me resolves a bearer, surfacing as auth-go's
// wrapped string (no typed sentinel). isKeychainTokenRejected must still map
// it to the re-login hint.
func TestRunAuthStatus_STSRejectionRendersInvalidMessage(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	fetchProfile := func(context.Context) (*authProfile, error) {
		return nil, errors.New("token exchange: status 400: invalid_grant: subject_token expired")
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, store, fetchProfile, noContexts, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "no longer valid") {
		t.Fatalf("output = %q, want invalid-token message", out.String())
	}
}

// TestRunAuthStatus_ExpiredCoreTokenRendersInvalidMessage: the tokenmanager's
// preflight returns auth.ErrNotLoggedIn for an expired core JWT. The keyring
// read still finds an entry, so the helper must route the wrapped sentinel to
// the re-login hint (and a string-only lookalike must NOT match).
func TestRunAuthStatus_ExpiredCoreTokenRendersInvalidMessage(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	stringOnly := func(context.Context) (*authProfile, error) {
		return nil, errors.New("fetch profile: " + auth.ErrNotLoggedIn.Error())
	}
	wrapped := func(context.Context) (*authProfile, error) {
		return nil, &wrappedTestError{msg: "fetch profile", inner: auth.ErrNotLoggedIn}
	}

	// Sanity: string-only does NOT match (no sentinel chain).
	var out1 bytes.Buffer
	if err := runAuthStatus(context.Background(), &out1, store, stringOnly, noContexts, testBaseURL); err == nil {
		t.Fatal("string-only ErrNotLoggedIn should not match — keep the test honest")
	}

	// Real path: errors.Is sees the sentinel through the %w chain.
	var out2 bytes.Buffer
	if err := runAuthStatus(context.Background(), &out2, store, wrapped, noContexts, testBaseURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out2.String(), "no longer valid") {
		t.Fatalf("output = %q, want invalid-token message", out2.String())
	}
}

// wrappedTestError is a tiny stand-in for fmt.Errorf("...: %w", inner).
type wrappedTestError struct {
	msg   string
	inner error
}

func (e *wrappedTestError) Error() string { return e.msg + ": " + e.inner.Error() }
func (e *wrappedTestError) Unwrap() error { return e.inner }

func TestRunAuthStatus_ServerError(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens[testBaseURL] = testAuthTok

	fetchProfile := func(context.Context) (*authProfile, error) {
		return nil, errors.New("connection refused")
	}

	var out bytes.Buffer
	err := runAuthStatus(context.Background(), &out, store, fetchProfile, noContexts, testBaseURL)
	if err == nil {
		t.Fatal("expected error for non-401 failure")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want underlying message", err)
	}
}

// --- registration -----------------------------------------------------------

func TestAuthCmd_RegistersExpectedSubcommands(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var authCmd *struct{}
	for _, c := range root.Commands() {
		if c.Use == "auth" {
			authCmd = &struct{}{}
			subcommands := map[string]bool{}
			for _, sub := range c.Commands() {
				name := strings.Fields(sub.Use)[0]
				subcommands[name] = true
			}
			for _, want := range []string{"login", "logout", "status", "contexts", "use"} {
				if !subcommands[want] {
					t.Errorf("auth missing subcommand %q (got: %v)", want, subcommands)
				}
			}
		}
	}
	if authCmd == nil {
		t.Fatal("auth command not registered on root")
	}
}

// --- resolveDataAPIToken ----------------------------------------------------
//
// These tests exercise the production path: they install a real
// tokenmanager.Manager via auth.SetManagerForTest and stub only the
// STS wire call via SetExchangeForTest. That covers the audience-
// matching logic the function-injection tests above can't reach
// (defaultRevokeCurrentSession / defaultRevokeAllSessions call
// resolveAuthHostToken directly, but unit tests for the surrounding flows
// inject fakes that bypass it).

// authResolveTestIssuer is intentionally distinct from api.AuthBaseURL() so
// the manager's same-host shortcut is skipped and the STS-exchange path runs.
const authResolveTestIssuer = "https://auth.resolve-test.example.com"

func TestResolveAuthHostToken_ScopesExchangeToAuthHostOrigin(t *testing.T) {
	// No t.Parallel: SetManagerForTest mutates package-level state in the
	// auth package. Concurrent tests in this package don't reach the real
	// auth.TokenForResource path (they inject lister/revoker fakes), so
	// serial execution here is purely defensive.

	store := newAuthMemStore()
	saveCoreToken(t, store, authResolveTestIssuer, "opaque-core-token")

	var capturedResource string
	mgr := newResolveTestManager(t, store, func(_ context.Context, req sts.ExchangeRequest) (*tokens.TokenSet, error) {
		capturedResource = req.Resource
		return &tokens.TokenSet{AccessToken: "exchanged-auth-host-tok"}, nil
	})
	t.Cleanup(auth.SetManagerForTest(t, mgr))

	got, err := resolveAuthHostToken(t.Context())
	if err != nil {
		t.Fatalf("resolveAuthHostToken: %v", err)
	}

	if got != "exchanged-auth-host-tok" {
		t.Errorf("token = %q, want %q", got, "exchanged-auth-host-tok")
	}
	// The whole point of the helper: when an exchange happens, the resource
	// handed to STS must be the auth host's origin (where the session
	// endpoints live), not the raw env-var value.
	if want := api.OriginOnly(api.AuthBaseURL()); capturedResource != want {
		t.Errorf("STS exchange Resource = %q, want %q (api.OriginOnly(api.AuthBaseURL()))",
			capturedResource, want)
	}
}

func TestResolveAuthHostToken_WrapsManagerError(t *testing.T) {
	store := newAuthMemStore()
	saveCoreToken(t, store, authResolveTestIssuer, "opaque-core-token")

	mgr := newResolveTestManager(t, store, func(context.Context, sts.ExchangeRequest) (*tokens.TokenSet, error) {
		return nil, errors.New("simulated transport failure")
	})
	t.Cleanup(auth.SetManagerForTest(t, mgr))

	_, err := resolveAuthHostToken(t.Context())
	if err == nil {
		t.Fatal("expected error when exchange fails")
	}
	if !strings.Contains(err.Error(), "resolve auth-host token") {
		t.Errorf("error = %v, want 'resolve auth-host token' wrap prefix", err)
	}
	if !strings.Contains(err.Error(), "simulated transport failure") {
		t.Errorf("error = %v, want underlying message preserved", err)
	}
}

// --- isKeychainTokenRejected -----------------------------------------------

func TestIsKeychainTokenRejected_AllShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err  error
		want bool
	}{
		"data API 401":           {&api.HTTPError{StatusCode: http.StatusUnauthorized}, true},
		"data API 500":           {&api.HTTPError{StatusCode: http.StatusInternalServerError}, false},
		"ErrNotLoggedIn":         {auth.ErrNotLoggedIn, true},
		"wrapped ErrNotLoggedIn": {errors.New("resolve API token: " + auth.ErrNotLoggedIn.Error()), false /* string-only, no chain — not detected */},
		"sts 401":                {errors.New("token exchange: status 401: invalid_client"), true},
		"sts 400 invalid_grant":  {errors.New("token exchange: status 400: invalid_grant: token expired"), true},
		"sts 500":                {errors.New("token exchange: status 500: server_error"), false},
		"network error":          {errors.New("dial tcp: i/o timeout"), false},
	}

	// Confirm wrapped chains do propagate (the "wrapped ErrNotLoggedIn"
	// case above uses string substitution which intentionally doesn't
	// preserve the sentinel; this case uses fmt.Errorf %w which does).
	cases["fmt.Errorf %w ErrNotLoggedIn"] = struct {
		err  error
		want bool
	}{errors.Join(errors.New("resolve API token"), auth.ErrNotLoggedIn), true}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isKeychainTokenRejected(tc.err); got != tc.want {
				t.Errorf("isKeychainTokenRejected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- helpers for resolveAuthHostToken tests ---------------------------------

// authMemStore is an in-memory tokenstore.Store for tests that need a
// real tokenmanager.Manager. Mirrors the private memStore in auth-go's
// tokenmanager_test.go — that one isn't exported, so we duplicate the
// trivial implementation rather than pull in a fragile internal package.
type authMemStore struct {
	data map[string]tokens.TokenSet
}

func newAuthMemStore() *authMemStore { return &authMemStore{data: map[string]tokens.TokenSet{}} }

func (s *authMemStore) SaveTokens(profile string, t tokens.TokenSet) error {
	s.data[profile] = t
	return nil
}

func (s *authMemStore) LoadTokens(profile string) (tokens.TokenSet, error) {
	t, ok := s.data[profile]
	if !ok {
		return tokens.TokenSet{}, tokenstore.ErrNotFound
	}
	return t, nil
}

func (s *authMemStore) DeleteTokens(profile string) error {
	delete(s.data, profile)
	return nil
}

func saveCoreToken(t *testing.T, store tokenstore.Store, profile, accessToken string) {
	t.Helper()
	if err := store.SaveTokens(profile, tokens.TokenSet{AccessToken: accessToken}); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
}

func newResolveTestManager(t *testing.T, store tokenstore.Store, exchange func(context.Context, sts.ExchangeRequest) (*tokens.TokenSet, error)) *tokenmanager.Manager {
	t.Helper()
	mgr, err := tokenmanager.New(tokenmanager.Config{
		Issuer:   authResolveTestIssuer,
		ClientID: "entire-cli-test",
		STSPath:  "/sts/token",
		Store:    store,
	})
	if err != nil {
		t.Fatalf("tokenmanager.New: %v", err)
	}
	tokenmanager.SetExchangeForTest(t, mgr, exchange)
	return mgr
}

func TestAuthCmd_TopLevelLoginAndLogoutStillRegistered(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	want := map[string]bool{"login": false, "logout": false}
	for _, c := range root.Commands() {
		if _, ok := want[c.Use]; ok {
			want[c.Use] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("top-level %q command should remain registered", name)
		}
	}
}
