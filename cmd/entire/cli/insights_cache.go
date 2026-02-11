package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// SessionFacet contains cached per-session analytics.
// This allows incremental updates - only new sessions need full analysis.
type SessionFacet struct {
	SessionID      string
	StartTime      time.Time
	Duration       time.Duration
	Tokens         int
	Messages       int
	FilesModified  int
	Agent          agent.AgentType
	Description    string
	ToolCounts     map[string]int
	HourlyActivity [24]int
}

// InsightsCache contains cached session facets.
type InsightsCache struct {
	Facets      map[string]SessionFacet `json:"facets"`
	LastUpdated time.Time               `json:"last_updated"`
}

const (
	// cacheTTL is how long cache entries are valid (30 days)
	cacheTTL = 30 * 24 * time.Hour
)

// getCachePath returns the path to the insights cache file for a repository.
func getCachePath(repoName string) (string, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return "", err
	}

	cacheDir := filepath.Join(repoRoot, ".entire", "insights-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Hash repo name for filename (simple hash to avoid special chars)
	hash := simpleHash(repoName)
	cachePath := filepath.Join(cacheDir, hash+".json")

	return cachePath, nil
}

// loadCache loads the insights cache for a repository.
// Returns nil if cache doesn't exist or is expired.
func loadCache(repoName string) (*InsightsCache, error) {
	cachePath, err := getCachePath(repoName)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Cache doesn't exist - return empty cache
			return &InsightsCache{
				Facets:      make(map[string]SessionFacet),
				LastUpdated: time.Now(),
			}, nil
		}
		return nil, fmt.Errorf("failed to read cache: %w", err)
	}

	var cache InsightsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// Cache is corrupt - return empty cache
		return &InsightsCache{
			Facets:      make(map[string]SessionFacet),
			LastUpdated: time.Now(),
		}, nil
	}

	// Check if cache is expired
	if time.Since(cache.LastUpdated) > cacheTTL {
		// Cache is too old - return empty cache
		return &InsightsCache{
			Facets:      make(map[string]SessionFacet),
			LastUpdated: time.Now(),
		}, nil
	}

	return &cache, nil
}

// saveCache saves the insights cache for a repository.
func saveCache(repoName string, cache *InsightsCache) error {
	cachePath, err := getCachePath(repoName)
	if err != nil {
		return err
	}

	cache.LastUpdated = time.Now()

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache: %w", err)
	}

	//nolint:gosec // G306: cache file is for user's own analysis, 0o644 is appropriate
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write cache: %w", err)
	}

	return nil
}

// simpleHash creates a simple hash of a string for use in filenames.
// Uses a basic algorithm to avoid collisions for typical repo names.
func simpleHash(s string) string {
	h := uint32(0)
	for i := range len(s) {
		h = h*31 + uint32(s[i])
	}
	return fmt.Sprintf("%08x", h)
}
