package coreapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// CoreOrigin reports the scheme://host the client dials, with the apiBasePath
// (and any trailing slash) stripped — the single source of truth display sites
// use so the named core can't diverge from where requests go.
func TestClient_CoreOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		coreURL string
		want    string
	}{
		{name: "bare origin", coreURL: "https://eu.auth.entire.io", want: "https://eu.auth.entire.io"},
		{name: "trailing slash", coreURL: "https://eu.auth.entire.io/", want: "https://eu.auth.entire.io"},
		{name: "with port", coreURL: "https://localhost:8443", want: "https://localhost:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewWithBearer(tt.coreURL, "tok")
			if err != nil {
				t.Fatalf("NewWithBearer: %v", err)
			}
			if got := c.CoreOrigin(); got != tt.want {
				t.Fatalf("CoreOrigin() = %q, want %q (apiBasePath and trailing slash must be stripped)", got, tt.want)
			}
		})
	}
}

// The whole point of the getter: a client built through the ENTIRE_TOKEN bypass
// reports the token's aud, so a display site asking the client "which core?"
// names the core the request actually dials — not a stale active context that a
// separate ResolveControlPlaneTarget would return.
//
// Not parallel: sets ENTIRE_TOKEN (process-global).
func TestNew_CoreOrigin_HonoursEnvToken(t *testing.T) {
	const core = "https://core.us.entire.io"
	t.Setenv(auth.EnvTokenVar, makeAudJWT(core))
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.CoreOrigin(); got != core {
		t.Fatalf("CoreOrigin() = %q, want the env token's aud %q", got, core)
	}
}

// makeAudJWT builds a JWT carrying only an aud claim. CoreURLFromEnvToken reads
// aud without verifying the signature, so the "sig" is a placeholder — but the
// header must name a real alg (alg:none is refused), matching how login JWTs
// look on the wire.
func makeAudJWT(aud string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(`{"aud":"` + aud + `"}`))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

func TestAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "non-API error returns empty",
			err:  errors.New("dial tcp: connection refused"),
			want: "",
		},
		{
			name: "prefers detail",
			err: &ErrorModelStatusCode{
				StatusCode: 409,
				Response: ErrorModel{
					Title:  NewOptString("Conflict"),
					Detail: NewOptString("organization name already taken"),
				},
			},
			want: "organization name already taken",
		},
		{
			name: "falls back to title when detail empty",
			err: &ErrorModelStatusCode{
				StatusCode: 403,
				Response:   ErrorModel{Title: NewOptString("Forbidden")},
			},
			want: "Forbidden",
		},
		{
			name: "falls back to status when title and detail empty",
			err:  &ErrorModelStatusCode{StatusCode: 500},
			want: "control-plane request failed with status 500",
		},
		{
			name: "unwraps a wrapped API error",
			err: fmt.Errorf("create org: %w", &ErrorModelStatusCode{
				StatusCode: 422,
				Response:   ErrorModel{Detail: NewOptString("name is required")},
			}),
			want: "name is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := APIError(tc.err); got != tc.want {
				t.Errorf("APIError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProviderSource_BearerAuth covers the three branches of the
// token-provider SecuritySource New() builds: a token passes through, a
// not-logged-in error gets the login hint, and any other error surfaces
// under the control-plane-token wrapper rather than the login hint.
func TestProviderSource_BearerAuth(t *testing.T) {
	t.Parallel()

	t.Run("token passes through", func(t *testing.T) {
		t.Parallel()
		src := &providerSource{provide: func(context.Context) (string, error) { return "tok-123", nil }}
		got, err := src.BearerAuth(context.Background(), "")
		if err != nil {
			t.Fatalf("BearerAuth: %v", err)
		}
		if got.Token != "tok-123" {
			t.Fatalf("Token = %q, want tok-123", got.Token)
		}
	})

	t.Run("not-logged-in maps to login hint", func(t *testing.T) {
		t.Parallel()
		src := &providerSource{provide: func(context.Context) (string, error) {
			return "", fmt.Errorf("wrapped: %w", auth.ErrNotLoggedIn)
		}}
		_, err := src.BearerAuth(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "entire login") {
			t.Fatalf("error = %v, want a login hint", err)
		}
		if !errors.Is(err, auth.ErrNotLoggedIn) {
			t.Fatalf("error must wrap ErrNotLoggedIn, got %v", err)
		}
	})

	t.Run("other errors surface verbatim", func(t *testing.T) {
		t.Parallel()
		// The active-context provider returns an already-tailored message; it
		// must reach the user unprefixed (no generic "resolve control-plane
		// token" wrapper burying it) and without the login hint.
		sentinel := errors.New("no usable login for \"ctx\" (https://core.example); run `entire login --server https://core.example`")
		src := &providerSource{provide: func(context.Context) (string, error) { return "", sentinel }}
		_, err := src.BearerAuth(context.Background(), "")
		if err == nil || err.Error() != sentinel.Error() {
			t.Fatalf("error = %v, want the provider message surfaced verbatim", err)
		}
	})
}

// providerSource must skip SessionAuth so no Cookie header is added — same
// contract as bearerOnlySource below, asserted here at the unit level.
func TestProviderSource_SkipsSessionAuth(t *testing.T) {
	t.Parallel()
	src := &providerSource{provide: func(context.Context) (string, error) { return "", nil }}
	if _, err := src.SessionAuth(context.Background(), ""); !errors.Is(err, ogenerrors.ErrSkipClientSecurity) {
		t.Fatalf("SessionAuth err = %v, want ErrSkipClientSecurity", err)
	}
}

// bearerOnlySource mirrors the CLI's bearerSource contract: a fixed
// bearer token, and ErrSkipClientSecurity for sessionAuth so the
// generated middleware does NOT add a `Cookie: entire_session=` header.
// Used by TestBearerOnlySource_NoCookieOnTheWire to nail down the
// "bearer-only, no cookie" contract at the HTTP layer.
type bearerOnlySource struct{}

func (bearerOnlySource) BearerAuth(context.Context, OperationName) (BearerAuth, error) {
	return BearerAuth{Token: "test-bearer"}, nil
}

func (bearerOnlySource) SessionAuth(context.Context, OperationName) (SessionAuth, error) {
	return SessionAuth{}, ogenerrors.ErrSkipClientSecurity
}

// TestBearerOnlySource_NoCookieOnTheWire documents the SessionAuth
// empty-value contract by checking the wire: any operation issued by a
// Client built with a SessionAuth-skipping source must NOT carry a
// Cookie header. (ogen's securitySessionAuth unconditionally calls
// req.AddCookie, so returning SessionAuth{} with a nil error would send
// an empty `entire_session=` cookie; only ErrSkipClientSecurity prevents
// the cookie from being added.)
func TestBearerOnlySource_NoCookieOnTheWire(t *testing.T) {
	t.Parallel()

	// The handler runs on httptest's goroutine and the assertion runs
	// on the test goroutine; HTTP completion isn't a happens-before
	// edge the race detector recognises. Pass the captured header
	// across through a buffered channel so -race stays happy.
	cookieCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieCh <- r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		// Minimal valid ListOrgMembersOutputBody payload so the response
		// decoder doesn't blow up; we only care about the inbound headers.
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"members":[]}`)); err != nil {
			t.Errorf("writing test response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, bearerOnlySource{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// ListOrgMembers is a simple GET that exercises the security
	// middleware; the result itself is irrelevant to this test.
	if _, err := c.ListOrgMembers(context.Background(), ListOrgMembersParams{OrgId: "01H000000000000000000000A1"}); err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}

	cookieHeader := <-cookieCh
	if cookieHeader != "" {
		t.Errorf("outbound Cookie header = %q, want empty (bearer-only contract)", cookieHeader)
	}
}

// TestListProjectRepos_UnknownEnumValuesPassThrough locks in the forward-compat
// contract for the display-only read-model enums loosened in
// spec/normalize.go's loosenReadModelEnums: Repo.state, Repo.visibility, and
// Repo.objectFormat are plain strings on the client, so a value the server adds
// later (a new lifecycle state, a new visibility) must decode and pass through
// verbatim rather than fail the whole `repo list` request in ogen's Validate().
// Before loosening, these were strict enums whose Validate() aborted the entire
// response on the first unknown value.
func TestListProjectRepos_UnknownEnumValuesPassThrough(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Values the current spec's enums did NOT allow.
		if _, err := w.Write([]byte(`{"repos":[{"id":"01H000000000000000000000A1","owningProjectId":"01H000000000000000000000P1","name":"demo","state":"archiving","visibility":"internal","objectFormat":"sha512"}]}`)); err != nil {
			t.Errorf("writing test response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, bearerOnlySource{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	out, err := c.ListProjectRepos(context.Background(), ListProjectReposParams{ProjectId: "01H000000000000000000000P1"})
	if err != nil {
		t.Fatalf("ListProjectRepos with unknown enum values must not fail (forward-compat), got: %v", err)
	}
	if len(out.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1", len(out.Repos))
	}
	repo := out.Repos[0]
	if got := repo.State.Or(""); got != "archiving" {
		t.Errorf("State = %q, want the unknown value %q passed through verbatim", got, "archiving")
	}
	if got := repo.Visibility.Or(""); got != "internal" {
		t.Errorf("Visibility = %q, want the unknown value %q passed through verbatim", got, "internal")
	}
	if got := repo.ObjectFormat.Or(""); got != "sha512" {
		t.Errorf("ObjectFormat = %q, want the unknown value %q passed through verbatim", got, "sha512")
	}
}

// TestGetJSON_SuccessDecodesAndSendsBearer covers the escape-hatch GET added for
// endpoints not yet in the generated spec: it must resolve the path against the
// client's /api/v1 base, apply the bearer from the SecuritySource, encode the
// query, and decode a 2xx JSON body into dst.
func TestGetJSON_SuccessDecodesAndSendsBearer(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"n":7}`)) //nolint:errcheck // test
	}))
	t.Cleanup(srv.Close)

	c, err := NewWithBearer(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewWithBearer: %v", err)
	}
	var dst struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := c.GetJSON(context.Background(), "repos/R/ci-builds", url.Values{"limit": {"100"}}, &dst); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if gotPath != "/api/v1/repos/R/ci-builds" {
		t.Errorf("path = %q, want /api/v1/repos/R/ci-builds", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if gotQuery != "limit=100" {
		t.Errorf("query = %q, want limit=100", gotQuery)
	}
	if !dst.OK || dst.N != 7 {
		t.Errorf("decoded = %+v, want {OK:true N:7}", dst)
	}
}

// TestGetJSON_Non2xxReturnsProblemDetail asserts a non-2xx response surfaces as
// an *ErrorModelStatusCode carrying the RFC 7807 detail, so APIError renders the
// server's message exactly as it does for generated operations.
func TestGetJSON_Non2xxReturnsProblemDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"title":"Not Found","detail":"repo not found","status":404}`)) //nolint:errcheck // test
	}))
	t.Cleanup(srv.Close)

	c, err := NewWithBearer(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewWithBearer: %v", err)
	}
	var dst map[string]any
	err = c.GetJSON(context.Background(), "repos/R/ci-builds", nil, &dst)
	if err == nil {
		t.Fatal("GetJSON: want error on 404")
	}
	var se *ErrorModelStatusCode
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want *ErrorModelStatusCode with 404", err)
	}
	if msg := APIError(err); msg != "repo not found" {
		t.Errorf("APIError = %q, want %q", msg, "repo not found")
	}
}
