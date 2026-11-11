package cli

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentConnectionAllowsRepoOnlyIdentity(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	target := &projectAgentTarget{client: api.NewClientWithBaseURL("admin-token", "https://cell.example.com"), host: "et", project: "acme", projectID: saTestProjectID}
	require.NoError(t, saveProjectAgentConnection(target))
	c, err := loadProjectAgentConnection("et", "acme")
	require.NoError(t, err)
	require.Equal(t, target.projectID, c.ProjectID)
	root, err := userdirs.ConfigRootForRead()
	require.NoError(t, err)
	b, err := osroot.ReadFileNoFollow(root, projectAgentConnectionName("et", "acme"))
	require.NoError(t, err)
	require.NotContains(t, string(b), "admin-token")
	other, err := loadProjectAgentConnection("gh", "acme")
	require.NoError(t, err)
	require.Nil(t, other)
	old := newProjectTrailCellClient
	t.Cleanup(func() { newProjectTrailCellClient = old })
	called := false
	newProjectTrailCellClient = func(_ context.Context, _ bool, cell *auth.CellTarget) (*api.Client, error) {
		called = true
		require.Equal(t, "https://cell.example.com", cell.BaseURL)
		return api.NewClientWithBaseURL("service-account-token", cell.BaseURL), nil
	}
	// No Core mock is supplied: resolving the project through Core would fail.
	cmd := newProjectAgentServeCmd()
	cmd.Flags().String("project", "et/acme", "")
	cmd.SetContext(t.Context())
	resolved, err := resolveProjectAgentServeTarget(cmd)
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, target.projectID, resolved.projectID)
}

func TestProjectAgentConnectionRejectsCorruption(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	target := &projectAgentTarget{client: api.NewClientWithBaseURL("test", "https://cell.example.com"), host: "et", project: "acme", projectID: saTestProjectID}
	require.NoError(t, saveProjectAgentConnection(target))
	root, err := userdirs.ConfigRoot()
	require.NoError(t, err)
	name := projectAgentConnectionName("et", "acme")
	for _, body := range []string{
		`{`,
		`{"host":"gh","project":"acme","project_id":"` + saTestProjectID + `","api_url":"https://cell.example.com"}`,
		`{"host":"et","project":"acme","project_id":"` + saTestProjectID + `","api_url":"https://secret@cell.example.com"}`,
	} {
		require.NoError(t, osroot.WriteFile(root, name, []byte(body), 0o600))
		_, err := loadProjectAgentConnection("et", "acme")
		require.Error(t, err)
	}
}
