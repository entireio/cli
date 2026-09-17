package coreapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ProjectResolution is Core's public, forge-qualified project lookup. Name in
// Project is an internal storage name for GitHub projects: use Reference for
// public API routes. Kept handwritten until this operation enters our ogen spec.
type ProjectResolution struct {
	Project *struct {
		ID                    string `json:"id"`
		Region                string `json:"region"`
		PrimaryProcessingCell string `json:"primaryProcessingCell"`
		APIURL                string `json:"apiUrl"`
	} `json:"project"`
	Reference struct {
		Host    string `json:"host"`
		Project string `json:"project"`
	} `json:"reference"`
}

// ResolveProject uses the same bearer source and cross-jurisdiction transport
// as generated operations, including ENTIRE_TOKEN and the active auth context.
func (c *Client) ResolveProject(ctx context.Context, host, project string) (*ProjectResolution, error) {
	u := *c.requestURL(ctx)
	u.Path += "/projects/resolve/" + host + "/" + project
	u.RawPath = c.requestURL(ctx).EscapedPath() + "/projects/resolve/" + url.PathEscape(host) + "/" + url.PathEscape(project)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("resolve project request: %w", err)
	}
	if err := c.securityBearerAuth(ctx, OperationName("resolveProject"), req); err != nil {
		return nil, fmt.Errorf("resolve project authentication: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolve project: %w", err)
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var problem struct {
			Detail string `json:"detail"`
		}
		if err := decoder.Decode(&problem); err != nil {
			return nil, fmt.Errorf("resolve project %s/%s: HTTP %d (invalid error response): %w", host, project, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("resolve project %s/%s: HTTP %d %s", host, project, resp.StatusCode, problem.Detail)
	}
	var out ProjectResolution
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode project resolution: %w", err)
	}
	if out.Project == nil || out.Project.ID == "" {
		return nil, fmt.Errorf("resolve project %s/%s: missing project identity", host, project)
	}
	return &out, nil
}
