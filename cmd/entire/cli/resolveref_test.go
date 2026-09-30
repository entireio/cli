package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// Valid ULID-shaped fixtures (26 Crockford base32 chars, no I/L/O/U) so the
// resolver tests exercise the ULID short-circuit instead of a name lookup.
const (
	ulidOrgAcme        = "0123456789ABCDEFGHJKMNPQR1"
	ulidOrgGlobex      = "0123456789ABCDEFGHJKMNPQR2"
	ulidProjectWidgets = "0123456789ABCDEFGHJKMNPQR3"
	ulidRepoWeb        = "0123456789ABCDEFGHJKMNPQR5"
	ulidAccount        = "0123456789ABCDEFGHJKMNPQR4"
	ulidResolvedAcct   = "0123456789ABCDEFGHJKMNPQR9"
)

// resolveTestClient builds a coreapi client pointed at a test server whose
// handler is h, and returns the client plus a counter of HTTP requests seen.
// It lets the resolver tests assert the load-bearing invariant from
// resolveref.go's doc comment: a ULID ref makes zero network calls, a project
// or repo name ref makes exactly one, and an org name ref makes one per page
// of the caller's org listing.
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

func TestResolveOrgRef(t *testing.T) {
	t.Parallel()

	t.Run("ULID passes through without a network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		got, err := resolveOrgRef(context.Background(), c, ulidOrgGlobex)
		if err != nil {
			t.Fatalf("resolveOrgRef: %v", err)
		}
		if got != ulidOrgGlobex {
			t.Errorf("resolveOrgRef = %q, want the ULID unchanged", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID ref made %d HTTP calls, want 0", n)
		}
	})

	t.Run("name is matched against the caller's own orgs, across pages", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, orgPagesHandler(t,
			[]coreapi.Org{{ID: ulidOrgAcme, Name: "acme"}},
			[]coreapi.Org{{ID: ulidOrgGlobex, Name: "globex"}},
		))
		got, err := resolveOrgRefResolved(context.Background(), c, "globex")
		if err != nil {
			t.Fatalf("resolveOrgRef: %v", err)
		}
		if got.ID != ulidOrgGlobex || got.Name != "globex" {
			t.Errorf("resolveOrgRef = %+v, want globex on page 2", got)
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("name ref made %d HTTP calls, want 2 (one per page)", n)
		}
	})

	t.Run("unknown name is a friendly error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, orgPagesHandler(t, []coreapi.Org{{ID: ulidOrgAcme, Name: "acme"}}))
		_, err := resolveOrgRef(context.Background(), c, "nope")
		if err == nil || !strings.Contains(err.Error(), "no org named") {
			t.Errorf("resolveOrgRef unknown name: err = %v, want a \"no org named\" error", err)
		}
	})

	t.Run("name match is case-insensitive", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, orgPagesHandler(t, []coreapi.Org{{ID: ulidOrgAcme, Name: "Acme"}}))
		got, err := resolveOrgRefResolved(context.Background(), c, "acme")
		if err != nil {
			t.Fatalf("resolveOrgRef case mismatch: %v", err)
		}
		if got.ID != ulidOrgAcme || got.Name != "Acme" {
			t.Errorf("resolveOrgRef = %+v, want Acme with the server's spelling", got)
		}
	})

	t.Run("exact-case match wins over case-folded ones", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, orgPagesHandler(t,
			[]coreapi.Org{{ID: ulidOrgAcme, Name: "ACME"}},
			[]coreapi.Org{{ID: ulidOrgGlobex, Name: "acme"}},
		))
		got, err := resolveOrgRef(context.Background(), c, "acme")
		if err != nil {
			t.Fatalf("resolveOrgRef exact-case: %v", err)
		}
		if got != ulidOrgGlobex {
			t.Errorf("resolveOrgRef = %q, want the exact-case match %q", got, ulidOrgGlobex)
		}
	})

	t.Run("several case-folded matches are ambiguous", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, orgPagesHandler(t,
			[]coreapi.Org{{ID: ulidOrgAcme, Name: "ACME"}, {ID: ulidOrgGlobex, Name: "Acme"}},
		))
		_, err := resolveOrgRef(context.Background(), c, "acme")
		if err == nil {
			t.Fatal("resolveOrgRef case-folded ambiguous name: want an error")
		}
		msg := err.Error()
		for _, want := range []string{"2 orgs are named \"acme\"", "ACME  " + ulidOrgAcme, "Acme  " + ulidOrgGlobex} {
			if !strings.Contains(msg, want) {
				t.Errorf("ambiguous error %q lacks %q", msg, want)
			}
		}
	})

	t.Run("several same-named orgs list each ULID", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, orgPagesHandler(t,
			[]coreapi.Org{{ID: ulidOrgAcme, Name: "acme"}},
			[]coreapi.Org{{ID: ulidOrgGlobex, Name: "acme"}},
		))
		_, err := resolveOrgRef(context.Background(), c, "acme")
		if err == nil {
			t.Fatal("resolveOrgRef ambiguous name: want an error")
		}
		msg := err.Error()
		for _, want := range []string{"2 orgs are named \"acme\"", "pass the ULID", "acme  " + ulidOrgAcme, "acme  " + ulidOrgGlobex} {
			if !strings.Contains(msg, want) {
				t.Errorf("ambiguous error %q lacks %q", msg, want)
			}
		}
	})
}

