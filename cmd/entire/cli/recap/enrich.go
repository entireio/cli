package recap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Enricher fetches server-authoritative checkpoint analysis (labels, tool
// profile) and projects it onto RecapCheckpoint values. Failures are
// non-fatal: the returned checkpoint keeps its local fields and Source
// remains SourceLocal so the caller can render "labels unavailable."
type Enricher struct {
	client *api.Client
	cache  *AnalysisCache
}

// NewEnricher wires an api client and cache. Either may be nil: a nil
// client disables fetching (all checkpoints stay local); a nil cache
// disables caching (every call hits the api).
func NewEnricher(client *api.Client, cache *AnalysisCache) *Enricher {
	return &Enricher{client: client, cache: cache}
}

// EnrichCheckpoint returns a copy of cp with Labels/ToolProfile populated
// from the server when available. cp.Repo must be set to "<org>/<name>".
// Network/decode failures yield the input checkpoint; the error return is
// reserved for future fatal conditions (signature stability for callers).
//
//nolint:unparam // error reserved for future fatal conditions; current failures are non-fatal by design
func (e *Enricher) EnrichCheckpoint(ctx context.Context, cp RecapCheckpoint) (RecapCheckpoint, error) {
	if e == nil || e.client == nil {
		return cp, nil
	}
	cpID := cp.ID.String()

	// Cache lookup (version-agnostic — we trust any cached copy for the
	// session; if pipeline_version bumps mid-session the next run will refetch).
	if e.cache != nil {
		if resp, ok := e.cache.Get(cpID); ok {
			return merge(cp, resp), nil
		}
	}

	resp, err := e.fetch(ctx, cp.Repo, cpID)
	if err != nil {
		logging.Debug(ctx, "recap: enrichment failed",
			"checkpoint", cpID, "error", err.Error())
		return cp, nil // non-fatal
	}
	if e.cache != nil {
		if putErr := e.cache.Put(cpID, resp); putErr != nil {
			logging.Debug(ctx, "recap: cache put failed",
				"checkpoint", cpID, "error", putErr.Error())
		}
	}
	return merge(cp, resp), nil
}

// fetch issues a GET to /<org>/<repo>/checkpoints/<id>/analysis and
// decodes the body as CheckpointAnalysisResponse. The endpoint is defined
// server-side at api/src/routes/cache.ts:4585 (repoRoutes).
func (e *Enricher) fetch(ctx context.Context, repo, checkpointID string) (*CheckpointAnalysisResponse, error) {
	if repo == "" {
		return nil, errors.New("enrich: empty repo")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("enrich: invalid repo %q (want \"org/name\")", repo)
	}
	path := fmt.Sprintf("/%s/%s/checkpoints/%s/analysis",
		url.PathEscape(parts[0]),
		url.PathEscape(parts[1]),
		url.PathEscape(checkpointID))
	resp, err := e.client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("enrich get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("enrich: http %d (body read failed: %w)", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("enrich: http %d: %s", resp.StatusCode, string(body))
	}
	var out CheckpointAnalysisResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("enrich decode: %w", err)
	}
	return &out, nil
}

// merge projects server response fields onto the local checkpoint. Unknown
// labels are dropped so a server-side taxonomy expansion doesn't crash rendering.
func merge(cp RecapCheckpoint, resp *CheckpointAnalysisResponse) RecapCheckpoint {
	out := cp
	for _, lbl := range resp.Extraction.Labels {
		if IsCanonicalLabel(lbl) {
			out.Labels = append(out.Labels, lbl)
		}
	}
	if resp.ToolProfile != nil {
		tp := ToolProfile{
			Total:      resp.ToolProfile.Total,
			Categories: make(map[string]ToolCategoryMetrics, len(resp.ToolProfile.Categories)),
		}
		for k, v := range resp.ToolProfile.Categories {
			tp.Categories[k] = v
		}
		out.ToolProfile = &tp
	}
	out.Source = SourceMixed
	return out
}
