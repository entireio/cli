package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/internal/coreapi"
)

func TestParseMirrorCloneRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "leading slash", ref: "/gh/entirehq/entire-api", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "no leading slash", ref: "gh/entirehq/entire-api", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "lowercased", ref: "/gh/EntireHQ/Entire-API", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "wrong provider", ref: "/gl/entirehq/entire-api", wantErr: true},
		{name: "missing repo", ref: "/gh/entirehq", wantErr: true},
		{name: "extra segment", ref: "/gh/entirehq/entire-api/extra", wantErr: true},
		{name: "dot-only repo", ref: "/gh/entirehq/..", wantErr: true},
		// GitHub cannot hold a name ending in .git, so here the suffix is only
		// ever decoration. Contrast the native table below.
		{name: "git suffix is dropped", ref: "/gh/entirehq/entire-api.git", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "git suffix dropped from a dotted name", ref: "/gh/entirehq/trails.el.git", wantOwner: "entirehq", wantRepo: "trails.el"},
		// `..git` is not dot-only as typed; it becomes so once the suffix goes,
		// which is why the trim has to run first.
		{name: "dot-only once the suffix is dropped", ref: "/gh/entirehq/..git", wantErr: true},
		{name: "git suffix alone leaves no name", ref: "/gh/entirehq/.git", wantErr: true},
		{name: "metachar in repo", ref: "/gh/entirehq/repo?x=1", wantErr: true},
		{name: "empty", ref: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider, owner, repo, err := parseMirrorCloneRef(tt.ref)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "github", provider)
			require.Equal(t, tt.wantOwner, owner)
			require.Equal(t, tt.wantRepo, repo)
		})
	}
}

// TestParseNativeCloneRef pins two things: that the `et/` token is required —
// the bare `<project>/<repo>` shorthand was removed because nothing in it says
// which forge was meant (#2252) — and that the names are checked against the
// server's own rules (entiredb core/resource/project_name.go), so a ref the
// control plane could never match costs a shape error rather than a round trip.
func TestParseNativeCloneRef(t *testing.T) {
	t.Parallel()
	// Bounds are the server's: projects 3-32 chars, repos 1-64.
	maxProject := "a" + strings.Repeat("b", 30) + "c"
	maxRepo := "a" + strings.Repeat("b", 62) + "c"
	tests := []struct {
		name        string
		ref         string
		wantProject string
		wantRepo    string
		wantErr     bool
	}{
		{name: "full et ref", ref: "/et/paul/dogbark", wantProject: "paul", wantRepo: "dogbark"},
		{name: "no leading slash", ref: "et/paul/dogbark", wantProject: "paul", wantRepo: "dogbark"},
		{name: "uppercase folds server-side", ref: "/et/Paul/DogBark", wantProject: "Paul", wantRepo: "DogBark"},
		{name: "dotted repo", ref: "/et/paul/entire-trails.el", wantProject: "paul", wantRepo: "entire-trails.el"},
		// `.git` is part of a native repo name: entiredb permits an interior
		// dot and the data plane resolves /et/ paths verbatim, so a repo can be
		// named "dogbark.git" and trimming names a different one. Contrast the
		// /gh/ table above.
		{name: "git suffix is part of the name", ref: "/et/paul/dogbark.git", wantProject: "paul", wantRepo: "dogbark.git"},
		{name: "git suffix on a dotted name", ref: "/et/paul/entire-trails.el.git", wantProject: "paul", wantRepo: "entire-trails.el.git"},
		{name: "a doubled suffix is verbatim too", ref: "/et/paul/dogbark.git.git", wantProject: "paul", wantRepo: "dogbark.git.git"},
		{name: "single-char repo", ref: "/et/paul/x", wantProject: "paul", wantRepo: "x"},
		{name: "shortest project", ref: "/et/abc/dogbark", wantProject: "abc", wantRepo: "dogbark"},
		{name: "longest project", ref: "/et/" + maxProject + "/dogbark", wantProject: maxProject, wantRepo: "dogbark"},
		{name: "longest repo", ref: "/et/paul/" + maxRepo, wantProject: "paul", wantRepo: maxRepo},
		// Hyphens and dots are only constrained at the edges and by the ".."
		// ban, so these interior runs are names the server accepts: a
		// per-segment regex would wrongly refuse them.
		{name: "consecutive hyphens", ref: "/et/paul/dog--bark", wantProject: "paul", wantRepo: "dog--bark"},
		{name: "hyphen beside a dot", ref: "/et/paul/dog-.bark", wantProject: "paul", wantRepo: "dog-.bark"},

		// No forge token: not a native ref, whatever the names look like.
		{name: "bare pair is not a shorthand", ref: "paul/dogbark", wantErr: true},
		{name: "bare pair with leading slash", ref: "/paul/dogbark", wantErr: true},
		{name: "ssh github url", ref: "git@github.com:foo/bar", wantErr: true},
		{name: "ssh github url with .git", ref: "git@github.com:foo/bar.git", wantErr: true},
		{name: "host-qualified pair", ref: "github.com/dogbark", wantErr: true},
		{name: "gh ref is not native", ref: "/gh/entirehq/entire-api", wantErr: true},
		{name: "truncated gh ref", ref: "/gh/entirehq", wantErr: true},
		{name: "single segment", ref: "dogbark", wantErr: true},
		{name: "empty", ref: "", wantErr: true},
		{name: "double leading slash hides the token", ref: "//et/paul/dogbark", wantErr: true},

		// Token present, wrong number of names.
		{name: "token and project only", ref: "/et/paul", wantErr: true},
		{name: "token alone", ref: "/et/", wantErr: true},
		{name: "too many segments", ref: "/et/paul/dogbark/extra", wantErr: true},

		// Token present, names refused.
		{name: "empty project", ref: "/et//dogbark", wantErr: true},
		{name: "empty repo", ref: "/et/paul/", wantErr: true},
		{name: "forge token in the project position", ref: "/et/gh/foo", wantErr: true},
		{name: "underscore in project", ref: "/et/foo_bar/dogbark", wantErr: true},
		{name: "underscore in repo", ref: "/et/paul/dog_bark", wantErr: true},
		{name: "space in repo", ref: "/et/paul/dog bark", wantErr: true},
		{name: "two-char project cannot exist server-side", ref: "/et/ab/dogbark", wantErr: true},
		{name: "over-long project", ref: "/et/" + maxProject + "d/dogbark", wantErr: true},
		{name: "over-long repo", ref: "/et/paul/" + maxRepo + "d", wantErr: true},
		{name: "leading hyphen in project", ref: "/et/-paul/dogbark", wantErr: true},
		{name: "trailing hyphen in project", ref: "/et/paul-/dogbark", wantErr: true},
		{name: "leading hyphen in repo", ref: "/et/paul/-dogbark", wantErr: true},
		{name: "trailing hyphen in repo", ref: "/et/paul/dogbark-", wantErr: true},
		{name: "leading dot in repo", ref: "/et/paul/.dogbark", wantErr: true},
		{name: "trailing dot in repo", ref: "/et/paul/dogbark.", wantErr: true},
		{name: "consecutive dots in repo", ref: "/et/paul/dog..bark", wantErr: true},
		{name: "dot-only repo", ref: "/et/paul/..", wantErr: true},
		{name: "git suffix alone leaves no name", ref: "/et/paul/.git", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			project, repo, err := parseNativeCloneRef(tt.ref)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantProject, project)
			require.Equal(t, tt.wantRepo, repo)
		})
	}
}