// orgPagesHandler serves GET /api/v1/orgs as the given pages, chained through
// nextPageToken. It refuses a ?name= filter: the resolver must read the whole
// listing, not the server's global by-name lookup.
func orgPagesHandler(t *testing.T, pages ...[]coreapi.Org) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orgs" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if name := r.URL.Query().Get("name"); name != "" {
			t.Errorf("server received name=%q; the resolver must not use the global lookup", name)
		}
		page := 0
		if cursor := r.URL.Query().Get("pageToken"); cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n < 1 || n >= len(pages) {
				t.Errorf("unexpected cursor %q", cursor)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			page = n
		}
		body := coreapi.ListOrgsOutputBody{Orgs: pages[page]}
		if page+1 < len(pages) {
			body.NextPageToken = coreapi.NewOptString(strconv.Itoa(page + 1))
		}
		if err := printJSON(w, &body); err != nil {
			t.Errorf("encode orgs: %v", err)
		}
	}
}

func TestResolveProjectRef(t *testing.T) {
	t.Parallel()
	matched := coreapi.NewOptProject(coreapi.Project{ID: ulidProjectWidgets, Name: "widgets", OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg})

	t.Run("ULID passes through without a network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		got, err := resolveProjectRef(context.Background(), c, ulidProjectWidgets)
		if err != nil {
			t.Fatalf("resolveProjectRef: %v", err)
		}
		if got != ulidProjectWidgets {
			t.Errorf("resolveProjectRef = %q, want the ULID unchanged", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID ref made %d HTTP calls, want 0", n)
		}
	})

	t.Run("name is resolved server-side in one call", func(t *testing.T) {
		t.Parallel()
		var gotName string
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotName = r.URL.Query().Get("name")
			if err := printJSON(w, &coreapi.ListProjectsOutputBody{Project: matched}); err != nil {
				t.Errorf("encode project: %v", err)
			}
		})
		got, err := resolveProjectRef(context.Background(), c, "widgets")
		if err != nil {
			t.Fatalf("resolveProjectRef: %v", err)
		}
		if got != ulidProjectWidgets {
			t.Errorf("resolveProjectRef = %q, want widgets id", got)
		}
		if gotName != "widgets" {
			t.Errorf("server received name=%q, want %q (filtering must be server-side)", gotName, "widgets")
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("name ref made %d HTTP calls, want 1", n)
		}
	})

	t.Run("unknown name is a friendly error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ListProjectsOutputBody{}); err != nil {
				t.Errorf("encode empty: %v", err)
			}
		})
		_, err := resolveProjectRef(context.Background(), c, "nope")
		if err == nil || !strings.Contains(err.Error(), "no project named") {
			t.Errorf("resolveProjectRef unknown name: err = %v, want a \"no project named\" error", err)
		}
	})
}

