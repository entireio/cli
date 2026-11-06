package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectTrailPatchDistinguishesClearsFromOmission(t *testing.T) {
	t.Parallel()
	emptyBody := ""
	emptyAssignees := []string{}
	body, err := json.Marshal(ProjectTrailUpdateRequest{Body: &emptyBody, Assignees: &emptyAssignees})
	require.NoError(t, err)
	require.JSONEq(t, `{"body":"","assignees":[]}`, string(body))
	body, err = json.Marshal(ProjectTrailUpdateRequest{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
}

func TestRepoChangeParentIdentityRemainsSeparate(t *testing.T) {
	t.Parallel()
	var change TrailResource
	require.NoError(t, json.Unmarshal([]byte(`{
		"id":"change-id","number":7,"branch":"feature/a",
		"parent":{"id":"parent-id","number":42,"project_id":"project-id","host":"gh","project":"acme",
		"path":"/api/v1/gh/acme/trails/parent-id","jurisdiction":"eu","primary_processing_cell":"cell-eu"}
	}`), &change))
	require.Equal(t, "change-id", change.ID)
	require.Equal(t, 7, change.Number)
	require.NotNil(t, change.Parent)
	require.Equal(t, "parent-id", change.Parent.ID)
	require.Equal(t, 42, change.Parent.Number)
	require.Equal(t, "cell-eu", change.Parent.PrimaryProcessingCell)
	require.Equal(t, "eu", change.Parent.Jurisdiction)
}

func TestInitialChangeCreationIsFlat(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(ProjectTrailCreateRequest{
		Title: "Intent",
		Changes: []ChangeCreateRequest{{
			Title: "Work", BranchName: "feature/a", BranchAction: "link", RepositoryID: "repo-id",
		}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"title":"Intent","changes":[{"title":"Work","branchName":"feature/a","branchAction":"link","repositoryId":"repo-id"}]}`, string(body))
}

// A bodyless read or delete must go out without a body or a JSON Content-Type;
// only requests that carry a payload declare one.
func TestProjectTrailRequestSendsBodyOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	type seen struct {
		contentType string
		length      int64
	}
	got := map[string]seen{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.Method] = seen{contentType: r.Header.Get("Content-Type"), length: r.ContentLength}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client := NewClientWithBaseURL("token", server.URL)

	for _, tc := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodDelete, nil},
		{http.MethodPatch, ProjectTrailUpdateRequest{}},
	} {
		_, err := client.ProjectTrailRequest(t.Context(), tc.method, "/api/v1/gh/acme/trails/x", tc.body, nil, nil)
		require.NoError(t, err, tc.method)
	}
	require.Equal(t, seen{}, got[http.MethodGet])
	require.Equal(t, seen{}, got[http.MethodDelete])
	require.Equal(t, seen{contentType: "application/json", length: 2}, got[http.MethodPatch])
}