// TestCloneRefAlwaysRequiresItsForgePrefix is a security test, not a parser
// test. `<owner>/<repo>` and `<project>/<repo>` are the SAME shape, so any
// default forge would let one namespace shadow the other: register `acme/tool`
// in whichever namespace the CLI happens to prefer, and a ref the user believes
// names the other one silently resolves — and then gets cloned and run. There
// is no safe default, so there is none.
//
// What this pins is that the requirement holds at every layer, not just in the
// parser a future refactor might route around: neither grammar accepts a bare
// pair, the command refuses one without reaching the control plane or `git
// clone`, and where a bare pair could legitimately mean either forge the error
// names BOTH — never resolving, and never quietly choosing one.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestCloneRefAlwaysRequiresItsForgePrefix(t *testing.T) {
	// Shapes an attacker would rely on: a plausible mirror pair, a plausible
	// native pair, and the spellings a user might paste or a script might build.
	bare := []string{
		"entireio/cli",
		"paul/dogbark",
		"/paul/dogbark",
		"paul/dogbark/",
		"Paul/DogBark",
		"paul/dogbark.git",
		"acme/tool",
	}

	t.Run("neither grammar accepts a bare pair", func(t *testing.T) {
		for _, ref := range bare {
			_, _, nativeErr := parseNativeCloneRef(ref)
			require.Errorf(t, nativeErr, "parseNativeCloneRef(%q) must not accept a forge-less pair", ref)
			_, _, _, mirrorErr := parseMirrorCloneRef(ref)
			require.Errorf(t, mirrorErr, "parseMirrorCloneRef(%q) must not accept a forge-less pair", ref)
		}
	})

	t.Run("the command refuses before the control plane or git", func(t *testing.T) {
		// The server IS the assertion: any request at all means a forge-less
		// ref reached resolution, so there is nothing to count afterwards.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a ref without a forge prefix reached the control plane: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		// A host-qualified pair is not a ref either: naming github.com says
		// which forge, but `repo clone` still takes only the /gh/ form, so this
		// must not become a second, laxer way in.
		for _, ref := range append(bare, "github.com/acme/tool", "https://github.com/acme/tool") {
			_, errOut, err := runCoreCmd(t, newRepoCloneCmd, srv.URL, ref)
			require.Errorf(t, err, "repo clone %q must fail", ref)
			// The parse error, specifically: it is what proves the refusal
			// happened before any resolution rather than after a failed one.
			require.ErrorContainsf(t, err, `invalid <repo> "`+ref+`"`, "repo clone %q", ref)
			// runGitClone announces itself on stderr before it execs, so its
			// absence is what pins that no clone was attempted.
			require.NotContainsf(t, errOut, "Cloning", "repo clone %q must not reach git clone", ref)
		}
	})

	t.Run("an ambiguous pair is never resolved to one forge", func(t *testing.T) {
		// Both readings parse, so both must be offered. A message naming one
		// would be the namesquatting default this grammar exists to avoid.
		_, _, nativeErr := parseNativeCloneRef("acme/tool")
		_, _, _, mirrorErr := parseMirrorCloneRef("acme/tool")
		msg := invalidCloneRefError("acme/tool", nativeErr, mirrorErr).Error()
		require.Contains(t, msg, "/et/acme/tool")
		require.Contains(t, msg, "/gh/acme/tool")
	})

	// `repo clone` is no longer the only command parsing a forge-prefixed ref:
	// resolveRepoRef took the native grammar for `repo view`, `repo delete`, and
	// the visibility and protection subtrees (COR-1632); `repo grant` parses
	// the path with parseNativeCloneRef first (resolveRepoPath), so the guard
	// covers it by construction. The guard follows the requirement rather than the command, so
	// the same table runs against the second entry point — a bare pair must not
	// become resolvable just because it was typed at a different subcommand.
	t.Run("the shared repo-ref resolver refuses a bare pair too", func(t *testing.T) {
		for _, ref := range append(bare, "github.com/acme/tool", "https://github.com/acme/tool") {
			c, calls := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("a ref without a forge prefix reached the control plane: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})
			// With --project set as well: the flag must not become a way to
			// have a forge-less pair read as a name inside that project.
			for _, project := range []string{"", "acme"} {
				_, err := resolveRepoRef(context.Background(), c, ref, project)
				require.Errorf(t, err, "resolveRepoRef(%q, project=%q) must not accept a forge-less pair", ref, project)
			}
			require.Zerof(t, calls.Load(), "ref %q must be refused before the control plane", ref)
		}
	})
}