func TestResolveRepoRef(t *testing.T) {
	t.Parallel()
	const ulidRepoWeb = "0123456789ABCDEFGHJKMNPQR5"

	t.Run("ULID passes through without a network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		got, err := resolveRepoRef(context.Background(), c, ulidRepoWeb, "")
		if err != nil {
			t.Fatalf("resolveRepoRef: %v", err)
		}
		if got != ulidRepoWeb {
			t.Errorf("resolveRepoRef = %q, want the ULID unchanged", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID ref made %d HTTP calls, want 0", n)
		}
	})

	t.Run("name without --project is rejected before any call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call when project scope is missing")
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := resolveRepoRef(context.Background(), c, "web", "")
		if err == nil || !strings.Contains(err.Error(), "pass --project") {
			t.Errorf("resolveRepoRef without project: err = %v, want a \"pass --project\" error", err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("missing-scope made %d HTTP calls, want 0", n)
		}
	})

	t.Run("name is resolved server-side, scoped to the project", func(t *testing.T) {
		t.Parallel()
		var gotName string
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotName = r.URL.Query().Get("name")
			// A name-filtered list returns the single match under the singular
			// `repo` field (like org/project) — NOT the plural `repos` array,
			// which is only populated for an unfiltered page. Reading `repos`
			// here was the COR-699 bug, so the fixture must mirror the real
			// server's singular field to keep that regression covered.
			if err := printJSON(w, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: ulidRepoWeb, Name: "web"})}); err != nil {
				t.Errorf("encode repo: %v", err)
			}
		})
		// Project passed as a ULID so resolveProjectRef short-circuits (no call);
		// only the repo by-name lookup hits the server — one O(1) call.
		got, err := resolveRepoRef(context.Background(), c, "web", ulidProjectWidgets)
		if err != nil {
			t.Fatalf("resolveRepoRef: %v", err)
		}
		if got != ulidRepoWeb {
			t.Errorf("resolveRepoRef = %q, want web id", got)
		}
		if gotName != "web" {
			t.Errorf("server received name=%q, want %q (filtering must be server-side)", gotName, "web")
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("name ref made %d HTTP calls, want 1", n)
		}
	})

	t.Run("unknown name is a friendly error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ListProjectReposOutputBody{}); err != nil {
				t.Errorf("encode empty: %v", err)
			}
		})
		_, err := resolveRepoRef(context.Background(), c, "nope", ulidProjectWidgets)
		if err == nil || !strings.Contains(err.Error(), "no repo named") {
			t.Errorf("resolveRepoRef unknown name: err = %v, want a \"no repo named\" error", err)
		}
	})

	t.Run("name under --project <name> resolves like a path, no project lookup", func(t *testing.T) {
		t.Parallel()
		// `repo view web --project widgets`: a repo-only grantee holds repo#pull
		// but not project#inspect, so the name pair must go through
		// repos/resolve. nativePathHandler refuses any /projects call.
		var gotFullName string
		c, calls := resolveTestClient(t, nativePathHandler(t, &gotFullName))
		got, err := resolveRepoRef(context.Background(), c, "web", "widgets")
		if err != nil {
			t.Fatalf("resolveRepoRef: %v", err)
		}
		if got != ulidRepoWeb {
			t.Errorf("resolveRepoRef = %q, want web id", got)
		}
		if gotFullName != "widgets/web" {
			t.Errorf("server received fullName=%q, want %q", gotFullName, "widgets/web")
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("name under project name made %d HTTP calls, want 1 (repos/resolve)", n)
		}
	})

	t.Run("unknown name under --project <name> is one friendly miss", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolveReposResponse{Resolutions: []coreapi.RepoResolution{{
				Provider: repoProviderEntire, RequestedFullName: "widgets/nope", Status: coreapi.RepoResolutionStatusUnavailable,
			}}}); err != nil {
				t.Errorf("encode resolution: %v", err)
			}
		})
		_, err := resolveRepoRef(context.Background(), c, "nope", "widgets")
		require.ErrorIs(t, err, errNamedRefNotFound)
		require.EqualError(t, err, "repo /et/widgets/nope not found or not shared with you")
	})
}

// TestResolveRepoInProject_GitSuffixMissCarriesHint pins that a miss on a name
// ending in .git says so, and that the hint did not cost the error its
// classification -- repository routing distinguishes a definitive lookup miss
// from a transport failure through errNamedRefNotFound.
func TestResolveRepoInProject_GitSuffixMissCarriesHint(t *testing.T) {
	t.Parallel()
	c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := printJSON(w, &coreapi.ListProjectReposOutputBody{}); err != nil {
			t.Errorf("encode empty: %v", err)
		}
	})
	// Every case of the suffix earns the hint. A user who typed ".GIT" has
	// exactly the misconception the hint exists to correct, and used to be
	// the one person it stayed silent for.
	for _, name := range []string{"web.git", "web.GIT", "web.Git", "web.gIt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveRepoRef(context.Background(), c, name, ulidProjectWidgets)
			require.Error(t, err)
			require.ErrorIs(t, err, errNamedRefNotFound)
			require.Contains(t, err.Error(), `no repo named "`+name+`"`)
			require.Contains(t, err.Error(), "drop the suffix")
			require.Contains(t, err.Error(), `"web"`)
		})
	}
}

// TestResolveRepoInProject_PlainMissHasNoHint pins that the hint is scoped to
// the case it explains.
func TestResolveRepoInProject_PlainMissHasNoHint(t *testing.T) {
	t.Parallel()
	c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := printJSON(w, &coreapi.ListProjectReposOutputBody{}); err != nil {
			t.Errorf("encode empty: %v", err)
		}
	})
	_, err := resolveRepoRef(context.Background(), c, "web", ulidProjectWidgets)
	require.Error(t, err)
	require.ErrorIs(t, err, errNamedRefNotFound)
	require.NotContains(t, err.Error(), "drop the suffix")
}

