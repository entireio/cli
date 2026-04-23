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
func (c *AnalysisCache) Get(checkpointID string) (*CheckpointAnalysisResponse, bool) {
	if !isValidKey(checkpointID) {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.read(checkpointID)
}

// GetAtVersion returns a cached response only when its pipeline_version
// matches requiredVersion. An empty requiredVersion disables the check
// (equivalent to Get). Version mismatch is a miss, not an error.
func (c *AnalysisCache) GetAtVersion(checkpointID, requiredVersion string) (*CheckpointAnalysisResponse, bool) {
	if !isValidKey(checkpointID) {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	resp, ok := c.read(checkpointID)
	if !ok {
		return nil, false
	}
	if requiredVersion != "" && resp.PipelineVersion != requiredVersion {
		return nil, false
	}
	return resp, true
}

// Put writes the response to disk, overwriting any prior value.
func (c *AnalysisCache) Put(checkpointID string, resp *CheckpointAnalysisResponse) error {
	if !isValidKey(checkpointID) {
		return fmt.Errorf("recap cache: invalid checkpoint id %q", checkpointID)
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
