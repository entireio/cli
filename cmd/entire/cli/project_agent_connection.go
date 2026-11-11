package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/spf13/cobra"
)

// Setup resolves this as an administrator. Repo-only service accounts cannot
// resolve the project shell in Core; do not grant project read just for routing.
// This operator-owned file contains no credential and is never repo-controlled.
type projectAgentConnection struct {
	ProjectID string `json:"project_id"`
	Host      string `json:"host"`
	Project   string `json:"project"`
	APIURL    string `json:"api_url"`
}

func projectAgentConnectionName(host, project string) string {
	return fmt.Sprintf("agent-serve/projects/%x.json", sha256.Sum256([]byte(host+"/"+project)))
}

func saveProjectAgentConnection(t *projectAgentTarget) error {
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open connection configuration: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, "agent-serve/projects", 0o700); err != nil {
		return fmt.Errorf("create connection configuration directory: %w", err)
	}
	c := projectAgentConnection{ProjectID: t.projectID, Host: t.host, Project: t.project, APIURL: t.client.BaseURL()}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode connection: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, projectAgentConnectionName(t.host, t.project), b, 0o600); err != nil {
		return fmt.Errorf("save connection: %w", err)
	}
	return nil
}

func loadProjectAgentConnection(host, project string) (*projectAgentConnection, error) {
	root, err := userdirs.ConfigRootForRead()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // No setup on this machine.
	}
	if err != nil {
		return nil, fmt.Errorf("open connection configuration: %w", err)
	}
	b, err := osroot.ReadFileNoFollow(root, projectAgentConnectionName(host, project))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // No setup for this project.
	}
	if err != nil {
		return nil, fmt.Errorf("read connection: %w", err)
	}
	var c projectAgentConnection
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode connection: %w", err)
	}
	if c.Host != host || c.Project != project || !looksLikeULID(c.ProjectID) {
		return nil, errors.New("invalid saved project connection; rerun project agent setup")
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return nil, errors.New("invalid saved API URL; rerun project agent setup")
	}
	return &c, nil
}

func resolveProjectAgentServeTarget(cmd *cobra.Command) (*projectAgentTarget, error) {
	host, project, err := resolveTrailProjectReference(cmd)
	if err != nil {
		return nil, err
	}
	c, err := loadProjectAgentConnection(host, project)
	if err != nil {
		return nil, err
	}
	if c == nil {
		// Credentials that already have project inspect may use normal discovery.
		t, err := resolveProjectAgentTarget(cmd)
		if err != nil {
			return nil, fmt.Errorf("no saved worker connection; run project agent setup on this machine as an administrator: %w", err)
		}
		return t, nil
	}
	client, err := newProjectTrailCellClient(cmd.Context(), insecureHTTPRequested(cmd), &auth.CellTarget{BaseURL: c.APIURL})
	if err != nil {
		return nil, fmt.Errorf("open saved worker connection: %w", err)
	}
	return &projectAgentTarget{client: client, projectID: c.ProjectID, host: host, project: project, base: "/api/v1/" + url.PathEscape(host) + "/" + url.PathEscape(project)}, nil
}