// TestResolveRepoRef_NativePath covers the /et/<project>/<repo> path grammar
// (COR-1632): the path the API returns and `repo clone` accepts resolves in
// every repo-ref command, --project alongside it is checked for agreement, and
// the #2252 rule holds at the resolver — no ref is read as a forge it did not
// name, and no slash-bearing ref reaches the by-name lookup.
// nativePathHandler serves the one lookup a /et/widgets/web ref makes, POST
// /repos/resolve, plus GET /repos/{id} for the --project ULID agreement check.
// It records the full name the server was asked to resolve, so tests can pin
// server-side matching and the .git trim. Any /projects call is refused: a
// native ref must resolve with repo#pull alone.
func nativePathHandler(t *testing.T, gotFullName *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repos/resolve"):
			var in coreapi.ResolveReposInputBody
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode resolve body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(in.Repositories) != 1 || in.Repositories[0].Provider != repoProviderEntire {
				t.Errorf("resolve body = %+v, want one entire reference", in.Repositories)
			}
			if len(in.Repositories) > 0 {
				*gotFullName = in.Repositories[0].FullName
			}
			// The CLI matches on the echoed requested name.
			if err := printJSON(w, nativeResolution(*gotFullName, ulidRepoWeb)); err != nil {
				t.Errorf("encode resolution: %v", err)
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/"+ulidRepoWeb):
			if err := printJSON(w, &coreapi.Repo{ID: ulidRepoWeb, Name: "web", OwningProjectId: ulidProjectWidgets}); err != nil {
				t.Errorf("encode repo: %v", err)
			}
		default:
			t.Errorf("unexpected %s %s: a native ref must not need a project lookup", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}
}

// TestResolveRepoRef_NativePathDropsGitSuffix pins that a trailing `.git` on a
// native ref is an alias, not a name: it never reaches the lookup. The handler
// answers any name with the "web" row; what matters is the name the server was
// asked for.
func TestResolveRepoRef_NativePathDropsGitSuffix(t *testing.T) {
	t.Parallel()
	var gotFullName string
	c, _ := resolveTestClient(t, nativePathHandler(t, &gotFullName))
	if _, err := resolveRepoRef(context.Background(), c, "/et/widgets/web.git", ""); err != nil {
		t.Fatalf("resolveRepoRef: %v", err)
	}
	if gotFullName != "widgets/web" {
		t.Errorf("server received fullName=%q, want %q", gotFullName, "widgets/web")
	}
}

// TestResolveRepoRef_NativePathEchoesOnlyTheServersName pins that the echoed
// identifier comes from the server's FullName and from nowhere else.
//
// The loop matches on RequestedFullName, so a resolution whose FullName names a
// different repo is still accepted; falling back to the ref this resolver
// composed would render the user's own spelling as a canonical /et/ path the
// server never confirmed. Every other fixture writes one name into both fields,
// so these two are the only cases that can tell the sources apart.
func TestResolveRepoRef_NativePathEchoesOnlyTheServersName(t *testing.T) {
	t.Parallel()
	// The typed ref carries the `.git` alias; the resolver drops it, so the
	// requested name below is the suffix-free spelling.
	const ref = "/et/audit1/victim.git"

	// resolutionHandler answers the one POST /repos/resolve a native path ref
	// makes, with the requested name fixed and the echoed name under test.
	resolutionHandler := func(fullName coreapi.OptString) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/repos/resolve") {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if err := printJSON(w, &coreapi.ResolveReposResponse{Resolutions: []coreapi.RepoResolution{{
				Provider:          repoProviderEntire,
				RequestedFullName: "audit1/victim",
				FullName:          fullName,
				Status:            coreapi.RepoResolutionStatusReady,
				RepoId:            coreapi.NewOptString(ulidRepoWeb),
			}}}); err != nil {
				t.Errorf("encode resolution: %v", err)
			}
		}
	}

	t.Run("a differing server name is the one echoed", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, resolutionHandler(coreapi.NewOptString("audit1/renamed")))
		got, err := resolveRepoRefResolved(context.Background(), c, ref, "")
		require.NoError(t, err)
		require.Equal(t, ulidRepoWeb, got.ID)
		require.Equal(t, "/et/audit1/renamed", got.Name, "the label must carry the name the server matched")
		require.Equal(t, "/et/audit1/renamed ("+ulidRepoWeb+")", resolvedRefLabel(ref, got))
	})

	t.Run("no server name leaves the label to the typed ref", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, resolutionHandler(coreapi.OptString{}))
		got, err := resolveRepoRefResolved(context.Background(), c, ref, "")
		require.NoError(t, err)
		require.Equal(t, ulidRepoWeb, got.ID)
		require.Empty(t, got.Name, "an unconfirmed name must not be manufactured from the ref")
		require.Equal(t, ref+" ("+ulidRepoWeb+")", resolvedRefLabel(ref, got),
			"the user's spelling is reported as the user's, not as a canonical path")
	})
}

