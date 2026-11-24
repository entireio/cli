package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// ulidShapedName is a valid project NAME that also looks like a ULID.
const ulidShapedName = "0123456789ABCDEFGHJKMNPQR7"

func TestParseControlPlaneProject(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		ref     string
		want    controlPlaneProject
		wantErr string
	}{
		{"widgets", controlPlaneProject{raw: "widgets", value: "widgets"}, ""},
		{ulidProjectWidgets, controlPlaneProject{raw: ulidProjectWidgets, value: ulidProjectWidgets}, ""},
		{"et/widgets", controlPlaneProject{raw: "et/widgets", value: "widgets", isName: true}, ""},
		{"et/" + ulidShapedName, controlPlaneProject{raw: "et/" + ulidShapedName, value: ulidShapedName, isName: true}, ""},
		{"gh/acme", controlPlaneProject{}, "gh/acme is the GitHub owner acme's mirror project"},
		{"et/", controlPlaneProject{}, "use a project name, et/<name>, or a ULID"},
		{"et/a/b", controlPlaneProject{}, "use a project name, et/<name>, or a ULID"},
		{"gl/acme", controlPlaneProject{}, "use a project name, et/<name>, or a ULID"},
		{"et/my_proj", controlPlaneProject{}, "project names contain only letters, digits, and hyphens"},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			got, err := parseControlPlaneProject(tt.ref)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// projectByNameHandler answers the by-name project lookup, recording the name.
func projectByNameHandler(t *testing.T, gotName *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/projects") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		*gotName = r.URL.Query().Get("name")
		if err := printJSON(w, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: ulidProjectWidgets, Name: *gotName, OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg})}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}
}

// et/<name> resolves by name, even a name shaped like a ULID, which a bare
// value would pass through as an ID.
func TestResolveProjectRef_EtReference(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"widgets", ulidShapedName} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got string
			c, calls := resolveTestClient(t, projectByNameHandler(t, &got))
			id, err := resolveProjectRef(context.Background(), c, "et/"+name)
			require.NoError(t, err)
			assert.Equal(t, ulidProjectWidgets, id)
			assert.Equal(t, name, got)
			assert.Equal(t, int64(1), calls.Load())
		})
	}
}

// repo list's resolver reads a ULID-shaped et/ name as a name too.
func TestResolveProjectRefNamed_EtULIDShapedName(t *testing.T) {
	t.Parallel()
	var got string
	c, _ := resolveTestClient(t, projectByNameHandler(t, &got))
	id, name, err := resolveProjectRefNamed(context.Background(), c, "et/"+ulidShapedName)
	require.NoError(t, err)
	assert.Equal(t, ulidProjectWidgets, id)
	assert.Equal(t, ulidShapedName, name)
	assert.Equal(t, ulidShapedName, got)
}

// gh/<owner> is refused with no network call; repo list points at the mirror
// listing.
func TestResolveProjectRef_MirrorReferenceRefused(t *testing.T) {
	t.Parallel()
	c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected HTTP call")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := resolveProjectRef(context.Background(), c, "gh/acme")
	var mirror *mirrorProjectRefError
	require.ErrorAs(t, err, &mirror)
	_, _, err = resolveProjectRefNamed(context.Background(), c, "gh/acme")
	require.ErrorContains(t, err, "list its repositories with 'entire repo mirror list --owner acme'")
	assert.Zero(t, calls.Load())
}

// project get also takes gh/<owner>, through Core's project resolver.
func TestResolveProjectGetRef_Mirror(t *testing.T) {
	t.Parallel()
	c, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/project-resolver/gh/acme") {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := printJSON(w, &coreapi.ResolveProjectOutputBody{
			Project:   coreapi.Project{ID: ulidProjectWidgets, Name: "gh:mirrors:" + ulidOrgAcme, OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg},
			Reference: coreapi.ProjectReference{Host: "gh", Project: "acme"},
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	id, err := resolveProjectGetRef(context.Background(), c, "gh/acme")
	require.NoError(t, err)
	assert.Equal(t, ulidProjectWidgets, id)
}