// TestCloneForgeTokensAreGitremotePathTokens ties the two ends of the forge
// token together. gitremote.pathForges decides which refs git-remote-entire
// SUGGESTS (`entire repo clone /<token>/…`); these constants decide which
// `repo clone` ACCEPTS. Nothing else relates the two packages, so a rename on
// either side would silently start suggesting a command that fails — the very
// thing pathForges' doc gives as the reason it excludes the legacy /git/ token.
func TestCloneForgeTokensAreGitremotePathTokens(t *testing.T) {
	t.Parallel()
	require.True(t, gitremote.IsForgePathToken(nativeCloneForge), "native token %q must be a gitremote path token", nativeCloneForge)
	require.True(t, gitremote.IsForgePathToken(mirrorCloneForge), "mirror token %q must be a gitremote path token", mirrorCloneForge)
}

// TestBareRefSuggestions covers the replacement for the removed shorthand: a
// bare pair is offered every forge-qualified reading that would actually parse,
// and nothing else. Offering a candidate that fails in turn, or guessing a
// single forge when both fit, would each defeat the point.
func TestBareRefSuggestions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		want []string
	}{
		{name: "legal on both", ref: "paul/dogbark", want: []string{"/et/paul/dogbark", "/gh/paul/dogbark"}},
		{name: "leading slash", ref: "/paul/dogbark", want: []string{"/et/paul/dogbark", "/gh/paul/dogbark"}},
		// Each of these is a legal GitHub name and an illegal native one, so
		// only the mirror reading is possible.
		{name: "underscore is GitHub-only", ref: "paul/dog_bark", want: []string{"/gh/paul/dog_bark"}},
		{name: "leading dot is GitHub-only", ref: "paul/.foo", want: []string{"/gh/paul/.foo"}},
		{name: "two-char project is GitHub-only", ref: "ab/dogbark", want: []string{"/gh/ab/dogbark"}},
		// Neither grammar can take these, so there is nothing to suggest.
		{name: "dot-only repo", ref: "paul/..", want: nil},
		{name: "space", ref: "paul/dog bark", want: nil},
		{name: "single segment", ref: "dogbark", want: nil},
		{name: "three segments", ref: "gh/paul/dogbark", want: nil},
		{name: "empty segment", ref: "paul//dogbark", want: nil},
		{name: "empty", ref: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, bareRefSuggestions(tt.ref))
		})
	}
}

