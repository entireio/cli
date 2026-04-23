package recap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// validCheckpointID matches the 12-hex-char format produced by the
// strategy package, plus a conservative superset for future compatibility.
// Anything else is rejected by Put/Get/GetAtVersion to prevent path
// traversal via malicious keys.
var validCheckpointID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// isValidKey returns true when checkpointID is safe to use as a path
// component. Empty strings and shell-metacharacter-containing strings
// return false.
func isValidKey(checkpointID string) bool {
	return validCheckpointID.MatchString(checkpointID)
}

// AnalysisCache stores CheckpointAnalysisResponse entries keyed by
// checkpoint ID. Invalidation is caller-driven via GetAtVersion: if the
// caller supplies a pipeline_version string, hits require an exact match.
//
// Thread-safe for concurrent readers. Concurrent writers are serialized
// via the internal mutex; for cross-process safety, callers should keep
// a single cache per process per repo.
type AnalysisCache struct {
	dir string
	mu  sync.RWMutex
}

// NewAnalysisCache creates (or opens) a cache at <baseDir>/entire-recap-cache/.
// The directory is created lazily on the first Put.
func NewAnalysisCache(baseDir string) (*AnalysisCache, error) {
	if baseDir == "" {
		return nil, errors.New("recap cache: empty base dir")
	}
	return &AnalysisCache{dir: filepath.Join(baseDir, "entire-recap-cache")}, nil
}

// Get returns the cached response for checkpointID, ignoring pipeline_version.
// Cached responses that carry no useful signal (no labels, no toolProfile,
// no skills, no agents) are treated as misses so a pending server-side
// analysis doesn't stay stuck as an empty render forever.
func (c *AnalysisCache) Get(checkpointID string) (*CheckpointAnalysisResponse, bool) {
	if !isValidKey(checkpointID) {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	resp, ok := c.read(checkpointID)
	if !ok || !hasUsefulSignal(resp) {
		return nil, false
	}
	return resp, true
}

// GetAtVersion returns a cached response only when its pipeline_version
// matches requiredVersion AND the response carries useful signal. Version
// mismatch is a miss, not an error.
func (c *AnalysisCache) GetAtVersion(checkpointID, requiredVersion string) (*CheckpointAnalysisResponse, bool) {
	if !isValidKey(checkpointID) {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	resp, ok := c.read(checkpointID)
	if !ok || !hasUsefulSignal(resp) {
		return nil, false
	}
	if requiredVersion != "" && resp.PipelineVersion != requiredVersion {
		return nil, false
	}
	return resp, true
}

// hasUsefulSignal returns true when any of the server-populated fields
// carries data. An "all zero" response means the server hasn't produced
// analysis yet — don't let that poison the cache.
func hasUsefulSignal(resp *CheckpointAnalysisResponse) bool {
	if resp == nil {
		return false
	}
	if len(resp.Extraction.Labels) > 0 {
		return true
	}
	if resp.ToolProfile != nil && resp.ToolProfile.Total > 0 {
		return true
	}
	if len(resp.SkillsUsed) > 0 || len(resp.MCPServersUsed) > 0 {
		return true
	}
	if len(resp.AgentsUsed) > 0 || len(resp.ModelsUsed) > 0 {
		return true
	}
	if resp.TotalTranscriptTokens > 0 || resp.TotalSteps > 0 {
		return true
	}
	return false
}

// Put writes the response to disk, overwriting any prior value. Responses
// without useful signal are refused so the cache never serves a hollow row
// back to the renderer — the next enrichment call tries the server again.
func (c *AnalysisCache) Put(checkpointID string, resp *CheckpointAnalysisResponse) error {
	if !isValidKey(checkpointID) {
		return fmt.Errorf("recap cache: invalid checkpoint id %q", checkpointID)
	}
	if !hasUsefulSignal(resp) {
		return nil // not an error; just a policy not-cached
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return fmt.Errorf("recap cache mkdir: %w", err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("recap cache marshal: %w", err)
	}
	target := c.path(checkpointID)
	tmp, err := os.CreateTemp(c.dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("recap cache tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("recap cache write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("recap cache tempfile close: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("recap cache chmod: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("recap cache rename: %w", err)
	}
	return nil
}

func (c *AnalysisCache) read(checkpointID string) (*CheckpointAnalysisResponse, bool) {
	data, err := os.ReadFile(c.path(checkpointID))
	if err != nil {
		return nil, false
	}
	var resp CheckpointAnalysisResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

func (c *AnalysisCache) path(checkpointID string) string {
	return filepath.Join(c.dir, checkpointID+".json")
}
