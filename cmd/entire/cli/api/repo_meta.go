package api

import (
	"context"
	"fmt"
	"net/url"
)

// RepoMeta is the part of GET /api/v1/repos/{repo_id} the CLI reads.
type RepoMeta struct {
	FullName      string `json:"full_name"`
	Permission    string `json:"permission"` // read | write | admin
	TrailsEnabled bool   `json:"trails_enabled"`
}

// FetchRepoMeta reads one repository's metadata from its entire-api cell.
func (c *Client) FetchRepoMeta(ctx context.Context, repoID string) (*RepoMeta, error) {
	resp, err := c.Get(ctx, "/api/v1/repos/"+url.PathEscape(repoID))
	if err != nil {
		return nil, fmt.Errorf("repo meta: %w", err)
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("repo meta: %w", err)
	}
	var out RepoMeta
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("repo meta: %w", err)
	}
	return &out, nil
}