// TestInvalidCloneRefError locks in that a ref which declared a forge token is
// answered with the rule it broke, and that one which declared none is not:
// telling someone who typed /et/<project>/<repo> to type /et/<project>/<repo>
// is the self-contradiction both forge branches exist to avoid.
func TestInvalidCloneRefError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ref      string
		want     string
		dontWant string
	}{
		{name: "ssh github url points at /gh/", ref: "git@github.com:Foo/Bar", want: "pass GitHub mirrors as /gh/foo/bar"},
		{name: "https github url points at /gh/", ref: "https://github.com/foo/bar.git", want: "pass GitHub mirrors as /gh/foo/bar"},
		{name: "dot-only github url gets no /gh/ hint", ref: "git@github.com:foo/..", want: cloneRefShapes, dontWant: "/gh/foo/.."},
		{name: "bad owner keeps mirror reason", ref: "/gh/foo_bar/baz", want: "owner: letters, digits, '-'", dontWant: cloneRefShapes},
		{name: "dot-only repo keeps mirror reason", ref: "/gh/foo/..", want: "repo cannot be dot-only", dontWant: cloneRefShapes},
		{name: "missing repo keeps mirror reason", ref: "gh/foo", want: "expected gh/<owner>/<repo>", dontWant: cloneRefShapes},
		{name: "bad project keeps native reason", ref: "/et/foo_bar/dogbark", want: `project "foo_bar" is not a name the server accepts`, dontWant: cloneRefShapes},
		{name: "bad repo keeps native reason", ref: "/et/paul/dog_bark", want: `repo "dog_bark" is not a name the server accepts`, dontWant: cloneRefShapes},
		{name: "dot-only native repo keeps native reason", ref: "/et/paul/..", want: "no consecutive dots", dontWant: cloneRefShapes},
		{name: "empty native repo keeps native reason", ref: "/et/paul/", want: `repo "" is not a name the server accepts`, dontWant: cloneRefShapes},
		{name: "truncated et ref names the missing segment", ref: "/et/paul", want: "expected /et/<project>/<repo> (2 names after the et token, got 1)", dontWant: cloneRefShapes},
		{name: "et ref without a leading slash keeps native reason", ref: "et/ab/cd", want: `project "ab" is not a name the server accepts`, dontWant: cloneRefShapes},
		// A bare pair declared no forge, so neither parser's reason describes
		// anything the user did; it gets the readings that would parse instead.
		{name: "bare pair is offered both forges", ref: "paul/dogbark", want: "did you mean /et/paul/dogbark or /gh/paul/dogbark?", dontWant: cloneRefShapes},
		{name: "bare pair legal on one forge is offered that one", ref: "paul/.foo", want: "did you mean /gh/paul/.foo?", dontWant: "is not a name the server accepts"},
		{name: "host-qualified pair points at /gh/", ref: "github.com/acme/app", want: "pass GitHub mirrors as /gh/acme/app"},
		{name: "unsuggestable bare pair lists all shapes", ref: "paul/dog bark", want: cloneRefShapes},
		{name: "unknown shape lists all shapes", ref: "/gl/foo/bar", want: cloneRefShapes, dontWant: "expected gh/"},
		{name: "single segment lists all shapes", ref: "dogbark", want: cloneRefShapes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, nativeErr := parseNativeCloneRef(tt.ref)
			require.Error(t, nativeErr)
			_, _, _, mirrorErr := parseMirrorCloneRef(tt.ref)
			require.Error(t, mirrorErr)
			got := invalidCloneRefError(tt.ref, nativeErr, mirrorErr).Error()
			require.Contains(t, got, `invalid <repo> "`+tt.ref+`"`)
			require.Contains(t, got, tt.want)
			if tt.dontWant != "" {
				require.NotContains(t, got, tt.dontWant)
			}
		})
	}
}

const testNativeRepoULID = "01ARZ3NDEKTSV4RRFFQ69G5FBB"

// nativeRepoFixture configures serveNativeRepo's fake control plane beyond the
// identity chain: the repo's native-mirror placements, the cluster catalog
// their slugs resolve against, and an optional non-200 status for the mirror
// listing (a legacy or unconfigured core).
type nativeRepoFixture struct {
	repo           coreapi.Repo
	mirrors        []coreapi.NativeMirrorPlacement
	clusters       []coreapi.Cluster
	mirrorsStatus  int
	clustersStatus int
	// queriedFullName, when non-nil, is set to the <project>/<repo> full name
	// the native path lookup was actually asked to resolve. The response below
	// is canned, so a test that wants to assert the ref it parsed (not just the
	// fixture it wired up) reaches the request needs this rather than the reply.
	queriedFullName *string
}

// serveNativeRepo fakes the two-call native resolution chain: POST
// /repos/resolve, then the single-repo GET (the one response that carries
// clusterHost + path). No /projects route is served: a native ref must resolve
// with repo#pull alone. The mirror listing answers empty, so resolution sees
// exactly one placement: the home cluster.
func serveNativeRepo(t *testing.T, repo coreapi.Repo) *coreapi.Client {
	t.Helper()
	return serveNativeRepoFixture(t, nativeRepoFixture{repo: repo})
}

func serveNativeRepoFixture(t *testing.T, fx nativeRepoFixture) *coreapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api/v1/repos/resolve":
			if fx.queriedFullName != nil {
				var in coreapi.ResolveReposInputBody
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Errorf("decode resolve body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(in.Repositories) != 1 {
					t.Errorf("resolve body = %+v, want one reference", in.Repositories)
				}
				if len(in.Repositories) > 0 {
					*fx.queriedFullName = in.Repositories[0].FullName
				}
			}
			body = nativeResolution("paul/"+fx.repo.Name, testNativeRepoULID)
		case "/api/v1/repos/" + testNativeRepoULID:
			body = &fx.repo
		case "/api/v1/repos/" + testNativeRepoULID + "/native-mirrors":
			if fx.mirrorsStatus != 0 {
				w.WriteHeader(fx.mirrorsStatus)
				return
			}
			mirrors := fx.mirrors
			if mirrors == nil {
				mirrors = []coreapi.NativeMirrorPlacement{}
			}
			body = &coreapi.ListNativeMirrorsOutputBody{NativeMirrors: mirrors}
		case "/api/v1/clusters":
			if fx.clustersStatus != 0 {
				w.WriteHeader(fx.clustersStatus)
				return
			}
			if fx.clusters == nil {
				t.Errorf("unexpected cluster catalog fetch: fixture has no clusters")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body = &coreapi.ListClustersOutputBody{Clusters: fx.clusters}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)
	return c
}

