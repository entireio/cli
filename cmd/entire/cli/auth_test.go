package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// --- status -----------------------------------------------------------------

const testCoreURL = "https://eu.auth.entire.io"

// okProfile is a profileFetcher returning a fully-populated profile, for the
// happy-path status tests.
func okProfile(context.Context, string, string) (*authProfile, error) {
	return &authProfile{
		Handle:         "alice",
		DisplayName:    "Alice Smith",
		Email:          "alice@example.com",
		Provider:       "github",
		ProviderUserID: "alice",
	}, nil
}

// unusedProfile is a profileFetcher that fails the test if called — for the
// not-logged-in path, where the empty-token check short-circuits before /me.
func unusedProfile(t *testing.T) profileFetcher {
	return func(context.Context, string, string) (*authProfile, error) {
		t.Helper()
		t.Fatal("/me should not be called when there is no token")
		return nil, errors.New("unreachable")
	}
}

// rejecting returns a profileFetcher that always fails with err.
func rejecting(err error) profileFetcher {
	return func(context.Context, string, string) (*authProfile, error) { return nil, err }
}

// noSessions is a authSessionLister returning an empty list (no table rendered).
func noSessions(context.Context, string, string) ([]api.AuthSession, error) { return nil, nil }

func TestRunAuthStatus_NotLoggedIn(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	target := statusTarget{coreURL: testCoreURL} // empty token
	if err := runAuthStatus(context.Background(), &out, unusedProfile(t), noSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Not logged in to "+testCoreURL) {
		t.Fatalf("output = %q, want 'Not logged in' message", out.String())
	}
}

func TestRunAuthStatus_LoggedIn(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Logged in to "+testCoreURL) {
		t.Fatalf("output = %q, want 'Logged in' to the active context's core", got)
	}
	if !strings.Contains(got, "Alice Smith") || !strings.Contains(got, "@alice") || !strings.Contains(got, "<alice@example.com>") {
		t.Fatalf("output = %q, want profile header (name/@handle/<email>)", got)
	}
	if !strings.Contains(got, "github/alice") {
		t.Fatalf("output = %q, want provider identity", got)
	}
	if !strings.Contains(got, "Context:") || !strings.Contains(got, "eu.auth.entire.io") {
		t.Fatalf("output = %q, want active-context line", got)
	}
	// noSessions returns an empty list, so no table is rendered.
	if strings.Contains(got, "Active sessions") {
		t.Fatalf("output = %q, empty session list should render no table", got)
	}
}

// TestWriteProfileLines_Jurisdiction verifies the home jurisdiction slug is
// rendered (so `auth token --jurisdiction` is discoverable) and omitted when the
// server didn't populate it.
func TestWriteProfileLines_Jurisdiction(t *testing.T) {
	t.Parallel()

	var withJ bytes.Buffer
	writeProfileLines(&withJ, &authProfile{Handle: "alice", Provider: "github", Jurisdiction: "us"})
	if !strings.Contains(withJ.String(), "Jurisdiction: us") {
		t.Fatalf("output = %q, want a 'Jurisdiction: us' line", withJ.String())
	}

	var withoutJ bytes.Buffer
	writeProfileLines(&withoutJ, &authProfile{Handle: "alice", Provider: "github"})
	if strings.Contains(withoutJ.String(), "Jurisdiction") {
		t.Fatalf("output = %q, want no Jurisdiction line when the slug is empty", withoutJ.String())
	}
}

// In ENTIRE_TOKEN mode there is no stored context, keychain slot, or revocable
// session: status names the env-token core and bearer source, and renders none
// of the context/keychain/session lines. listSessions must not be called — you
// can't manage an env-token session.
func TestRunAuthStatus_EnvTokenMode(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", envToken: true}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		t.Helper()
		t.Fatal("listSessions must not be called in ENTIRE_TOKEN mode")
		return nil, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Logged in to "+testCoreURL) {
		t.Fatalf("output = %q, want 'Logged in' to the env token's core", got)
	}
	if !strings.Contains(got, "Alice Smith") {
		t.Fatalf("output = %q, want the profile header", got)
	}
	if !strings.Contains(got, auth.EnvTokenVar+" environment variable") {
		t.Fatalf("output = %q, want the ENTIRE_TOKEN bearer note", got)
	}
	for _, unwanted := range []string{"Context:", "stored in OS keychain", "Active sessions", "login contexts saved"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output = %q, must not contain %q in ENTIRE_TOKEN mode", got, unwanted)
		}
	}
}

// resolveEnvTokenStatusTarget reads the core from the token's aud (the same
// origin coreapi.New dials) and uses the token verbatim as the bearer; a blank
// or aud-less token is a fail-closed error, never a fall-back to a context.
func TestResolveEnvTokenStatusTarget(t *testing.T) {
	t.Parallel()

	t.Run("valid token yields aud core + verbatim bearer", func(t *testing.T) {
		t.Parallel()
		tok := makeJWT(t, `{"alg":"HS256","typ":"JWT"}`, `{"aud":"`+testCoreURL+`"}`)
		got, err := resolveEnvTokenStatusTarget("  " + tok + "  ") // surrounding whitespace trimmed
		if err != nil {
			t.Fatalf("resolveEnvTokenStatusTarget: %v", err)
		}
		if got.coreURL != testCoreURL {
			t.Fatalf("coreURL = %q, want the token's aud %q", got.coreURL, testCoreURL)
		}
		if got.token != tok {
			t.Fatalf("token = %q, want the verbatim env token", got.token)
		}
		if !got.envToken {
			t.Fatal("envToken = false, want true")
		}
	})

	t.Run("blank is fail-closed", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveEnvTokenStatusTarget("   "); err == nil {
			t.Fatal("want an error for a blank ENTIRE_TOKEN, got nil")
		}
	})

	t.Run("token without a URL aud is rejected", func(t *testing.T) {
		t.Parallel()
		tok := makeJWT(t, `{"alg":"HS256","typ":"JWT"}`, `{"sub":"ci-runner"}`)
		if _, err := resolveEnvTokenStatusTarget(tok); err == nil {
			t.Fatal("want an error when the token has no URL-shaped aud, got nil")
		}
	})
}

