package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		// Same policy as the native side (see gitDirSuffix): GitHub cannot hold
		// a name ending in .git, so the suffix is only ever decoration.
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
		// `.git` is never part of a name on either backend (see gitDirSuffix),
		// so it is dropped before the name is validated.
		{name: "git suffix is dropped", ref: "/et/paul/dogbark.git", wantProject: "paul", wantRepo: "dogbark"},
		{name: "git suffix dropped from a dotted name", ref: "/et/paul/entire-trails.el.git", wantProject: "paul", wantRepo: "entire-trails.el"},
		{name: "only the last git suffix is dropped", ref: "/et/paul/dogbark.git.git", wantProject: "paul", wantRepo: "dogbark.git"},
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

// serveNativeRepo fakes the three-call native resolution chain: project by
// name, repo by name within the project, then the single-repo GET (the one
// response that carries clusterHost + path).
func serveNativeRepo(t *testing.T, repo coreapi.Repo) *coreapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api/v1/projects":
			body = &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
				ID: testProjectULID, Name: "paul", OwnerId: testProjectULID, OwnerType: coreapi.ProjectOwnerTypeOrg,
			})}
		case "/api/v1/projects/" + testProjectULID + "/repos":
			body = &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
				ID: testNativeRepoULID, Name: repo.Name, OwningProjectId: testProjectULID,
			})}
		case "/api/v1/repos/" + testNativeRepoULID:
			body = &repo
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

	t.Run("builds the URL from the server's clusterHost and path", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io", "/et/paul/dogbark"))
		got, err := resolveNativeCloneURL(t.Context(), c, "paul", "dogbark")
		require.NoError(t, err)
		require.Equal(t, "entire://aws-ap-southeast-2.entire.io/et/paul/dogbark", got)
	})

	t.Run("missing path means not ready, not a half-formed URL", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io", ""))
		_, err := resolveNativeCloneURL(t.Context(), c, "paul", "dogbark")
		require.Error(t, err)
		require.Contains(t, err.Error(), "no clone URL")
	})

	t.Run("missing cluster host means not ready", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("", "/et/paul/dogbark"))
		_, err := resolveNativeCloneURL(t.Context(), c, "paul", "dogbark")
		require.Error(t, err)
		require.Contains(t, err.Error(), "no clone URL")
	})

	t.Run("malformed server host is rejected before it reaches git", func(t *testing.T) {
		t.Parallel()
		c := serveNativeRepo(t, native("aws-ap-southeast-2.entire.io@evil.com", "/et/paul/dogbark"))
		_, err := resolveNativeCloneURL(t.Context(), c, "paul", "dogbark")
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid cluster host")
	})
}

// TestRepoClone_NativeRefRejectsClusterFlag locks in that --cluster (a mirror
// placement selector) fails fast on a native ref instead of being ignored.
func TestRepoClone_NativeRefRejectsClusterFlag(t *testing.T) {
	t.Parallel()
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	cmd.SetArgs([]string{"/et/paul/dogbark", "--cluster", "aws-us-east-2.entire.io"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "--cluster applies to /gh/ mirror refs")
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
		mirrorCloneURL("aws-us-east-2.entire.io", "entirehq", "entire-api"))
}

func TestMirrorCellLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.ResolvedPlacement
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mirrorCellLabel(tt.mirror))
		})
	}
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
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast}, "")
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("dedupes repeated host to a single placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, usEast}, "")
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("--cluster picks the matching placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-eu-west-1.entire.io")
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster matches case-insensitively", func(t *testing.T) {
		t.Parallel()
		// DNS hosts are case-insensitive: a mixed-case --cluster must still match
		// the API's lowercase ClusterHost rather than falsely "not mirrored".
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "AWS-EU-West-1.Entire.IO")
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster with no match errors and lists hosts", func(t *testing.T) {
		t.Parallel()
		_, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-ap-south-1.entire.io")
		require.Error(t, err)
		require.Contains(t, err.Error(), "aws-us-east-2.entire.io")
		require.Contains(t, err.Error(), "aws-eu-west-1.entire.io")
	})

	t.Run("multiple placements with no terminal errors with a --cluster pointer", func(t *testing.T) {
		t.Parallel()
		// go test is non-interactive, so the picker path is unreachable here.
		_, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "--cluster")
	})
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