// readyNativeMirror is a native-mirror placement in the one state the clone
// path offers as a placement: fully announced and still meant to exist.
func readyNativeMirror(slug string) coreapi.NativeMirrorPlacement {
	return coreapi.NativeMirrorPlacement{
		PlacementId:  "01ARZ3NDEKTSV4RRFFQ69G5FCC",
		ClusterSlug:  slug,
		Status:       coreapi.NativeMirrorPlacementStatusReady,
		DesiredState: coreapi.NativeMirrorPlacementDesiredStateActive,
		Stage:        coreapi.NativeMirrorPlacementStageAnnounced,
	}
}

func TestResolveNativeCloneURL(t *testing.T) {
	t.Parallel()

	native := func(host, path string) coreapi.Repo {
		r := coreapi.Repo{ID: testNativeRepoULID, Name: "dogbark", OwningProjectId: testProjectULID}
		if host != "" {
			r.ClusterHost = coreapi.NewOptString(host)
		}
		if path != "" {
			r.Path = coreapi.NewOptString(path)
		}
		return r
	}

	resolve := func(t *testing.T, c *coreapi.Client, clusterSel string) (string, error) {
		t.Helper()
		// The default picker, which has no probe: these cases are about URL
		// construction, not about which placement wins. The --nearest native
		// path has its own case below.
		return resolveNativeCloneURL(t.Context(), newCloneTestCmd(), c, "paul", "dogbark", clusterSel, stubPlacementPicker(nil))
	}

	t.Run("builds the URL from the server's clusterHost and path", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"))
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("missing path means not ready, not a half-formed URL", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io", ""))
		_, err := resolve(t, c, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "no clone URL")
	})

	t.Run("missing cluster host means not ready", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("", "/et/paul/dogbark"))
		_, err := resolve(t, c, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "no clone URL")
	})

	t.Run("malformed server host is rejected before it reaches git", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io@evil.com", "/et/paul/dogbark"))
		_, err := resolve(t, c, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid cluster host")
	})

	usEast := coreapi.Cluster{Slug: "aws-us-east-2", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io"}

	t.Run("--cluster picks a ready native mirror", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:     native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:  []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clusters: []coreapi.Cluster{usEast},
		})
		got, err := resolve(t, c, "aws-us-east-2.entire.io")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-us-east-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("--cluster still selects the home cluster", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:     native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:  []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clusters: []coreapi.Cluster{usEast},
		})
		got, err := resolve(t, c, "aws-ap-southeast-2.entire.io")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("several placements and no terminal resolve the home cluster", func(t *testing.T) {
		t.Parallel()
		// A native repo's home cluster is its data primary, so it is the answer
		// a script gets when there is no terminal to pick a mirror on.
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:     native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:  []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clusters: []coreapi.Cluster{usEast},
		})
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("a clearly nearer native mirror resolves without a terminal", func(t *testing.T) {
		t.Parallel()
		// End to end on the native path: home is the far cluster, the ready
		// mirror is near, and the clone URL follows the measurement instead of
		// demanding --cluster.
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:     native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:  []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clusters: []coreapi.Cluster{usEast},
		})
		got, err := resolveNativeCloneURL(t.Context(), newCloneTestCmd(), c, "paul", "dogbark", "",
			stubPlacementPicker(map[string]probeResult{
				"aws-ap-southeast-2.entire.io": {rtt: 190 * time.Millisecond},
				"aws-us-east-2.entire.io":      {rtt: 16 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Equal(t, "entire://aws-us-east-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("a mirror that is not ready or marked deleted is not a placement", func(t *testing.T) {
		t.Parallel()
		processing := readyNativeMirror("aws-us-east-2")
		processing.Status = coreapi.NativeMirrorPlacementStatusProcessing
		deleted := readyNativeMirror("aws-eu-central-1")
		deleted.DesiredState = coreapi.NativeMirrorPlacementDesiredStateDeleted
		// No clusters in the fixture: with no ready mirror the catalog must not
		// be fetched at all, and serveNativeRepoFixture fails the test if it is.
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:    native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors: []coreapi.NativeMirrorPlacement{processing, deleted},
		})
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("a failed mirror listing degrades to the home cluster", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:          native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrorsStatus: http.StatusNotFound,
		})
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("a failed mirror listing surfaces when --cluster asked for a placement", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:          native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrorsStatus: http.StatusNotFound,
		})
		_, err := resolve(t, c, "aws-us-east-2.entire.io")
		require.Error(t, err)
		require.Contains(t, err.Error(), "list native mirrors")
	})

	t.Run("a failed catalog fetch degrades to the home cluster", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:           native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:        []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clustersStatus: http.StatusServiceUnavailable,
		})
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("a failed catalog fetch surfaces when --cluster asked for a placement", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:           native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:        []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clustersStatus: http.StatusServiceUnavailable,
		})
		_, err := resolve(t, c, "aws-us-east-2.entire.io")
		require.Error(t, err)
		require.Contains(t, err.Error(), "list clusters")
	})

	t.Run("a cancelled context surfaces instead of degrading to the home cluster", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo: native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		repo := coreapi.Repo{
			ID:          testNativeRepoULID,
			ClusterHost: coreapi.NewOptString("aws-ap-southeast-2.entire.io"),
			Path:        coreapi.NewOptString("/et/paul/dogbark"),
		}
		_, err := nativePlacements(ctx, c, &repo, false)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("a mirror slug the catalog cannot resolve safely is omitted", func(t *testing.T) {
		t.Parallel()
		evil := coreapi.Cluster{Slug: "aws-us-east-2", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io@evil.com"}
		c := serveNativeRepoFixture(t, nativeRepoFixture{
			repo:     native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"),
			mirrors:  []coreapi.NativeMirrorPlacement{readyNativeMirror("aws-us-east-2")},
			clusters: []coreapi.Cluster{evil},
		})
		got, err := resolve(t, c, "")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})
}

// TestRepoClone_NativeInvalidClusterFlag locks in that a malformed --cluster on
// a native ref is rejected up front (before any core is dialled), same as the
// /gh/ branch: the anti-token-leak guard validateClusterHost applies to the
// user-supplied cluster the clone routes to.
func TestRepoClone_NativeInvalidClusterFlag(t *testing.T) {
	t.Parallel()
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	cmd.SetArgs([]string{"/et/paul/dogbark", "--cluster", "aws-us-east-2.entire.io@evil.com"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --cluster")
}

func TestIsEntireCloneURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  string
		want bool
	}{
		{ref: "entire://aws-us-east-2.entire.io/gh/entirehq/entire-api", want: true},
		{ref: "  entire://host/gh/a/b", want: true},
		{ref: "/gh/entirehq/entire-api", want: false},
		{ref: "gh/entirehq/entire-api", want: false},
		{ref: "https://github.com/entirehq/entire-api", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isEntireCloneURL(tt.ref))
		})
	}
}

