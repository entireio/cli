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
	resp, err := c.Get(ctx, fmt.Sprintf("/api/v1/trails/%s/%s/%s?limit=1",
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
