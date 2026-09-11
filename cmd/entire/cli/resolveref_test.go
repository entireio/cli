package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

// Valid ULID-shaped fixtures (26 Crockford base32 chars, no I/L/O/U) so the
// resolver tests exercise the ULID short-circuit instead of a name lookup.
const (
	ulidOrgAcme        = "0123456789ABCDEFGHJKMNPQR1"
	ulidOrgGlobex      = "0123456789ABCDEFGHJKMNPQR2"
	ulidProjectWidgets = "0123456789ABCDEFGHJKMNPQR3"
	ulidAccount        = "0123456789ABCDEFGHJKMNPQR4"
	ulidResolvedAcct   = "0123456789ABCDEFGHJKMNPQR9"
)

// resolveTestClient builds a coreapi client pointed at a test server whose
// handler is h, and returns the client plus a counter of HTTP requests seen.
// It lets the resolver tests assert the load-bearing invariant from
// resolveref.go's doc comment: a ULID ref makes zero network calls, a name ref
// makes exactly one.
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

	t.Run("name is resolved server-side in one call", func(t *testing.T) {
		t.Parallel()
		var gotName string
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotName = r.URL.Query().Get("name")
			if err := printJSON(w, &coreapi.ListOrgsOutputBody{Org: coreapi.NewOptOrg(coreapi.Org{ID: ulidOrgGlobex, Name: "globex"})}); err != nil {
				t.Errorf("encode org: %v", err)
			}
		})
		got, err := resolveOrgRef(context.Background(), c, "globex")
		if err != nil {
			t.Fatalf("resolveOrgRef: %v", err)
		}
		if got != ulidOrgGlobex {
			t.Errorf("resolveOrgRef = %q, want globex id", got)
		}
		if gotName != "globex" {
			t.Errorf("server received name=%q, want %q (filtering must be server-side)", gotName, "globex")
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("name ref made %d HTTP calls, want 1", n)
		}
	})

	t.Run("unknown name is a friendly error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ListOrgsOutputBody{}); err != nil {
				t.Errorf("encode empty: %v", err)
			}
		})
		_, err := resolveOrgRef(context.Background(), c, "nope")
		if err == nil || !strings.Contains(err.Error(), "no org named") {
			t.Errorf("resolveOrgRef unknown name: err = %v, want a \"no org named\" error", err)
		}
	})
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
}

// TestResolveRepoRef_NativePath covers the /et/<project>/<repo> path grammar
// (COR-1632): the path the API returns and `repo clone` accepts resolves in
// every repo-ref command, --project alongside it is checked for agreement, and
// the #2252 rule holds at the resolver — no ref is read as a forge it did not
// name, and no slash-bearing ref reaches the by-name lookup.
func TestResolveRepoRef_NativePath(t *testing.T) {
	t.Parallel()
	const ulidRepoWeb = "0123456789ABCDEFGHJKMNPQR5"

	// nativePathHandler serves the two lookups a /et/widgets/web ref makes:
	// GET /projects?name=widgets and GET /projects/{id}/repos?name=web. It
	// also records the repo name the server was asked for, so tests can pin
	// server-side filtering and the .git trim.
	nativePathHandler := func(t *testing.T, gotRepoName *string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/repos") {
				*gotRepoName = r.URL.Query().Get("name")
				if err := printJSON(w, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: ulidRepoWeb, Name: "web"})}); err != nil {
					t.Errorf("encode repo: %v", err)
				}
				return
			}
			if err := printJSON(w, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: ulidProjectWidgets, Name: "widgets", OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg})}); err != nil {
				t.Errorf("encode project: %v", err)
			}
		}
	}

	t.Run("native /et/ path resolves via its embedded project", func(t *testing.T) {
		t.Parallel()
		for _, ref := range []string{"/et/widgets/web", "et/widgets/web", "/et/widgets/web.git"} {
			t.Run(ref, func(t *testing.T) {
				t.Parallel()
				var gotRepoName string
				c, calls := resolveTestClient(t, nativePathHandler(t, &gotRepoName))
				got, err := resolveRepoRef(context.Background(), c, ref, "")
				if err != nil {
					t.Fatalf("resolveRepoRef(%q): %v", ref, err)
				}
				if got != ulidRepoWeb {
					t.Errorf("resolveRepoRef = %q, want web id", got)
				}
				if gotRepoName != "web" {
					t.Errorf("server received repo name=%q, want %q", gotRepoName, "web")
				}
				if n := calls.Load(); n != 2 {
					t.Errorf("path ref made %d HTTP calls, want 2 (project + repo lookup)", n)
				}
			})
		}
	})

	t.Run("--project agreeing with the path is allowed", func(t *testing.T) {
		t.Parallel()
		// A name compares case-insensitively (the server matches lower(name));
		// a ULID compares against the resolved project id.
		for _, project := range []string{"widgets", "WIDGETS", ulidProjectWidgets} {
			t.Run(project, func(t *testing.T) {
				t.Parallel()
				var gotRepoName string
				c, _ := resolveTestClient(t, nativePathHandler(t, &gotRepoName))
				got, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", project)
				if err != nil {
					t.Fatalf("resolveRepoRef with --project %q: %v", project, err)
				}
				if got != ulidRepoWeb {
					t.Errorf("resolveRepoRef = %q, want web id", got)
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
		var gotRepoName string
		c, _ := resolveTestClient(t, nativePathHandler(t, &gotRepoName))
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", ulidOrgGlobex)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("mismatched --project ULID: err = %v, want a \"does not match\" error", err)
		}
		if gotRepoName != "" {
			t.Error("repo lookup must not run when --project disagrees with the path")
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

func TestResolvedRefLabel(t *testing.T) {
	t.Parallel()

	const id = "01J0REPO000000000000000001"

	t.Run("ulid passes through", func(t *testing.T) {
		t.Parallel()
		if got := resolvedRefLabel(id, id); got != id {
			t.Errorf("got %q, want %q", got, id)
		}
	})

	t.Run("name includes resolved id", func(t *testing.T) {
		t.Parallel()
		want := fmt.Sprintf("acme (%s)", id)
		if got := resolvedRefLabel("acme", id); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
