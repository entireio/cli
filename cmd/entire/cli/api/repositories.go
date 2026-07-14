package api

import (
	"context"
	"fmt"
	"net/url"
)

// Repository is a single entry returned by GET /api/v1/repositories.
// Only fields currently consumed by callers are decoded; extras are ignored.
type Repository struct {
	FullName        string `json:"full_name"`
	CheckpointCount int    `json:"checkpoint_count"`
}

// RepositoriesResponse is the envelope returned by GET /api/v1/repositories.
type RepositoriesResponse struct {
	Repositories []Repository `json:"repositories"`
}

type RepositorySort string

const (
	RepositorySortRecent RepositorySort = "recent"
	RepositorySortName   RepositorySort = "name"
)

// ListRepositories lists the authenticated user's repositories.
// An empty sort uses the server default.
func (c *Client) ListRepositories(ctx context.Context, sort RepositorySort) ([]Repository, error) {
	path := "/api/v1/repositories"
	if sort != "" {
		path += "?" + url.Values{"sort": []string{string(sort)}}.Encode()
	}

	resp, err := c.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return nil, err
	}

	var out RepositoriesResponse
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return out.Repositories, nil
}
