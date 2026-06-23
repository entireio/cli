package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/require"
)

// newResolverTestClient returns a coreapi client pointed at an httptest server
// driven by h, so the name-resolvers can be exercised against canned ?name=
// responses. The server is torn down at test end.
func newResolverTestClient(t *testing.T, h http.HandlerFunc) *coreapi.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)
	return c
}

// failingCoreClient returns a client whose server fails the test if hit — used
// to assert the ULID/early-exit paths make no network call.
func failingCoreClient(t *testing.T) *coreapi.Client {
	t.Helper()
	return newResolverTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// writeProblem writes a control-plane RFC 7807 error so the ogen client decodes
// it as *ErrorModelStatusCode (which isNotFound keys on). A bare WriteHeader
// without the problem+json content type would instead surface as a decode error.
func writeProblem(t *testing.T, w http.ResponseWriter, status int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if _, err := fmt.Fprintf(w, `{"status":%d,"detail":"not found"}`, status); err != nil {
		t.Errorf("write problem: %v", err)
	}
}

func TestLooksLikeULID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{in: "01J0ABCDEFGHJKMNPQRSTVWXYZ", want: true}, // 26 chars, valid alphabet
		{in: "01j0abcdefghjkmnpqrstvwxyz", want: true}, // lowercase accepted
		{in: "acme", want: false},                      // short name
		{in: "my-project", want: false},                // hyphen not in alphabet
		{in: "", want: false},                          // empty
		{in: "01J0ABCDEFGHJKMNPQRSTVWXY", want: false}, // 25 chars
		{in: "01J0ABCDEFGHJKMNPQRSTVWXYZ0", want: false},
		{in: "01J0ABCDEFGHIKMNPQRSTVWXYZ", want: false}, // contains I
		{in: "01J0ABCDEFGHLKMNPQRSTVWXYZ", want: false}, // contains L
		{in: "01J0ABCDEFGHOKMNPQRSTVWXYZ", want: false}, // contains O
		{in: "01J0ABCDEFGHUKMNPQRSTVWXYZ", want: false}, // contains U
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeULID(tt.in); got != tt.want {
				t.Errorf("looksLikeULID(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseQualifiedHandle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in           string
		wantProvider string
		wantHandle   string
		wantErr      bool
	}{
		{in: "github:alice", wantProvider: "github", wantHandle: "alice"},
		{in: "github:alice:bob", wantProvider: "github", wantHandle: "alice:bob"}, // only first colon splits
		{in: "alice", wantErr: true},                                              // no provider prefix
		{in: "github:", wantErr: true},                                            // empty handle
		{in: ":alice", wantErr: true},                                             // empty provider
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			provider, handle, err := parseQualifiedHandle(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseQualifiedHandle(%q) expected error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQualifiedHandle(%q): %v", tt.in, err)
			}
			if provider != tt.wantProvider || handle != tt.wantHandle {
				t.Errorf("parseQualifiedHandle(%q) = (%q, %q), want (%q, %q)", tt.in, provider, handle, tt.wantProvider, tt.wantHandle)
			}
		})
	}
}

// validULID is accepted by looksLikeULID, so the resolvers short-circuit on it.
const validULID = "01J0ABCDEFGHJKMNPQRSTVWXYZ"

// apiProjectsPath is the projects collection path, shared across the resolver
// test handlers.
const apiProjectsPath = "/api/v1/projects"

