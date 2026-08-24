package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// TrailsEnabled probes change availability: 2xx=true, 403/410=false,
// everything else ambiguous. A 404 from the canonical /api/v1/changes path
// falls back to the legacy /api/v1/trails path before concluding disabled —
// see probeTrailsEnabledPath.
func (c *Client) TrailsEnabled(ctx context.Context, forge, owner, repo string) (bool, error) {
	enabled, notFound, err := c.probeTrailsEnabledPath(ctx, "/api/v1/changes/%s/%s/%s?pageSize=1", forge, owner, repo)
	if err != nil || !notFound {
		return enabled, err
	}
	// TODO(ENT-1891): remove this fallback once every entire-api deployment
	// serves /api/v1/changes. Until then, a CLI released ahead of the 1b
	// server rollout would see the new route 404 on a repo that actually has
	// trails enabled, and — since the result is cached for ~1h
	// (settings.ClonePreferences.TrailsEnabledCheckedAt) — the whole trails
	// family would silently self-disable for that long.
	enabled, _, err = c.probeTrailsEnabledPath(ctx, "/api/v1/trails/%s/%s/%s?pageSize=1", forge, owner, repo)
	return enabled, err
}

// probeTrailsEnabledPath issues one enablement probe against pathFormat (a
// fmt string with 3 %s verbs for forge/owner/repo). notFound is true only for
// a 404, distinguishing "this route doesn't exist" (worth retrying against a
// different route) from a definitive 403/410 negative.
func (c *Client) probeTrailsEnabledPath(ctx context.Context, pathFormat, forge, owner, repo string) (enabled, notFound bool, err error) {
	resp, err := c.Get(ctx, fmt.Sprintf(pathFormat,
		url.PathEscape(forge), url.PathEscape(owner), url.PathEscape(repo)))
	if err != nil {
		return false, false, fmt.Errorf("probe trails enablement: %w", err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so net/http can reuse the connection; the body is unused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // best-effort drain
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return true, false, nil
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return false, true, nil
	case http.StatusForbidden, http.StatusGone:
		return false, false, nil
	default:
		return false, false, fmt.Errorf("probe trails enablement: unexpected status %s", resp.Status)
	}
}