func TestRunAuthStatus_RendersSessionsTable(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	lastUsed := "2026-05-01T00:00:00Z"
	listSessions := func(_ context.Context, coreURL, token string) ([]api.AuthSession, error) {
		if coreURL != testCoreURL || token != "tok" {
			t.Errorf("listSessions called with (%q, %q), want the active core+token", coreURL, token)
		}
		return []api.AuthSession{
			{ID: "fam-1", Name: "OIDC login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z", LastUsedAt: &lastUsed},
			{ID: "fam-2", Name: "OIDC login", CreatedAt: "2026-02-01T00:00:00Z", ExpiresAt: "2026-12-15T00:00:00Z"},
		}, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Active sessions (2):") {
		t.Fatalf("output = %q, want active-sessions heading with count", got)
	}
	for _, want := range []string{"NAME", "CREATED", "LAST USED", "EXPIRES", formatAuthDate("2026-01-01T00:00:00Z"), "never"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want table to contain %q", got, want)
		}
	}
	if !strings.Contains(got, "entire logout --everywhere") {
		t.Fatalf("output = %q, want logout hint tying the table to logout", got)
	}
}

func TestFormatAuthDate_DoesNotShiftUTCDateToLocalTimezone(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("PST", -8*60*60)
	t.Cleanup(func() {
		time.Local = oldLocal
	})

	got := formatAuthDate("2026-01-01T00:00:00Z")
	if got != "2026-01-01" {
		t.Fatalf("formatAuthDate() = %q, want %q", got, "2026-01-01")
	}
}

func TestRunAuthStatus_SessionListFailureIsSoftNote(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		return nil, errors.New("sessions endpoint unreachable")
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err) // liveness already passed via /me
	}
	got := out.String()
	if !strings.Contains(got, "Logged in to "+testCoreURL) {
		t.Fatalf("output = %q, want still-logged-in", got)
	}
	if !strings.Contains(got, "could not list active sessions") {
		t.Fatalf("output = %q, want soft note about the listing failure", got)
	}
}

// TestRunAuthStatus_QueriesActiveContextCore pins the multi-core fix: /me is
// called against the active context's core with that context's token, not a
// static AuthBaseURL.
func TestRunAuthStatus_QueriesActiveContextCore(t *testing.T) {
	t.Parallel()

	var gotCoreURL, gotToken string
	fetch := func(_ context.Context, coreURL, token string) (*authProfile, error) {
		gotCoreURL, gotToken = coreURL, token
		return &authProfile{Handle: "alice"}, nil
	}
	target := statusTarget{coreURL: testCoreURL, token: "eu-session-tok", activeContext: "eu.auth.entire.io", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, fetch, noSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCoreURL != testCoreURL {
		t.Errorf("fetchProfile coreURL = %q, want %q", gotCoreURL, testCoreURL)
	}
	if gotToken != "eu-session-tok" {
		t.Errorf("fetchProfile token = %q, want the active context's token", gotToken)
	}
}

func TestRunAuthStatus_MultipleContextsHint(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "a", totalContexts: 3}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "3 login contexts saved") {
		t.Fatalf("output = %q, want multi-context hint", out.String())
	}
}

func TestRunAuthStatus_InvalidTokenShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		// 401 from GET /me as a typed core error.
		"typed 401": &coreapi.ErrorModelStatusCode{StatusCode: http.StatusUnauthorized},
		// 401 whose body isn't JSON: ogen fails to decode and the status is
		// only in the message string. This is the shape `auth status` hit in
		// the wild against a cross-core mismatch.
		"non-JSON 401": errors.New("decode response: default (code 401): unexpected Content-Type: text/plain"),
		// STS rejection during a split-host exchange (no typed sentinel).
		"sts 4xx": errors.New("token exchange: status 400: invalid_grant: subject_token expired"),
		// Expired core JWT surfaces as a wrapped ErrNotLoggedIn.
		"wrapped not-logged-in": &wrappedTestError{msg: "fetch profile", inner: auth.ErrNotLoggedIn},
	}

	for name, fetchErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := statusTarget{coreURL: testCoreURL, token: "tok"}
			var out bytes.Buffer
			if err := runAuthStatus(context.Background(), &out, rejecting(fetchErr), noSessions, target); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out.String(), "no longer valid") {
				t.Fatalf("output = %q, want invalid-token message", out.String())
			}
			if !strings.Contains(out.String(), "entire login") {
				t.Fatalf("output = %q, want re-auth hint", out.String())
			}
		})
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

	target := statusTarget{coreURL: testCoreURL, token: "tok"}

	var out bytes.Buffer
	err := runAuthStatus(context.Background(), &out, rejecting(errors.New("connection refused")), noSessions, target)
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
		// ogen decode failure on a non-JSON 401 body (the /me cross-core case).
		"non-JSON 401 decode": {errors.New("decode response: default (code 401): unexpected Content-Type: text/plain"), true},
		"non-JSON 500 decode": {errors.New("decode response: default (code 500): unexpected Content-Type: text/plain"), false},
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