func TestResolveRepoRef_NativePath(t *testing.T) {
	t.Parallel()
	t.Run("native /et/ path resolves in one pull-gated call", func(t *testing.T) {
		t.Parallel()
		for _, ref := range []string{"/et/widgets/web", "et/widgets/web"} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				var gotFullName string
				c, calls := resolveTestClient(t, nativePathHandler(t, &gotFullName))
				got, err := resolveRepoRef(context.Background(), c, ref, "")
				if err != nil {
					t.Fatalf("resolveRepoRef(%q): %v", ref, err)
				}
				if got != ulidRepoWeb {
					t.Errorf("resolveRepoRef = %q, want web id", got)
				}
				if gotFullName != "widgets/web" {
					t.Errorf("server received fullName=%q, want %q", gotFullName, "widgets/web")
				}
				if n := calls.Load(); n != 1 {
					t.Errorf("path ref made %d HTTP calls, want 1 (repos/resolve)", n)
				}
			})
		}
	})

	t.Run("a repo the server does not resolve is one friendly miss", func(t *testing.T) {
		t.Parallel()
		// Unknown and unshared repos share the server's "unavailable" answer.
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolveReposResponse{Resolutions: []coreapi.RepoResolution{{
				Provider: repoProviderEntire, RequestedFullName: "widgets/web", Status: coreapi.RepoResolutionStatusUnavailable,
			}}}); err != nil {
				t.Errorf("encode resolution: %v", err)
			}
		})
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", "")
		require.ErrorIs(t, err, errNamedRefNotFound)
		require.EqualError(t, err, "repo /et/widgets/web not found or not shared with you")
	})

	t.Run("--project agreeing with the path is allowed", func(t *testing.T) {
		t.Parallel()
		// A name compares locally; a ULID costs one GetRepo.
		for project, wantCalls := range map[string]int64{"widgets": 1, "WIDGETS": 1, ulidProjectWidgets: 2} {
			t.Run(project, func(t *testing.T) {
				t.Parallel()
				var gotFullName string
				c, calls := resolveTestClient(t, nativePathHandler(t, &gotFullName))
				got, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", project)
				if err != nil {
					t.Fatalf("resolveRepoRef with --project %q: %v", project, err)
				}
				if got != ulidRepoWeb {
					t.Errorf("resolveRepoRef = %q, want web id", got)
				}
				if n := calls.Load(); n != wantCalls {
					t.Errorf("--project %q made %d HTTP calls, want %d", project, n, wantCalls)
				}
			})
		}
	})

	t.Run("--project name disagreeing with the path is rejected before any call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a mismatched --project name")
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", "gadgets")
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("mismatched --project: err = %v, want a \"does not match\" error", err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("mismatched --project name made %d HTTP calls, want 0", n)
		}
	})

	t.Run("--project ULID disagreeing with the path is rejected", func(t *testing.T) {
		t.Parallel()
		var gotFullName string
		c, _ := resolveTestClient(t, nativePathHandler(t, &gotFullName))
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", ulidOrgGlobex)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("mismatched --project ULID: err = %v, want a \"does not match\" error", err)
		}
	})

	// The remaining subtests pin the #2252 rule at the resolver: no ref is ever
	// read as a forge it did not name, and no slash-bearing ref reaches the
	// by-name lookup.
	refuseLocally := func(t *testing.T, ref, wantErr string) {
		t.Helper()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("unexpected HTTP call for ref %q", ref)
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := resolveRepoRef(context.Background(), c, ref, "")
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("resolveRepoRef(%q): err = %v, want it to contain %q", ref, err, wantErr)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ref %q made %d HTTP calls, want 0", ref, n)
		}
	}

	t.Run("a /gh/ mirror ref is refused, never read as a name", func(t *testing.T) {
		t.Parallel()
		refuseLocally(t, "/gh/entirehq/entire-api", "entire repo mirror")
	})

	// The message describes the REF, not the command. Several commands sharing
	// this resolver do address mirror repos by ULID — `repo protection list`
	// answers one with protectionMirrorNote — so claiming the command is
	// native-only was false, and `entire repo mirror` has no visibility or
	// protection counterpart to send those callers to. Naming the ULID is the
	// part that is true everywhere and actually unblocks the user.
	t.Run("the mirror refusal names the ULID as the way through", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := resolveRepoRef(context.Background(), c, "/gh/entirehq/entire-api", "")
		if err == nil {
			t.Fatal("a /gh/ ref must be refused")
		}
		if !strings.Contains(err.Error(), "ULID") {
			t.Errorf("mirror refusal = %q, want it to offer the ULID", err)
		}
		if strings.Contains(err.Error(), "addresses Entire-native repos") {
			t.Errorf("mirror refusal must not claim the command is native-only: %q", err)
		}
	})

	// Following the suggestion with the flag still set would fail the agreement
	// check on the very next run, so the suggestion has to mention it.
	t.Run("the forge suggestion says to drop a --project that would then clash", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("a forge-less pair must not reach the control plane")
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := resolveRepoRef(context.Background(), c, "acme/tool", "widgets")
		if err == nil {
			t.Fatal("a forge-less pair must be refused")
		}
		for _, want := range []string{"/et/acme/tool", "--project", "widgets"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("suggestion = %q, want it to contain %q", err, want)
			}
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("made %d HTTP calls, want 0", n)
		}
	})

	t.Run("a bare pair names no forge and is refused with the /et/ suggestion", func(t *testing.T) {
		t.Parallel()
		refuseLocally(t, "widgets/web", "/et/widgets/web")
	})

	t.Run("a malformed /et/ ref keeps the native parser's reason", func(t *testing.T) {
		t.Parallel()
		refuseLocally(t, "/et/widgets", "<repo>")
	})

	t.Run("a slash-bearing ref matching no grammar lists the accepted shapes", func(t *testing.T) {
		t.Parallel()
		// "/web" pins that dispatch reads the ref as given: trimming the leading
		// slash first would send it to the by-name lookup slash and all — a
		// guaranteed 404, since names can never contain '/'.
		for _, ref := range []string{"a/b/c/d", "/web", "web/"} {
			refuseLocally(t, ref, "/et/<project>/<repo>")
		}
	})
}