func TestMirrorCloneURL(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"entire://aws-us-east-2.entire.io/gh/entirehq/entire-api",
		forgeCloneURL(mirrorCloneForge, "aws-us-east-2.entire.io", "entirehq", "entire-api"))
}

func TestMirrorCellLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.ResolvedPlacement
		rtt    string
		want   string
	}{
		{
			name:   "host only",
			mirror: coreapi.ResolvedPlacement{ClusterHost: "aws-us-east-2.entire.io"},
			want:   "aws-us-east-2.entire.io",
		},
		{
			name: "cell and jurisdiction",
			mirror: coreapi.ResolvedPlacement{
				ClusterHost:  "aws-us-east-2.entire.io",
				Cell:         coreapi.NewOptString("aws-us-east-2"),
				Jurisdiction: coreapi.NewOptString("us"),
			},
			want: "aws-us-east-2 (us) — aws-us-east-2.entire.io",
		},
		{
			name: "cell without jurisdiction",
			mirror: coreapi.ResolvedPlacement{
				ClusterHost: "aws-us-east-2.entire.io",
				Cell:        coreapi.NewOptString("aws-us-east-2"),
			},
			want: "aws-us-east-2 — aws-us-east-2.entire.io",
		},
		{
			name: "measured round trip is appended",
			mirror: coreapi.ResolvedPlacement{
				ClusterHost: "aws-us-east-2.entire.io",
				Cell:        coreapi.NewOptString("aws-us-east-2"),
			},
			rtt:  "18ms",
			want: "aws-us-east-2 — aws-us-east-2.entire.io [18ms]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mirrorCellLabel(tt.mirror, tt.rtt))
		})
	}
}