func TestResolveProjectGetRef_MirrorNotFound(t *testing.T) {
	t.Parallel()
	c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"title":"Not Found"}`)) //nolint:errcheck // test server
	})
	_, err := resolveProjectGetRef(context.Background(), c, "gh/acme")
	require.EqualError(t, err, "project gh/acme does not exist or is not visible to your login; check that the GitHub owner is connected to Entire")
}

// repo commands' --project takes et/<name>: a bare repo name resolves at
// <name>/<repo>, a path must agree with it (quoting what was typed), and a
// repo ULID still ignores --project, even an unusable one.
func TestResolveRepoRef_ProjectEtReference(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"widgets", ulidShapedName} {
		t.Run("bare repo name in et/"+name, func(t *testing.T) {
			t.Parallel()
			var full string
			c, _ := resolveTestClient(t, nativePathHandler(t, &full))
			id, err := resolveRepoRef(context.Background(), c, "web", "et/"+name)
			require.NoError(t, err)
			assert.Equal(t, ulidRepoWeb, id)
			assert.Equal(t, name+"/web", full)
		})
	}
	// A ULID-shaped et/ name compares as a name: no GetRepo agreement check.
	t.Run("path with a ULID-shaped et/ name", func(t *testing.T) {
		t.Parallel()
		var full string
		c, calls := resolveTestClient(t, nativePathHandler(t, &full))
		_, err := resolveRepoRef(context.Background(), c, "/et/"+ulidShapedName+"/web", "et/"+ulidShapedName)
		require.NoError(t, err)
		assert.Equal(t, int64(1), calls.Load())
	})
	t.Run("path agrees", func(t *testing.T) {
		t.Parallel()
		var full string
		c, _ := resolveTestClient(t, nativePathHandler(t, &full))
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", "et/widgets")
		require.NoError(t, err)
	})
	t.Run("path disagrees", func(t *testing.T) {
		t.Parallel()
		var full string
		c, calls := resolveTestClient(t, nativePathHandler(t, &full))
		_, err := resolveRepoRef(context.Background(), c, "/et/widgets/web", "et/gadgets")
		require.EqualError(t, err, `supplied --project "et/gadgets" does not match the project "widgets" named in "/et/widgets/web"`)
		assert.Zero(t, calls.Load())
	})
	t.Run("mirror project refused", func(t *testing.T) {
		t.Parallel()
		var full string
		c, calls := resolveTestClient(t, nativePathHandler(t, &full))
		_, err := resolveRepoRef(context.Background(), c, "web", "gh/acme")
		require.ErrorContains(t, err, "mirror project")
		assert.Zero(t, calls.Load())
	})
	t.Run("repo ULID ignores --project", func(t *testing.T) {
		t.Parallel()
		var full string
		c, calls := resolveTestClient(t, nativePathHandler(t, &full))
		id, err := resolveRepoRef(context.Background(), c, ulidRepoWeb, "gh/acme")
		require.NoError(t, err)
		assert.Equal(t, ulidRepoWeb, id)
		assert.Zero(t, calls.Load())
	})
}

func TestParseSAPath_ProjectReference(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ ref, project, name string }{
		{"widgets/ci", "widgets", "ci"},
		{"et/widgets/ci", "et/widgets", "ci"},
		{"gh/acme/ci", "gh/acme", "ci"},
	} {
		project, name, err := parseSAPath(tt.ref)
		require.NoError(t, err, tt.ref)
		assert.Equal(t, tt.project, project, tt.ref)
		assert.Equal(t, tt.name, name, tt.ref)
	}
	_, _, err := parseSAPath("et/widgets")
	require.Error(t, err)
}

func TestResolveProjectGetRef_MirrorErrors(t *testing.T) {
	t.Parallel()
	t.Run("forbidden", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden"}`)) //nolint:errcheck // test server
		})
		_, err := resolveProjectGetRef(context.Background(), c, "gh/acme")
		require.EqualError(t, err, "your login does not have access to project gh/acme")
	})
	t.Run("a different reference", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ResolveProjectOutputBody{
				Project:   coreapi.Project{ID: ulidProjectWidgets, Name: "gh:mirrors:" + ulidOrgAcme, OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg},
				Reference: coreapi.ProjectReference{Host: "gh", Project: "other"},
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
		})
		_, err := resolveProjectGetRef(context.Background(), c, "gh/acme")
		require.EqualError(t, err, "core returned a different project reference")
	})
}

// Not parallel: runCoreCmd swaps the package-level core client.
// `entire project get gh/<owner>` resolves the mirror project, then shows it.
func TestProjectGet_MirrorReference(t *testing.T) {
	mirror := coreapi.Project{ID: ulidProjectWidgets, Name: "gh:mirrors:" + ulidOrgAcme, OwnerId: ulidOrgAcme, OwnerType: coreapi.ProjectOwnerTypeOrg, Region: "us"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case strings.HasSuffix(r.URL.Path, "/project-resolver/gh/acme"):
			body = &coreapi.ResolveProjectOutputBody{Project: mirror, Reference: coreapi.ProjectReference{Host: "gh", Project: "acme"}}
		case strings.HasSuffix(r.URL.Path, "/projects/"+ulidProjectWidgets):
			body = &mirror
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	out, _, err := runCoreCmd(t, newProjectCmd, srv.URL, "get", "gh/acme")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^ID +`+ulidProjectWidgets+`$`, out)
}