// TestResolveRepoPath covers the one repo spelling `repo grant` accepts, the
// native /et/<project>/<repo> path: it resolves through the same path lookup
// as the path does elsewhere, and everything else — a ULID, a bare name, a
// bare pair, a /gh/ mirror ref — is refused locally with the shape named,
// before any request is made.
func TestResolveRepoPath(t *testing.T) {
	t.Parallel()

	t.Run("a native path resolves in one call", func(t *testing.T) {
		t.Parallel()
		for _, ref := range []string{"/et/widgets/web", "et/widgets/web", "/et/widgets/web.git"} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				var gotFullName string
				c, calls := resolveTestClient(t, nativePathHandler(t, &gotFullName))
				got, err := resolveRepoPath(context.Background(), c, ref)
				require.NoError(t, err)
				require.Equal(t, ulidRepoWeb, got)
				require.Equal(t, "widgets/web", gotFullName)
				require.EqualValues(t, 1, calls.Load(), "repos/resolve")
			})
		}
	})

	t.Run("a ULID-shaped segment is still a name", func(t *testing.T) {
		t.Parallel()
		// The server's name rules admit 26 base32 characters, so a project or
		// repo can be NAMED like a ULID. Inside a path both segments are names
		// by construction and must go to the path lookup, never the ULID
		// passthrough that a bare ref gets.
		for _, ref := range []string{"/et/widgets/" + ulidAccount, "/et/" + ulidAccount + "/web"} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				var gotFullName string
				c, calls := resolveTestClient(t, nativePathHandler(t, &gotFullName))
				got, err := resolveRepoPath(context.Background(), c, ref)
				require.NoError(t, err)
				require.Equal(t, ulidRepoWeb, got)
				require.Equal(t, strings.TrimPrefix(ref, "/et/"), gotFullName)
				require.EqualValues(t, 1, calls.Load(), "repos/resolve")
			})
		}
	})

	t.Run("anything but the native path is refused without a request", func(t *testing.T) {
		t.Parallel()
		// A ref that never named the et/ token gets the accepted shape and
		// nothing else: the parser's "not a native ref" reason is its signal to
		// try another grammar, not a message for the user.
		for _, ref := range []string{ulidRepoWeb, "web", "widgets/web", "/gh/acme/tool"} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				})
				_, err := resolveRepoPath(context.Background(), c, ref)
				require.EqualError(t, err, "repo "+strconv.Quote(ref)+" must be a /et/<project>/<repo> path")
				require.Zero(t, calls.Load())
			})
		}
	})

	t.Run("a malformed native path keeps the parser's reason", func(t *testing.T) {
		t.Parallel()
		// The ref named et/ and got the rest wrong, so the parser knows which
		// part: that reason already carries the shape or the offending name,
		// and only the ref itself is added in front of it.
		for ref, reason := range map[string]string{
			"/et/widgets":       "2 names after the et token, got 1",
			"/et/widgets/-bad-": `repo "-bad-" is not a name the server accepts`,
		} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				})
				_, err := resolveRepoPath(context.Background(), c, ref)
				require.ErrorContains(t, err, "invalid repo ref "+strconv.Quote(ref)+": ")
				require.ErrorContains(t, err, reason)
				require.NotContains(t, err.Error(), "must be a")
				require.Zero(t, calls.Load())
			})
		}
	})
}