// TestRepoClone_NearestWithCluster locks in that the two cluster selectors are
// refused together rather than silently ranked. They disagree whenever
// --cluster is not already the nearest, and the loser here is the remote URL
// the user keeps.
func TestRepoClone_NearestWithCluster(t *testing.T) {
	t.Parallel()
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	cmd.SetArgs([]string{"/gh/entirehq/entire-api", "--nearest", "--cluster", "aws-us-east-2.entire.io"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "pass one")
}

// TestRepoClone_NearestIsOptIn locks in that the probe is wired to the flag and
// nothing else: the default picker must have no probe, so a plain clone dials
// no cluster and keeps the behaviour it has always had.
func TestRepoClone_NearestIsOptIn(t *testing.T) {
	t.Parallel()
	require.Nil(t, clonePlacementPicker().probe, "the default clone picker must not probe")
	require.NotNil(t, withLatencyProbe(clonePlacementPicker()).probe, "--nearest must install a probe")
}

// TestRepoClone_InvalidClusterFlag locks in that a malformed --cluster is
// rejected up front (before any core is dialled), so the anti-token-leak guard
// validateClusterHost applies to the user-supplied cluster the clone routes to.
func TestRepoClone_InvalidClusterFlag(t *testing.T) {
	t.Parallel()
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	cmd.SetArgs([]string{"/gh/entirehq/entire-api", "--cluster", "aws-us-east-2.entire.io@evil.com"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --cluster")
}

func newCloneTestCmd() *cobra.Command {
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	return cmd
}

type nopWriter struct{}

func (*nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestSelectCloneTarget(t *testing.T) {
	t.Parallel()

	usEast := coreapi.ResolvedPlacement{ClusterHost: "aws-us-east-2.entire.io"}
	euWest := coreapi.ResolvedPlacement{ClusterHost: "aws-eu-west-1.entire.io"}

	t.Run("single placement returns directly", func(t *testing.T) {
		t.Parallel()
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast}, "", "", clonePlacementPicker())
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("dedupes repeated host to a single placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, usEast}, "", "", clonePlacementPicker())
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("--cluster picks the matching placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-eu-west-1.entire.io", "", clonePlacementPicker())
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster matches case-insensitively", func(t *testing.T) {
		t.Parallel()
		// DNS hosts are case-insensitive: a mixed-case --cluster must still match
		// the API's lowercase ClusterHost rather than falsely "not mirrored".
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "AWS-EU-West-1.Entire.IO", "", clonePlacementPicker())
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster with no match errors and lists hosts", func(t *testing.T) {
		t.Parallel()
		_, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-ap-south-1.entire.io", "", clonePlacementPicker())
		require.Error(t, err)
		require.Contains(t, err.Error(), "aws-us-east-2.entire.io")
		require.Contains(t, err.Error(), "aws-eu-west-1.entire.io")
	})

	t.Run("--cluster loses to nothing: an explicit miss errors even when a default would match", func(t *testing.T) {
		t.Parallel()
		// The default only stands in for an absent selector. A --cluster the repo
		// is not on is a typo the user must see, not a reason to clone elsewhere.
		_, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-ap-south-1.entire.io", "aws-us-east-2.entire.io", clonePlacementPicker())
		require.ErrorContains(t, err, "aws-ap-south-1.entire.io")
	})

	t.Run("no terminal resolves the default placement", func(t *testing.T) {
		t.Parallel()
		// go test is non-interactive, so this is the path a script or CI run takes.
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{euWest, usEast}, "", "aws-us-east-2.entire.io", clonePlacementPicker())
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("no terminal and the default is not one of the placements errors", func(t *testing.T) {
		t.Parallel()
		_, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-ap-south-1.entire.io", clonePlacementPicker())
		require.ErrorContains(t, err, "none of them is aws-ap-south-1.entire.io")
		require.Contains(t, err.Error(), "--cluster")
		require.Contains(t, err.Error(), "aws-us-east-2.entire.io")
		require.Contains(t, err.Error(), "aws-eu-west-1.entire.io")
	})

	t.Run("no terminal and no known primary asks only for the selector", func(t *testing.T) {
		t.Parallel()
		// A caller that could not determine a primary passes none. Phrasing that
		// as a primary the repo lacks would name an empty host.
		_, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "", "", clonePlacementPicker())
		require.ErrorContains(t, err, "repo is on 2 clusters; pass --cluster")
		require.NotContains(t, err.Error(), "none of them is")
	})

	t.Run("--nearest displaces the primary as the no-terminal default", func(t *testing.T) {
		t.Parallel()
		// The primary is eu-west and would win without the flag. Substituting
		// the measured nearest for it is precisely what --nearest asks for.
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-eu-west-1.entire.io",
			stubPlacementPicker(map[string]probeResult{
				"aws-eu-west-1.entire.io": {rtt: 210 * time.Millisecond},
				"aws-us-east-2.entire.io": {rtt: 14 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("--nearest keeps a primary that answered no probe", func(t *testing.T) {
		t.Parallel()
		// End to end on the regression: the mirror is measured but slow in
		// absolute terms and the primary is silent. The primary stays, because
		// one number is not a comparison.
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-eu-west-1.entire.io",
			stubPlacementPicker(map[string]probeResult{
				"aws-us-east-2.entire.io": {rtt: 300 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("nothing is announced when the primary is kept", func(t *testing.T) {
		t.Parallel()
		// The announcement exists to name a trade. No trade was made, so a line
		// claiming a "nearest placement" would report a choice that never happened.
		var stderr strings.Builder
		cmd := newRepoCloneCmd()
		cmd.SetOut(&nopWriter{})
		cmd.SetErr(&stderr)
		_, err := selectPlacement(cmd, []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-eu-west-1.entire.io",
			stubPlacementPicker(map[string]probeResult{
				"aws-us-east-2.entire.io": {rtt: 300 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Empty(t, stderr.String())
	})

	t.Run("an invalid placement host is never dialled", func(t *testing.T) {
		t.Parallel()
		// validateClusterHost gates the CHOSEN placement further down; the probe
		// must not reach past that guard. An empty host dials ":443" — the local
		// machine — so the probe has to be handed a filtered list.
		blank := coreapi.ResolvedPlacement{ClusterHost: ""}
		picker := withLatencyProbe(clonePlacementPicker())
		picker.probe = func(_ context.Context, hosts []string) map[string]probeResult {
			require.NotContains(t, hosts, "", "an unvalidated host reached the probe")
			require.Len(t, hosts, 2, "only the valid hosts are dialled")
			return map[string]probeResult{
				"aws-eu-west-1.entire.io": {rtt: 210 * time.Millisecond},
				"aws-us-east-2.entire.io": {rtt: 14 * time.Millisecond},
			}
		}
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest, blank}, "", "aws-eu-west-1.entire.io", picker)
		require.NoError(t, err)
		// Still offered, just unmeasured: a rejected host is dropped from the
		// probe, not from the picker, and is refused after selection anyway.
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("--nearest falls back to the primary when every probe failed", func(t *testing.T) {
		t.Parallel()
		// Opting in does not guarantee a measurement, and an unmeasurable
		// network must land where a caller who never passed the flag lands,
		// not on an error the flag introduced.
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-eu-west-1.entire.io",
			stubPlacementPicker(map[string]probeResult{}))
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("the chosen placement is announced on stderr, naming the primary it replaced", func(t *testing.T) {
		t.Parallel()
		// The host lands in .git/config and every later fetch follows it. A
		// mirror lags its primary, so trading one for the other is the part a
		// reader most needs to see named.
		var stderr strings.Builder
		cmd := newRepoCloneCmd()
		cmd.SetOut(&nopWriter{})
		cmd.SetErr(&stderr)
		_, err := selectPlacement(cmd, []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-eu-west-1.entire.io",
			stubPlacementPicker(map[string]probeResult{
				"aws-eu-west-1.entire.io": {rtt: 210 * time.Millisecond},
				"aws-us-east-2.entire.io": {rtt: 14 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Contains(t, stderr.String(), "aws-us-east-2.entire.io")
		require.Contains(t, stderr.String(), "14ms")
		require.Contains(t, stderr.String(), "not the primary aws-eu-west-1.entire.io")
		// Both figures, so the reader can weigh the trade rather than take the
		// word "nearest" on trust.
		require.Contains(t, stderr.String(), "210ms")
		require.Contains(t, stderr.String(), "--cluster")
	})

	t.Run("no announcement when the nearest IS the primary", func(t *testing.T) {
		t.Parallel()
		// Nothing was traded away, so there is nothing to warn about.
		var stderr strings.Builder
		cmd := newRepoCloneCmd()
		cmd.SetOut(&nopWriter{})
		cmd.SetErr(&stderr)
		got, err := selectPlacement(cmd, []coreapi.ResolvedPlacement{usEast, euWest}, "", "aws-us-east-2.entire.io",
			stubPlacementPicker(map[string]probeResult{
				"aws-eu-west-1.entire.io": {rtt: 210 * time.Millisecond},
				"aws-us-east-2.entire.io": {rtt: 14 * time.Millisecond},
			}))
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
		require.NotContains(t, stderr.String(), "not the primary")
	})

	t.Run("an explicit --cluster is never probed", func(t *testing.T) {
		t.Parallel()
		// A choice already made must not cost a dial, so a probe that fails the
		// test if called proves the short-circuit.
		picker := withLatencyProbe(clonePlacementPicker())
		picker.probe = func(context.Context, []string) map[string]probeResult {
			t.Error("probed despite an explicit --cluster")
			return nil
		}
		got, err := selectPlacement(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-eu-west-1.entire.io", "aws-us-east-2.entire.io", picker)
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})
}

// stubPlacementPicker is the opted-in (`--nearest`) picker with the dialling
// probe replaced by a fixed table, so placement-selection tests stay hermetic
// and parallel. An empty table means "every probe failed", the fallback every
// path must survive.
//
// Passing nil instead models the DEFAULT picker, which has no probe at all —
// the two are distinct: no probe never dials, a failed probe dialled and got
// nothing, and both must end at the same alphabetical behaviour.
func stubPlacementPicker(rtt map[string]probeResult) placementPicker {
	p := clonePlacementPicker()
	if rtt == nil {
		return p
	}
	p.probe = func(context.Context, []string) map[string]probeResult { return rtt }
	return p
}

// TestResolvePullablePlacements_ReturnsPlacements verifies the clone-discovery
// resolver hits the pull-gated /mirrors/placements endpoint with the upstream
// coords and returns every placement (host + cell + jurisdiction) for the
// picker. A public mirror the caller holds no grant on resolves here even
// though it never would via the affiliation-scoped list — the whole point of
// the endpoint.
func TestResolvePullablePlacements_ReturnsPlacements(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		body := &coreapi.ResolvePlacementsOutputBody{Placements: []coreapi.ResolvedPlacement{
			{MirrorId: "01AAA", ClusterHost: "aws-us-east-2.entire.io", Cell: coreapi.NewOptString("aws-us-east-2"), Jurisdiction: coreapi.NewOptString("us")},
			{MirrorId: "01BBB", ClusterHost: "aws-eu-west-1.entire.io", Cell: coreapi.NewOptString("aws-eu-west-1"), Jurisdiction: coreapi.NewOptString("eu")},
		}}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)

	got, err := resolvePullablePlacements(t.Context(), c, "karthik-rameshkumar", "my-entire")
	require.NoError(t, err)

	require.Equal(t, "/api/v1/mirrors/placements", gotPath)
	require.Contains(t, gotQuery, "provider=github")
	require.Contains(t, gotQuery, "owner=karthik-rameshkumar")
	require.Contains(t, gotQuery, "repo=my-entire")

	require.Len(t, got, 2)
	require.Equal(t, "aws-us-east-2.entire.io", got[0].ClusterHost)
	require.Equal(t, "aws-us-east-2", got[0].Cell.Or(""))
	require.Equal(t, "us", got[0].Jurisdiction.Or(""))
	require.Equal(t, "01AAA", got[0].MirrorId)
	require.Equal(t, "aws-eu-west-1.entire.io", got[1].ClusterHost)
}

// TestListMirrorsForRepo_FiltersByRepo verifies the client-side repo filter:
// the list API filters provider+owner server-side, but the repo match (which
// the API has no param for) is applied locally.
func TestListMirrorsForRepo_FiltersByRepo(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
			{Owner: "entirehq", Repo: "entire-api", ClusterHost: "aws-us-east-2.entire.io"},
			{Owner: "entirehq", Repo: "entire-api", ClusterHost: "aws-eu-west-1.entire.io"},
			{Owner: "entirehq", Repo: "entire-cli", ClusterHost: "aws-us-east-2.entire.io"},
		}}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)

	got, err := listMirrorsForRepo(t.Context(), c, "github", "entirehq", "entire-api")
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		require.Equal(t, "entire-api", m.Repo)
	}
}
