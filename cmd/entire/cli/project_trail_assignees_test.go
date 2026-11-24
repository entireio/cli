package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	projectAssigneeAliceID = "01ARZ3NDEKTSV4RRFFQ69G5FA0"
	projectAssigneeBobID   = "01ARZ3NDEKTSV4RRFFQ69G5FA1"
)

// Not parallel: the helper replaces the active-core constructor. All identity
// lookups go to an httptest server, never the developer's login or keychain.
func setupProjectAssigneeCore(t *testing.T, identities map[string]string) {
	t.Helper()
	client, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		const prefix = "/api/v1/identity/handles/github/"
		assert.True(t, strings.HasPrefix(r.URL.Path, prefix), r.URL.Path)
		login := strings.TrimPrefix(r.URL.Path, prefix)
		id, ok := identities[strings.ToLower(login)]
		if !ok {
			writeNotFoundProblem(t, w)
			return
		}
		assert.NoError(t, printJSON(w, &coreapi.ResolvedIdentity{AccountId: id, Provider: "github", Handle: login}))
	})
	old := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) { return client, nil }
	t.Cleanup(func() { activeCoreClient = old })
}

func TestProjectTrailUpdateAssigneeLoginRoundTrip(t *testing.T) {
	setupProjectAssigneeCore(t, map[string]string{"alice": projectAssigneeAliceID})
	current := projectTrailTestResource()
	current.Assignees = []string{"ulid:" + projectAssigneeBobID}
	var writes int
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, projectTrailTestPath, r.URL.Path)
		if r.Method == http.MethodPatch {
			writes++
			assert.Equal(t, `W/"version"`, r.Header.Get("If-Match"))
			var patch api.ProjectTrailUpdateRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&patch))
			if assert.NotNil(t, patch.Assignees) {
				current.Assignees = *patch.Assignees
			}
		}
		w.Header().Set("ETag", `W/"version"`)
		assert.NoError(t, json.NewEncoder(w).Encode(current))
	})
	for _, flag := range []string{"--add-assignee", "--remove-assignee"} {
		out, _, err := executeProjectTrailTest(t, "update", projectTrailTestID, "--project", "gh/acme", flag, "Alice", "--json")
		require.NoError(t, err)
		var got api.ProjectTrail
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		want := []string{"ulid:" + projectAssigneeBobID}
		if flag == "--add-assignee" {
			want = append(want, "ulid:"+projectAssigneeAliceID)
		}
		require.Equal(t, want, got.Assignees)
	}
	require.Equal(t, 2, writes)
}

func TestProjectTrailAssigneeResolutionFailureRefusesPatch(t *testing.T) {
	setupProjectAssigneeCore(t, map[string]string{"no-account": "", "malformed": "not-an-account-id"})
	writes := 0
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		current := projectTrailTestResource()
		current.Assignees = []string{"ulid:" + projectAssigneeAliceID}
		w.Header().Set("ETag", `W/"version"`)
		assert.NoError(t, json.NewEncoder(w).Encode(current))
	})
	for _, login := range []string{"unknown", "no-account", "malformed", "ulid:bad", "@entire-username"} {
		_, _, err := executeProjectTrailTest(t, "update", projectTrailTestID, "--project", "gh/acme", "--remove-assignee", login)
		require.Error(t, err)
	}
	require.Zero(t, writes, "an unresolved identity must never result in a partial assignment update")
}

func TestProjectTrailAssigneesUseKnownIDsAndDeduplicateAliases(t *testing.T) {
	setupProjectAssigneeCore(t, map[string]string{"alice": projectAssigneeAliceID, "alias": projectAssigneeAliceID})
	current := api.ProjectTrail{
		Assignees:          []string{"OldLogin", "ulid:" + projectAssigneeBobID},
		AssigneeAccountIDs: []*string{stringPtr(projectAssigneeAliceID), stringPtr(projectAssigneeBobID)},
	}
	got, err := mergeProjectTrailAssignees(t.Context(), current, []string{"ALICE", "alias"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"ulid:" + projectAssigneeAliceID, "ulid:" + projectAssigneeBobID}, got)
	got, err = mergeProjectTrailAssignees(t.Context(), current, nil, []string{"alice", "ulid:" + strings.ToLower(projectAssigneeBobID)})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestProjectTrailAssigneeIDsNeedNoCoreClient(t *testing.T) {
	old := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		t.Error("canonical account references must not initialize the control-plane client")
		return nil, errors.New("unexpected control-plane client initialization")
	}
	t.Cleanup(func() { activeCoreClient = old })
	got, err := mergeProjectTrailAssignees(t.Context(), api.ProjectTrail{Assignees: []string{"ulid:" + projectAssigneeAliceID}}, nil, []string{"ulid:" + projectAssigneeAliceID})
	require.NoError(t, err)
	require.Empty(t, got)
}