// TestResolveRepoRefResolved_ReportsServerPath pins that the resolved-ref entry
// point hands back the server's own identifier. The path is preferred over the
// bare name: "victim.git" alone would not show which project the repo was in.
func TestResolveRepoRefResolved_ReportsServerPath(t *testing.T) {
	t.Parallel()
	var gotFullName string
	c, _ := resolveTestClient(t, nativePathHandler(t, &gotFullName))
	got, err := resolveRepoRefResolved(context.Background(), c, "/et/widgets/web", "")
	require.NoError(t, err)
	require.Equal(t, ulidRepoWeb, got.ID)
	require.Equal(t, "/et/widgets/web", got.Name)
}

// TestResolveRepoRefResolved_FallsBackToName pins the project-scoped lookup,
// which answers with a repo row rather than a resolution: path is an optional
// field there, so a response without one still names the repo.
func TestResolveRepoRefResolved_FallsBackToName(t *testing.T) {
	t.Parallel()
	c, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos") {
			t.Errorf("unexpected %s %s: a --project ULID needs only the repo listing", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := printJSON(w, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
			ID:   ulidRepoWeb,
			Name: "web",
		})}); err != nil {
			t.Errorf("encode repo: %v", err)
		}
	})
	got, err := resolveRepoRefResolved(context.Background(), c, "web", ulidProjectWidgets)
	require.NoError(t, err)
	require.Equal(t, ulidRepoWeb, got.ID)
	require.Equal(t, "web", got.Name)
}

// TestResolveRepoRefResolved_ULIDHasNoName pins that a ULID ref costs no extra
// round trip: no lookup happened, so there is no name to report.
func TestResolveRepoRefResolved_ULIDHasNoName(t *testing.T) {
	t.Parallel()
	c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("ULID ref must not hit the server")
		w.WriteHeader(http.StatusInternalServerError)
	})
	got, err := resolveRepoRefResolved(context.Background(), c, ulidRepoWeb, "")
	require.NoError(t, err)
	require.Equal(t, ulidRepoWeb, got.ID)
	require.Empty(t, got.Name)
	require.EqualValues(t, 0, calls.Load())
}

func TestResolveAccountRef(t *testing.T) {
	t.Parallel()

	t.Run("ULID passes through without a network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		got, err := resolveAccountRef(context.Background(), c, ulidAccount)
		if err != nil {
			t.Fatalf("resolveAccountRef: %v", err)
		}
		if got != ulidAccount {
			t.Errorf("resolveAccountRef = %q, want the ULID unchanged", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID ref made %d HTTP calls, want 0", n)
		}
	})

	t.Run("handle resolves via exactly one call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: ulidResolvedAcct, Provider: "github", Handle: "alice"}); err != nil {
				t.Errorf("encode identity: %v", err)
			}
		})
		got, err := resolveAccountRef(context.Background(), c, "github:alice")
		if err != nil {
			t.Fatalf("resolveAccountRef: %v", err)
		}
		if got != ulidResolvedAcct {
			t.Errorf("resolveAccountRef = %q, want resolved account id", got)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("handle ref made %d HTTP calls, want 1", n)
		}
	})

	t.Run("empty resolved account id is an error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: "", Provider: "github", Handle: "alice"}); err != nil {
				t.Errorf("encode identity: %v", err)
			}
		})
		if _, err := resolveAccountRef(context.Background(), c, "github:alice"); err == nil {
			t.Error("resolveAccountRef expected error for empty account id")
		}
	})

	// `project create --owner` takes the same spellings a grantee does.
	for _, ref := range []string{"google:1001", "google:google-1001"} {
		t.Run("google owner "+ref+" resolves the minted handle", func(t *testing.T) {
			t.Parallel()
			c, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/google/google-1001") {
					t.Errorf("resolved path %q, want the minted handle google-1001", r.URL.Path)
				}
				if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: ulidResolvedAcct, Provider: providerGoogle, Handle: "google-1001", ProviderUserId: "1001"}); err != nil {
					t.Errorf("encode identity: %v", err)
				}
			})
			got, err := resolveAccountRef(context.Background(), c, ref)
			if err != nil {
				t.Fatalf("resolveAccountRef: %v", err)
			}
			if got != ulidResolvedAcct {
				t.Errorf("resolveAccountRef = %q, want %q", got, ulidResolvedAcct)
			}
		})
	}

	t.Run("non-qualified handle fails before any network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for an invalid handle")
			w.WriteHeader(http.StatusInternalServerError)
		})
		if _, err := resolveAccountRef(context.Background(), c, "alice"); err == nil {
			t.Error("resolveAccountRef expected error for non-qualified handle")
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("invalid handle made %d HTTP calls, want 0", n)
		}
	})
}

