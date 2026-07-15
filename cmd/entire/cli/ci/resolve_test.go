package ci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/ci"
	"github.com/entireio/cli/internal/coreapi"
)

// Valid ULID-shaped fixtures (26 Crockford base32 chars, no I/L/O/U) so the
// resolver tests exercise the ULID short-circuit instead of a name lookup.
const (
	ulidOrgAcme        = "0123456789ABCDEFGHJKMNPQR1"
	ulidProjectWidgets = "0123456789ABCDEFGHJKMNPQR3"
	ulidRepoWeb        = "0123456789ABCDEFGHJKMNPQR5"
)

// widgetsProject is the canonical project fixture. The generated response
// validator requires OwnerType/OwnerId to be set to valid values, so a bare
// {ID, Name} would fail decode.
func widgetsProject() coreapi.OptProject {
	return coreapi.NewOptProject(coreapi.Project{
		ID:        ulidProjectWidgets,
		Name:      "widgets",
		OwnerId:   ulidOrgAcme,
		OwnerType: coreapi.ProjectOwnerTypeOrg,
	})
}

// resolveTestClient builds a coreapi client pointed at a test server whose
// handler is h, and returns the client plus a counter of HTTP requests seen.
// It lets the tests assert the load-bearing invariant: a ULID or mirror ref
// makes zero network calls, a native path makes exactly the expected lookups.
func resolveTestClient(t *testing.T, h http.HandlerFunc) (*coreapi.Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewWithBearer: %v", err)
	}
	return c, &calls
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// nativeRepoHandler serves the two by-name lookups a native path resolution
// makes: /projects (project by name) and /projects/{id}/repos (repo by name),
// each returning the single match under its singular field, mirroring the real
// server. Either fixture may be left zero to simulate "not found".
func nativeRepoHandler(t *testing.T, project coreapi.OptProject, repo coreapi.OptRepo, gotRepoName *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos"):
			if gotRepoName != nil {
				*gotRepoName = r.URL.Query().Get("name")
			}
			writeJSON(t, w, &coreapi.ListProjectReposOutputBody{Repo: repo})
		case strings.HasSuffix(r.URL.Path, "/projects"):
			writeJSON(t, w, &coreapi.ListProjectsOutputBody{Project: project})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func TestResolveNativeRepo_ULIDPassthrough(t *testing.T) {
	t.Parallel()
	c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected HTTP call for a ULID ref")
		w.WriteHeader(http.StatusInternalServerError)
	})
	got, err := ci.ResolveNativeRepo(context.Background(), c, ulidRepoWeb)
	if err != nil {
		t.Fatalf("ResolveNativeRepo: %v", err)
	}
	if got != ulidRepoWeb {
		t.Errorf("ResolveNativeRepo = %q, want the ULID unchanged", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("ULID ref made %d HTTP calls, want 0", n)
	}
}

func TestResolveNativeRepo_NativePath(t *testing.T) {
	t.Parallel()
	project := widgetsProject()
	repo := coreapi.NewOptRepo(coreapi.Repo{ID: ulidRepoWeb, Name: "web"})

	t.Run("plain project/repo resolves", func(t *testing.T) {
		t.Parallel()
		var gotRepoName string
		c, calls := resolveTestClient(t, nativeRepoHandler(t, project, repo, &gotRepoName))
		got, err := ci.ResolveNativeRepo(context.Background(), c, "widgets/web")
		if err != nil {
			t.Fatalf("ResolveNativeRepo: %v", err)
		}
		if got != ulidRepoWeb {
			t.Errorf("ResolveNativeRepo = %q, want %q", got, ulidRepoWeb)
		}
		if gotRepoName != "web" {
			t.Errorf("server received repo name=%q, want %q (filtering must be server-side)", gotRepoName, "web")
		}
		// project-by-name + repo-by-name = two lookups.
		if n := calls.Load(); n != 2 {
			t.Errorf("native path made %d HTTP calls, want 2", n)
		}
	})

	t.Run("/et/ prefix is tolerated", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, nativeRepoHandler(t, project, repo, nil))
		got, err := ci.ResolveNativeRepo(context.Background(), c, "/et/widgets/web")
		if err != nil {
			t.Fatalf("ResolveNativeRepo: %v", err)
		}
		if got != ulidRepoWeb {
			t.Errorf("ResolveNativeRepo(/et/…) = %q, want %q", got, ulidRepoWeb)
		}
	})
}

func TestResolveNativeRepo_MirrorRejected(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"gh/acme/web", "/gh/acme/web", "github.com/acme/web"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("unexpected HTTP call for a mirror ref")
				w.WriteHeader(http.StatusInternalServerError)
			})
			_, err := ci.ResolveNativeRepo(context.Background(), c, ref)
			if err == nil || !strings.Contains(err.Error(), "not GitHub mirrors") {
				t.Errorf("ResolveNativeRepo(%q) err = %v, want a GitHub-mirror rejection", ref, err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("mirror ref made %d HTTP calls, want 0", n)
			}
		})
	}
}

func TestResolveNativeRepo_UnknownProject(t *testing.T) {
	t.Parallel()
	// Empty project match; the repo lookup must never be reached.
	c, _ := resolveTestClient(t, nativeRepoHandler(t, coreapi.OptProject{}, coreapi.OptRepo{}, nil))
	_, err := ci.ResolveNativeRepo(context.Background(), c, "ghost/web")
	if err == nil || !strings.Contains(err.Error(), "no project named") {
		t.Errorf("ResolveNativeRepo unknown project: err = %v, want a \"no project named\" error", err)
	}
}

func TestResolveNativeRepo_UnknownRepo(t *testing.T) {
	t.Parallel()
	project := widgetsProject()
	// Project resolves, repo does not.
	c, _ := resolveTestClient(t, nativeRepoHandler(t, project, coreapi.OptRepo{}, nil))
	_, err := ci.ResolveNativeRepo(context.Background(), c, "widgets/ghost")
	if err == nil || !strings.Contains(err.Error(), "no repo named") {
		t.Errorf("ResolveNativeRepo unknown repo: err = %v, want a \"no repo named\" error", err)
	}
}

func TestResolveNativeRepo_InvalidShape(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"", "  ", "widgets", "widgets/", "/et/widgets"} {
		t.Run("ref="+ref, func(t *testing.T) {
			t.Parallel()
			c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("unexpected HTTP call for an invalid ref")
				w.WriteHeader(http.StatusInternalServerError)
			})
			if _, err := ci.ResolveNativeRepo(context.Background(), c, ref); err == nil {
				t.Errorf("ResolveNativeRepo(%q) expected an error", ref)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("invalid ref %q made %d HTTP calls, want 0", ref, n)
			}
		})
	}
}
