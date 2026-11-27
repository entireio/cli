package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

func TestProjectRef(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		project coreapi.Project
		wantRef string
	}{
		{"native", coreapi.Project{Name: "rocket-lab", OwnerName: coreapi.NewOptString("acme-org")}, "et/rocket-lab"},
		{"native account-owned", coreapi.Project{Name: "acme", OwnerName: coreapi.NewOptString("github:alice")}, "et/acme"},
		{"org mirror", coreapi.Project{Name: "gh:mirrors:01KV7G8VR6X773V9KZQ36JCT4J", OwnerName: coreapi.NewOptString("widget-co")}, "gh/widget-co"},
		{"account mirror", coreapi.Project{Name: "gh:mirrors:01KSPWEZWS89N7M7Y97XMXP6JV", OwnerName: coreapi.NewOptString("alice")}, "gh/alice"},
		{"mirror without an owner name", coreapi.Project{Name: "gh:mirrors:01KSPWEZWS89N7M7Y97XMXP6JV"}, ""},
		{"other internal name", coreapi.Project{Name: "gl:mirrors:01KSPWEZWS89N7M7Y97XMXP6JV", OwnerName: coreapi.NewOptString("alice")}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantRef, projectRef(tt.project))
		})
	}
}

const projectRefTestCreatedAt = "2026-11-01T00:00:00Z"