func TestResolveOrgRef(t *testing.T) {
	t.Run("ULID passes through with no network call", func(t *testing.T) {
		got, err := resolveOrgRef(t.Context(), failingCoreClient(t), validULID)
		require.NoError(t, err)
		require.Equal(t, validULID, got)
	})

	t.Run("name resolves via ?name= to the single org id", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/orgs" || r.URL.Query().Get("name") != "acme" {
				t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"org":{"id":"01J0ORG0000000000000000001","name":"acme","region":"us","createdAt":"2020-01-01T00:00:00Z"}}`)
		})
		got, err := resolveOrgRef(t.Context(), c, "acme")
		require.NoError(t, err)
		require.Equal(t, "01J0ORG0000000000000000001", got)
	})

	t.Run("404 becomes a friendly not-found error", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeProblem(t, w, http.StatusNotFound)
		})
		_, err := resolveOrgRef(t.Context(), c, "ghost")
		require.ErrorContains(t, err, `no org named "ghost"`)
	})
}

func TestResolveProjectRef(t *testing.T) {
	t.Run("ULID passes through with no network call", func(t *testing.T) {
		got, err := resolveProjectRef(t.Context(), failingCoreClient(t), validULID)
		require.NoError(t, err)
		require.Equal(t, validULID, got)
	})

	t.Run("name resolves via ?name= to the single project id", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != apiProjectsPath || r.URL.Query().Get("name") != "widgets" {
				t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"project":{"id":"01J0PRJ0000000000000000001","name":"widgets","ownerType":"org","ownerId":"01J0ORG0000000000000000001","region":"us","createdAt":"2020-01-01T00:00:00Z"}}`)
		})
		got, err := resolveProjectRef(t.Context(), c, "widgets")
		require.NoError(t, err)
		require.Equal(t, "01J0PRJ0000000000000000001", got)
	})

	t.Run("404 becomes a friendly not-found error", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeProblem(t, w, http.StatusNotFound)
		})
		_, err := resolveProjectRef(t.Context(), c, "ghost")
		require.ErrorContains(t, err, `no project named "ghost"`)
	})
}

func TestResolveRepoRef(t *testing.T) {
	const projID = "01J0PRJ0000000000000000001"

	t.Run("ULID passes through with no network call", func(t *testing.T) {
		got, err := resolveRepoRef(t.Context(), failingCoreClient(t), validULID, "")
		require.NoError(t, err)
		require.Equal(t, validULID, got)
	})

	t.Run("name without --project is rejected before any call", func(t *testing.T) {
		_, err := resolveRepoRef(t.Context(), failingCoreClient(t), "web", "")
		require.ErrorContains(t, err, "pass --project")
	})

	t.Run("name resolves project then repo via ?name=", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case apiProjectsPath:
				fmt.Fprintf(w, `{"project":{"id":%q,"name":"widgets","ownerType":"org","ownerId":"01J0ORG0000000000000000001","region":"us","createdAt":"2020-01-01T00:00:00Z"}}`, projID)
			case apiProjectsPath + "/" + projID + "/repos":
				if r.URL.Query().Get("name") != "web" {
					t.Errorf("unexpected repo name query %q", r.URL.RawQuery)
				}
				fmt.Fprintf(w, `{"repo":{"id":"01J0REPO000000000000000001","owningProjectId":%q,"name":"web"}}`, projID)
			default:
				t.Errorf("unexpected path %q", r.URL.Path)
			}
		})
		got, err := resolveRepoRef(t.Context(), c, "web", "widgets")
		require.NoError(t, err)
		require.Equal(t, "01J0REPO000000000000000001", got)
	})

	t.Run("repo 404 becomes a friendly not-found error", func(t *testing.T) {
		c := newResolverTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case apiProjectsPath:
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"project":{"id":%q,"name":"widgets","ownerType":"org","ownerId":"01J0ORG0000000000000000001","region":"us","createdAt":"2020-01-01T00:00:00Z"}}`, projID)
			case apiProjectsPath + "/" + projID + "/repos":
				writeProblem(t, w, http.StatusNotFound)
			default:
				t.Errorf("unexpected path %q", r.URL.Path)
			}
		})
		_, err := resolveRepoRef(t.Context(), c, "ghost", "widgets")
		require.ErrorContains(t, err, `no repo named "ghost"`)
	})
}

func TestFilterProjectsByName(t *testing.T) {
	t.Parallel()
	projects := []coreapi.Project{
		{ID: "1", Name: "a"},
		{ID: "2", Name: "b"},
		{ID: "3", Name: "a"},
	}

	t.Run("empty name returns all", func(t *testing.T) {
		t.Parallel()
		if got := filterProjectsByName(projects, ""); len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("exact filter", func(t *testing.T) {
		t.Parallel()
		got := filterProjectsByName(projects, "a")
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for _, p := range got {
			if p.Name != "a" {
				t.Errorf("unexpected project %q", p.Name)
			}
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if got := filterProjectsByName(projects, "z"); len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
}
