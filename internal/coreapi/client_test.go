package coreapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		// Values the current spec's enums did NOT allow, and no
		// "capabilities": a required-but-unread field must not be needed.
		if _, err := w.Write([]byte(`{"repos":[{"id":"01H000000000000000000000A1","owningProjectId":"01H000000000000000000000P1","name":"demo","state":"archiving","visibility":"internal","objectFormat":"sha512","provider":"gitlab"}]}`)); err != nil {
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

// TestListRepos_UnsentRequiredReadFieldsDecode locks in the backward-compat
// contract for spec/normalize.go's loosenReadModelRequired on RepoIndexEntry:
// `org` and `provider` ship as "required" upstream, but the CLI reads neither,
// so a core that predates them — or a mixed-version roll mid-deploy — must not
// fail the response. ogen's decoder rejects a missing required field outright,
// which would take out the whole index read rather than one field.
//
// RepoIndexEntry gets its own test because ListRepos is the widest consumer of
// the loosening: the consolidated repos index is what resolveRepoCellTarget
// routes repo-scoped requests with, and what `search`, `repo mirror` and the
// dispatch wizard page through. A decode failure here is not one command
// erroring, it is cell routing and search going dark at once — a materially
// different blast radius from TestListProjectRepos_UnknownEnumValuesPassThrough,
// which covers the sibling loosening on Repo.
//
// The unknown `permission` value pins the matching loosenReadModelEnums entry
// in the same request: it was a closed enum upstream, and a value added later
// must pass through rather than abort the read.
func TestListRepos_UnsentRequiredReadFieldsDecode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No "org", no "provider", no "candidatesIncomplete" — all required
		// upstream, none read here. "permission" carries a value the current
		// spec's enum did NOT allow.
		if _, err := w.Write([]byte(`{"repos":[{` +
			`"id":"01H000000000000000000000R1","name":"web","full_name":"gh/acme/web",` +
			`"jurisdiction":"us","cell":"aws-us-east-2","clusterSlug":"us","visibility":"private",` +
			`"permission":"triage",` +
			`"placements":[{"id":"01H000000000000000000000R1","jurisdiction":"us","cell":"aws-us-east-2","clusterSlug":"us","mirror":false,"status":"ready"}]` +
			`}],"truncated":false}`)); err != nil {
			t.Errorf("writing test response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, bearerOnlySource{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	out, err := c.ListRepos(context.Background(), ListReposParams{})
	if err != nil {
		t.Fatalf("ListRepos without org/provider must not fail (backward-compat), got: %v", err)
	}
	if len(out.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1", len(out.Repos))
	}
	entry := out.Repos[0]
	if entry.Org.IsSet() {
		t.Errorf("Org.IsSet() = true, want false: an absent field must decode as unset, not fail the read")
	}
	if entry.Provider.IsSet() {
		t.Errorf("Provider.IsSet() = true, want false: an absent field must decode as unset, not fail the read")
	}
	if got := entry.Permission.Or(""); got != "triage" {
		t.Errorf("Permission = %q, want the unknown value %q passed through verbatim", got, "triage")
	}
	if got := entry.FullName; got != "gh/acme/web" {
		t.Errorf("FullName = %q, want the entry to decode intact alongside the absent fields", got)
	}
}

// TestListOrgsAndProjects_UnsentCapabilitiesDecode is the same contract for the
// other two schemas that gained a required `capabilities` the CLI never reads.
// Cheaper to lose than the repo index, but the loosening is only real if a
// response without the field actually decodes.
func TestListOrgsAndProjects_UnsentCapabilitiesDecode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		call func(*Client) (int, error)
	}{
		{
			name: "orgs",
			body: `{"orgs":[{"id":"01H000000000000000000000O1","name":"acme","region":"us","createdAt":"2026-01-01T00:00:00Z"}]}`,
			call: func(c *Client) (int, error) {
				out, err := c.ListOrgs(context.Background(), ListOrgsParams{})
				if err != nil {
					return 0, err
				}
				return len(out.Response.Orgs), nil
			},
		},
		{
			name: "projects",
			body: `{"projects":[{"id":"01H000000000000000000000P1","name":"core","ownerType":"org","ownerId":"01H000000000000000000000O1","region":"us","createdAt":"2026-01-01T00:00:00Z"}]}`,
			call: func(c *Client) (int, error) {
				out, err := c.ListProjects(context.Background(), ListProjectsParams{})
				if err != nil {
					return 0, err
				}
				return len(out.Projects), nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Errorf("writing test response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			c, err := NewClient(srv.URL, bearerOnlySource{})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			n, err := tc.call(c)
			if err != nil {
				t.Fatalf("list without capabilities must not fail (backward-compat), got: %v", err)
			}
			if n != 1 {
				t.Fatalf("decoded %d item(s), want 1", n)
			}
		})
	}
}
