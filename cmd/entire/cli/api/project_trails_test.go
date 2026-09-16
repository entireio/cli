package api

import (
	"encoding/json"
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
		"parent":{"id":"parent-id","number":42,"projectId":"project-id","host":"gh","project":"acme",
		"path":"/api/v1/gh/acme/trails/parent-id","jurisdiction":"eu","primaryProcessingCell":"cell-eu"}
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
			TrailCreateRequest: TrailCreateRequest{Title: "Work", BranchName: "feature/a", BranchAction: "link"},
			RepositoryID:       "repo-id",
		}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"title":"Intent","changes":[{"title":"Work","branchName":"feature/a","branchAction":"link","repositoryId":"repo-id"}]}`, string(body))
}
