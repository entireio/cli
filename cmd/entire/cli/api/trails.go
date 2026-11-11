package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// TrailsEnabled probes trail availability: 2xx=true, 403/404/410=false,
// everything else ambiguous.
func (c *Client) TrailsEnabled(ctx context.Context, forge, owner, repo string) (bool, error) {
	resp, err := c.Get(ctx, fmt.Sprintf("/api/v1/trails/%s/%s/%s?per_page=1",
		url.PathEscape(forge), url.PathEscape(owner), url.PathEscape(repo)))
	if err != nil {
		return false, fmt.Errorf("probe trails enablement: %w", err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so net/http can reuse the connection; the body is unused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // best-effort drain
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return true, nil
	}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return false, nil
	default:
		return false, fmt.Errorf("probe trails enablement: unexpected status %s", resp.Status)
	}
}

// RepoTrailsEnabled reads a repository's Trails setting from the cell that
// holds it (GET /api/v1/repos/{repo_id}). known is false when the response
// carries no setting, so callers can tell "off" from "not reported".
func (c *Client) RepoTrailsEnabled(ctx context.Context, repoID string) (enabled, known bool, err error) {
	resp, err := c.Get(ctx, "/api/v1/repos/"+url.PathEscape(repoID))
	if err != nil {
		return false, false, fmt.Errorf("read repository settings: %w", err)
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return false, false, fmt.Errorf("read repository settings: %w", err)
	}
	var repo struct {
		TrailsEnabled *bool `json:"trails_enabled"`
	}
	if err := DecodeJSON(resp, &repo); err != nil {
		return false, false, fmt.Errorf("decode repository settings: %w", err)
	}
	if repo.TrailsEnabled == nil {
		return false, false, nil
	}
	return *repo.TrailsEnabled, true, nil
}
