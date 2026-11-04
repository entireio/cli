package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

// serveProjectTrailUpdate answers the working project reads and the PATCH,
// recording whether the PATCH was sent.
func serveProjectTrailUpdate(t *testing.T, patched *bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == projectTrailTestPath {
			*patched = true
			assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
			return
		}
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func serveWidgetChangeDetail(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails/gh/acme/widget/7", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Status: "open",
			Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
				Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}}))
	}
}

// A change selector reaches its parent project trail, whose fields every
// change shares; the update says so before writing.
func TestProjectTrailUpdateThroughChangeNamesTheParent(t *testing.T) {
	for _, args := range [][]string{
		{"update", "widget/7", "--project", "gh/acme", "--title", "New intent"},
		{"update", "widget/7", "--project", "gh/acme", "--status", "closed"},
	} {
		t.Run(args[4], func(t *testing.T) {
			var patched bool
			setupProjectTrailTest(t, serveProjectTrailUpdate(t, &patched))
			setupWorkingRepoClient(t, serveWidgetChangeDetail(t))

			out, errOut, err := executeProjectTrailTest(t, args...)

			require.NoError(t, err)
			require.True(t, patched)
			require.Contains(t, errOut, `Updating trail #42 "Project intent" (parent of widget/7); these fields apply to the whole trail, not just this change.`)
			require.NotContains(t, out, "Updating trail", "the notice stays off stdout")
		})
	}
}

// With no selector, update follows the branch's parent: the case agents hit
// most, since the injected guidance says to run `entire trail update`.
func TestProjectTrailUpdateFollowingBranchNamesTheParent(t *testing.T) {
	var patched bool
	setupProjectTrailTest(t, serveProjectTrailUpdate(t, &patched))
	parent := &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
		Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		change := api.TrailResource{ID: projectTrailTestChange, Number: 7, Status: "open", Branch: "feature/work", Parent: parent}
		switch r.URL.Path {
		case "/api/v1/trails/gh/acme/widget":
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{change}, Total: 1}))
		case "/api/v1/trails/gh/acme/widget/7":
			assert.NoError(t, json.NewEncoder(w).Encode(change))
		default:
			t.Errorf("unexpected repo request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	_, errOut, err := executeProjectTrailTest(t, "update", "--repo", "gh/acme/widget", "--branch", "feature/work", "--body", "New description")

	require.NoError(t, err)
	require.True(t, patched)
	require.Contains(t, errOut, `Updating trail #42 "Project intent" (parent of feature/work)`)
}

// A local preflight failure (here assignee resolution) means no write
// happens, so the notice must not claim one is under way.
func TestProjectTrailUpdateAssigneeFailurePrintsNoNotice(t *testing.T) {
	var patched bool
	setupProjectTrailTest(t, serveProjectTrailUpdate(t, &patched))
	setupWorkingRepoClient(t, serveWidgetChangeDetail(t))
	// Stub the control plane so the failure never depends on a real login or
	// ENTIRE_TOKEN in the environment.
	old := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) { return nil, errors.New("control plane unavailable") }
	t.Cleanup(func() { activeCoreClient = old })

	_, errOut, err := executeProjectTrailTest(t, "update", "widget/7", "--project", "gh/acme", "--add-assignee", "@bob")

	require.ErrorContains(t, err, "resolve assignee")
	require.False(t, patched)
	require.NotContains(t, errOut, "Updating trail")
}

// syncBuffer lets the test server read what the command has written so far.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The notice is a warning before the write, not a report after it: it must
// already be on stderr when the PATCH reaches the server.
func TestProjectTrailUpdateNoticePrecedesPatch(t *testing.T) {
	var errOut syncBuffer
	var stderrAtPatch string
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == projectTrailTestPath {
			stderrAtPatch = errOut.String()
			assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
			return
		}
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	setupWorkingRepoClient(t, serveWidgetChangeDetail(t))
	cmd := newTrailCmdForMode(true)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"update", "widget/7", "--project", "gh/acme", "--title", "New intent"})

	require.NoError(t, cmd.Execute())
	require.Contains(t, stderrAtPatch, `Updating trail #42 "Project intent" (parent of widget/7)`)
}

// Naming the project trail itself is already explicit: no notice, even when
// --branch picks the working change.
func TestProjectTrailUpdateByProjectSelectorHasNoNotice(t *testing.T) {
	for _, args := range [][]string{
		{"update", "42", "--project", "gh/acme", "--title", "New intent"},
		{"update", "42", "--project", "gh/acme", "--repo", "gh/acme/widget", "--branch", "feature/work", "--title", "New intent"},
	} {
		t.Run(args[len(args)-3], func(t *testing.T) {
			var patched bool
			setupProjectTrailTest(t, serveProjectTrailUpdate(t, &patched))
			setupWorkingRepoClient(t, serveWidgetChangeDetail(t))

			_, errOut, err := executeProjectTrailTest(t, args...)

			require.NoError(t, err)
			require.True(t, patched)
			require.NotContains(t, errOut, "Updating trail")
		})
	}
}

func TestProjectTrailParentUpdateNotice(t *testing.T) {
	t.Parallel()
	change := &projectTrailCurrentChange{ID: projectTrailTestChange, Repo: "widget"}
	trail := api.ProjectTrail{ID: projectTrailTestID, Number: 42, Title: "Intent\x1b]0;x\x07",
		Changes: []api.ChangeSummary{{ID: projectTrailTestChange, Number: 7}}}
	require.Equal(t, `Updating trail #42 "Intent" (parent of widget/7); these fields apply to the whole trail, not just this change.`,
		projectTrailParentUpdateNotice(trail, change, ""), "repository falls back to the resolved name; title sanitized")

	forged := trail
	forged.Title = "Intent\nUpdated trail #1 \u202Eevil"
	forged.Changes = []api.ChangeSummary{{ID: projectTrailTestChange, Branch: "feat\t\u2066x\n"}}
	got := projectTrailParentUpdateNotice(forged, change, "")
	require.NotContains(t, got, "\n", "server text cannot forge a further stderr line")
	require.NotContains(t, got, "\u202E", "bidi controls cannot reorder the notice")
	require.NotContains(t, got, "\u2066")
	require.Contains(t, got, `"Intent Updated trail #1 evil" (parent of feat x )`)

	trail.Number = 0
	trail.Changes = []api.ChangeSummary{{ID: projectTrailTestChange, Branch: "feature/work"}}
	require.Contains(t, projectTrailParentUpdateNotice(trail, change, ""), "Updating trail "+projectTrailTestID+` "Intent" (parent of feature/work)`)

	trail.Changes = nil
	require.Contains(t, projectTrailParentUpdateNotice(trail, change, ""), "(parent of the selected change)")
	require.Contains(t, projectTrailParentUpdateNotice(trail, change, "widget/7"), "(parent of widget/7)", "a change selector names the change as given")
}

func TestIsProjectTrailSelector(t *testing.T) {
	t.Parallel()
	for selector, want := range map[string]bool{"": false, "widget/7": false, "42": true, projectTrailTestID: true} {
		require.Equal(t, want, isProjectTrailSelector(selector), selector)
	}
}

// trail list and timeline take -n like trail finding does.
func TestProjectTrailPaginationLimitShorthand(t *testing.T) {
	t.Parallel()
	root := newTrailCmdForMode(true)
	for _, name := range []string{"list", "timeline"} {
		cmd, _, err := root.Find([]string{name})
		require.NoError(t, err)
		flag := cmd.Flags().Lookup("limit")
		require.NotNil(t, flag, name)
		require.Equal(t, "n", flag.Shorthand, name)
	}
}