func projectRefWire(id, name, ownerID, ownerName string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"ownerId":%q,"ownerName":%q,"ownerType":"org","region":"us","createdAt":%q}`,
		id, name, ownerID, ownerName, projectRefTestCreatedAt)
}

// Not parallel: swaps the package-level core client.
func execProjectCmd(t *testing.T, handler http.HandlerFunc, args ...string) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) { return coreapi.NewWithBearer(srv.URL, "tok") }
	t.Cleanup(func() { activeCoreClient = prev })
	root := newProjectCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	require.NoError(t, root.ExecuteContext(t.Context()))
	return out.String()
}

const (
	projectRefNativeID = "01M87624K2GNQY6Q7SX7HVHTW1"
	projectRefMirrorID = "01M0AAKP9F28YHHV647BF3NBWR"
	projectRefOwnerID  = "01M0AAKMQHT1PVD6J14P2648X8"
)

func projectRefListHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/v1") {
		case "/projects":
			fmt.Fprintf(w, `{"projects":[%s,%s]}`,
				projectRefWire(projectRefNativeID, "rocket-lab", projectRefOwnerID, "acme-org"),
				projectRefWire(projectRefMirrorID, "gh:mirrors:"+projectRefOwnerID, projectRefOwnerID, "widget-co"))
		case "/projects/" + projectRefMirrorID:
			fmt.Fprint(w, projectRefWire(projectRefMirrorID, "gh:mirrors:"+projectRefOwnerID, projectRefOwnerID, "widget-co"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// The table shows each project's reference and its owner's name, so what a
// reader sees is what they type.
func TestProjectList_ShowsTheReferenceAndOwnerName(t *testing.T) {
	out := execProjectCmd(t, projectRefListHandler(t), "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, out)
	assert.Equal(t, []string{"ID", "REF", "NAME", "OWNER-TYPE", "OWNER", "REGION"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{projectRefNativeID, "et/rocket-lab", "rocket-lab", "org", "acme-org", "us"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{projectRefMirrorID, "gh/widget-co", "gh:mirrors:" + projectRefOwnerID, "org", "widget-co", "us"}, strings.Fields(lines[2]))
}

// --json keeps every server key and adds "ref".
func TestProjectList_JSONAddsRef(t *testing.T) {
	out := execProjectCmd(t, projectRefListHandler(t), "list", "--json")
	var got []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "et/rocket-lab", got[0]["ref"])
	assert.Equal(t, "gh/widget-co", got[1]["ref"])
	assert.Equal(t, projectRefOwnerID, got[1]["ownerId"])
	assert.Equal(t, "gh:mirrors:"+projectRefOwnerID, got[1]["name"])
}

func TestProjectGet_ShowsTheReference(t *testing.T) {
	out := execProjectCmd(t, projectRefListHandler(t), "get", projectRefMirrorID)
	assert.Regexp(t, `(?m)^REF +gh/widget-co$`, out)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(execProjectCmd(t, projectRefListHandler(t), "get", projectRefMirrorID, "--json")), &got))
	assert.Equal(t, "gh/widget-co", got["ref"])
}

// trail list --project takes a project ID: Core names its reference, the list
// reads that project's collection, and the table prints the reference.
func TestProjectTrailList_AcceptsAProjectID(t *testing.T) {
	for _, tt := range []struct {
		name, projectName, ownerName, wantRef string
	}{
		{"native", "acme", "acme-org", "et/acme"},
		{"mirror", "gh:mirrors:" + projectRefOwnerID, "Widget-Co", "gh/widget-co"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, projectTrailTestProject, r.URL.Query().Get("projectId"))
				assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{Items: []api.ProjectTrail{projectTrailTestResource()}}))
			})
			core.projects = map[string]coreapi.Project{projectTrailTestProject: {ID: projectTrailTestProject, Name: tt.projectName, OwnerName: coreapi.NewOptString(tt.ownerName)}}
			out, _, err := executeProjectTrailTest(t, "list", "--project", projectTrailTestProject)
			require.NoError(t, err)
			assert.Equal(t, 1, core.getCalls)
			assert.Equal(t, []string{tt.wantRef, tt.wantRef}, core.resolved, "the ID check, then the route")
			lines := strings.Split(strings.TrimSpace(out), "\n")
			require.GreaterOrEqual(t, len(lines), 2, out)
			assert.Equal(t, tt.wantRef, strings.Fields(lines[1])[0])
		})
	}
}

// A project ID whose reference now resolves to another project is refused
// rather than read through.
func TestProjectTrailList_ProjectIDMustStillOwnItsReference(t *testing.T) {
	const other = "01M0AAKP9F28YHHV647BF3NBWR"
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {
		t.Error("must not read a collection")
	})
	core.projects = map[string]coreapi.Project{other: {ID: other, Name: "acme"}}
	_, _, err := executeProjectTrailTest(t, "list", "--project", other)
	require.ErrorContains(t, err, "now resolves to a different project")
}

func TestProjectTrailList_UnknownProjectID(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	_, _, err := executeProjectTrailTest(t, "list", "--project", projectTrailTestProject)
	require.ErrorIs(t, err, errProjectTrailProjectNotFound)
	assert.Equal(t, 0, core.resolveCalls)
}

func TestProjectTrailList_MirrorIDWithoutAReference(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	core.projects = map[string]coreapi.Project{projectTrailTestProject: {ID: projectTrailTestProject, Name: "gh:mirrors:" + projectRefOwnerID}}
	_, _, err := executeProjectTrailTest(t, "list", "--project", projectTrailTestProject)
	require.ErrorContains(t, err, "has no gh/<owner> or et/<project> reference")
}

// A bare name could mean et/<x> or gh/<x>: it is refused, naming both, with
// no network call.
func TestProjectTrailList_BareNameSuggestsReferences(t *testing.T) {
	for _, tt := range []struct{ ref, want string }{
		{"acme", `use et/acme for the Entire project named acme, or gh/acme for the GitHub owner acme`},
		{"gh:mirrors:" + projectRefOwnerID, "GitHub mirror project's internal name; pass gh/<owner>"},
		{"a_b", "use gh/<owner>, et/<project>, or a project ID"},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
			_, _, err := executeProjectTrailTest(t, "list", "--project", tt.ref)
			require.ErrorContains(t, err, tt.want)
			assert.Equal(t, 0, core.getCalls+core.resolveCalls)
		})
	}
}

// Against a branch's parent, a project ID compares with the parent's project
// ID, with no Core lookup; another project's ID is refused.
func TestProjectTrailShow_ProjectIDMatchesTheBranchParent(t *testing.T) {
	for _, tt := range []struct {
		name, project, wantErr string
	}{
		{"same project", projectTrailTestProject, ""},
		{"another project", projectRefNativeID, "branch's parent does not belong to --project"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
					ID: projectTrailTestChange, Number: 7, Branch: "feature/a",
					Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
						Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"},
				}}}))
			}))
			t.Cleanup(repoServer.Close)
			old := newTrailAPIClient
			newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
				return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-id", nil
			}
			t.Cleanup(func() { newTrailAPIClient = old })
			_, _, err := executeProjectTrailTest(t, "show", "--repo", "gh/acme/widget", "--branch", "feature/a", "--project", tt.project, "--json")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Zero(t, core.getCalls+core.resolveCalls)
		})
	}
}

// A project ID that is not the repository's own project is refused before
// anything is written.
func TestProjectTrailCreate_ProjectIDMustBeTheRepositorysProject(t *testing.T) {
	_, project := setupRepositoryTrailCreation(t, "gh", func(http.ResponseWriter, *http.Request) {
		t.Error("must not create")
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--title", "Intent", "--project", projectRefNativeID, "--repo", "gh/acme/widget")
	require.ErrorContains(t, err, "--project must match the scoped repository's project")
	assert.Zero(t, project.getCalls+project.resolveCalls)
}

// Core is asked for the canonical (uppercase) form of a lowercase ID.
func TestProjectTrailList_LowercaseProjectID(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{}))
	})
	core.projects = map[string]coreapi.Project{projectTrailTestProject: {ID: projectTrailTestProject, Name: "acme"}}
	_, _, err := executeProjectTrailTest(t, "list", "--project", strings.ToLower(projectTrailTestProject))
	require.NoError(t, err)
	assert.Equal(t, 1, core.getCalls)
}

func TestProjectTrailList_ProjectIDForbidden(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	core.getErr = &coreapi.ErrorModelStatusCode{StatusCode: http.StatusForbidden}
	_, _, err := executeProjectTrailTest(t, "list", "--project", projectTrailTestProject)
	require.ErrorIs(t, err, errProjectTrailProjectForbidden)
	assert.Equal(t, "your login does not have access to project "+projectTrailTestProject, err.Error())
}

// Logged out, a project ID gets the same one-line login hint as gh/ and et/.
func TestProjectTrailList_ProjectIDLoggedOut(t *testing.T) {
	setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	old := newProjectTrailCoreClient
	newProjectTrailCoreClient = func() (projectTrailCoreClient, error) { return nil, auth.ErrNotLoggedIn }
	t.Cleanup(func() { newProjectTrailCoreClient = old })
	_, errOut, err := executeProjectTrailTest(t, "list", "--project", projectTrailTestProject)
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
	assert.Contains(t, errOut, "Not logged in. Run 'entire login' to authenticate.")
}

// The project agent commands take a project ID too.
func TestProjectAgentTarget_AcceptsAProjectID(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	core.projects = map[string]coreapi.Project{projectTrailTestProject: {ID: projectTrailTestProject, Name: "acme"}}
	cmd := newProjectAgentCmd()
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.ParseFlags([]string{"--project", projectTrailTestProject}))
	target, err := resolveProjectAgentTarget(cmd)
	require.NoError(t, err)
	assert.Equal(t, "et/acme", target.label())
	assert.Equal(t, projectTrailTestProject, target.projectID)
}

// serve finds a worker's saved connection by reference, and resolving an ID
// needs project inspect a worker may not have, so it asks for the reference.
func TestProjectAgentServe_RefusesAProjectID(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {})
	cmd := newProjectAgentCmd()
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.ParseFlags([]string{"--project", projectTrailTestProject}))
	_, err := resolveProjectAgentServeTarget(cmd)
	require.ErrorContains(t, err, "needs --project as gh/<owner> or et/<project>")
	assert.Zero(t, core.getCalls+core.resolveCalls)
}