func TestResolveGranteeProvider(t *testing.T) {
	t.Parallel()

	t.Run("handle resolves to the provider user id in one call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: ulidResolvedAcct, Provider: providerGitHub, Handle: "alice", ProviderUserId: "12345"}); err != nil {
				t.Errorf("encode identity: %v", err)
			}
		})
		provider, puid, err := resolveGranteeProvider(context.Background(), c, "github:alice")
		if err != nil {
			t.Fatalf("resolveGranteeProvider: %v", err)
		}
		if provider != providerGitHub || puid != "12345" {
			t.Errorf("resolveGranteeProvider = (%q, %q), want (github, 12345)", provider, puid)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("handle ref made %d HTTP calls, want 1", n)
		}
	})

	// `auth status` shows a Google login as google:<subject id>; the server
	// only resolves its minted google-<subject id>, so both spellings must look
	// up the minted one.
	for _, ref := range []string{"google:1001", "google:google-1001"} {
		t.Run("google grantee "+ref+" resolves the minted handle", func(t *testing.T) {
			t.Parallel()
			c, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/google/google-1001") {
					t.Errorf("resolved path %q, want the minted handle google-1001", r.URL.Path)
				}
				if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: ulidResolvedAcct, Provider: providerGoogle, Handle: "google-1001", ProviderUserId: "1001"}); err != nil {
					t.Errorf("encode identity: %v", err)
				}
			})
			provider, puid, err := resolveGranteeProvider(context.Background(), c, ref)
			if err != nil {
				t.Fatalf("resolveGranteeProvider: %v", err)
			}
			if provider != providerGoogle || puid != "1001" {
				t.Errorf("resolveGranteeProvider = (%q, %q), want (google, 1001)", provider, puid)
			}
		})
	}

	t.Run("non-qualified handle fails before any network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for an invalid handle")
			w.WriteHeader(http.StatusInternalServerError)
		})
		if _, _, err := resolveGranteeProvider(context.Background(), c, "alice"); err == nil {
			t.Error("resolveGranteeProvider expected error for non-qualified handle")
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("invalid handle made %d HTTP calls, want 0", n)
		}
	})

	t.Run("account ULID is rejected before any network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID grantee")
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, _, err := resolveGranteeProvider(context.Background(), c, wiringGranteeULID)
		if err == nil {
			t.Fatal("resolveGranteeProvider expected error for a ULID grantee")
		}
		if !strings.Contains(err.Error(), "provider-qualified handle") {
			t.Errorf("error %q should point at the provider-qualified handle form", err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID grantee made %d HTTP calls, want 0", n)
		}
	})

	t.Run("empty provider user id is an error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolvedIdentity{AccountId: ulidResolvedAcct, Provider: providerGitHub, Handle: "alice", ProviderUserId: ""}); err != nil {
				t.Errorf("encode identity: %v", err)
			}
		})
		if _, _, err := resolveGranteeProvider(context.Background(), c, "github:alice"); err == nil {
			t.Error("resolveGranteeProvider expected error for empty provider user id")
		}
	})
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

// formatQualifiedHandle is the inverse of parseQualifiedHandle; they live
// together so the grantee spelling cannot drift between what the CLI prints and
// what it accepts.
func TestFormatQualifiedHandle_RoundTripsThroughTheParser(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ provider, handle string }{
		{"github", "alice"},
		{"github", "alice:bob"}, // only the first colon splits, so this survives
		{"gitlab", "a"},
	} {
		t.Run(tt.provider+":"+tt.handle, func(t *testing.T) {
			t.Parallel()
			provider, handle, err := parseQualifiedHandle(formatQualifiedHandle(tt.provider, tt.handle))
			if err != nil {
				t.Fatalf("parseQualifiedHandle(formatQualifiedHandle(%q, %q)): %v", tt.provider, tt.handle, err)
			}
			if provider != tt.provider || handle != tt.handle {
				t.Errorf("round trip = (%q, %q), want (%q, %q)", provider, handle, tt.provider, tt.handle)
			}
		})
	}
}

// An empty provider yields the bare handle: ":alice" parses as nothing and
// would be a grantee string no command accepts.
func TestFormatQualifiedHandle_OmitsAnEmptyProvider(t *testing.T) {
	t.Parallel()
	if got := formatQualifiedHandle("", "alice"); got != "alice" {
		t.Errorf("formatQualifiedHandle(\"\", \"alice\") = %q, want %q", got, "alice")
	}
}

func TestToProjectList(t *testing.T) {
	t.Parallel()

	t.Run("set project yields one element", func(t *testing.T) {
		t.Parallel()
		got := toProjectList(coreapi.NewOptProject(coreapi.Project{ID: ulidProjectWidgets, Name: "widgets"}))
		if len(got) != 1 || got[0].ID != ulidProjectWidgets {
			t.Errorf("toProjectList = %+v, want one widgets project", got)
		}
	})

	t.Run("unset project yields empty", func(t *testing.T) {
		t.Parallel()
		if got := toProjectList(coreapi.OptProject{}); len(got) != 0 {
			t.Errorf("toProjectList(unset) = %+v, want empty", got)
		}
	})
}
