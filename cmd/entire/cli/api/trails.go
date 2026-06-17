package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// TrailsEnabled reports whether the trails feature is enabled for the repo on
// the API. It probes the trails list endpoint (limit=1): a 2xx response means
// trails are provisioned/enabled for the repo, while 404/403 (and any other
// non-2xx) mean they are not enabled or not accessible to this caller.
//
// Transport errors are returned to the caller (with enabled=false) so a
// "couldn't reach the API" outcome is distinguishable from a definitive
// "not enabled".
func (c *Client) TrailsEnabled(ctx context.Context, forge, owner, repo string) (bool, error) {
	resp, err := c.Get(ctx, fmt.Sprintf("/api/v1/trails/%s/%s/%s?limit=1",
		url.PathEscape(forge), url.PathEscape(owner), url.PathEscape(repo)))
	if err != nil {
		return false, fmt.Errorf("probe trails enablement: %w", err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so net/http can reuse the connection; the body is unused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // best-effort drain
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices, nil
}
