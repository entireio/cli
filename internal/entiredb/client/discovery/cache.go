package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const (
	cacheFileName = "nodes.json"
	lockTimeout   = 5 * time.Second
	lockRetry     = 100 * time.Millisecond

	// DefaultTTL is the cache TTL for replica sets discovered via info/refs.
	DefaultTTL = 24 * time.Hour
)

// ClusterCache is the top-level cache structure, keyed by cluster host.
type ClusterCache map[string]*ClusterEntry

// ClusterEntry holds cached data for a single cluster.
type ClusterEntry struct {
	Nodes          []string              `json:"nodes"`
	NodesExpiresAt time.Time             `json:"nodes_expires_at"`
	Repos          map[string]*RepoEntry `json:"repos,omitempty"`
}

// RepoEntry caches the hosting nodes for a single repository.
type RepoEntry struct {
	Nodes     []string  `json:"nodes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DefaultCacheDir returns ~/.cache/entire, respecting XDG_CACHE_HOME.
func DefaultCacheDir() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "entire")
	}
	home, _ := os.UserHomeDir() //nolint:errcheck // best-effort default
	return filepath.Join(home, ".cache", "entire")
}

// LoadCache reads the node cache from disk. Returns an empty cache if the file
// does not exist.
func LoadCache(cacheDir string) (ClusterCache, error) {
	path := filepath.Join(cacheDir, cacheFileName)

	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return make(ClusterCache), nil
		}
		return nil, fmt.Errorf("read cache: %w", err)
	}

	var cache ClusterCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// Corrupted cache — start fresh.
		return make(ClusterCache), nil //nolint:nilerr // intentional: treat corrupt cache as empty
	}
	return cache, nil
}

// SaveCache writes the cache to disk atomically.
func SaveCache(cacheDir string, cache ClusterCache) error {
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	path := filepath.Join(cacheDir, cacheFileName)

	fl := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	locked, err := fl.TryLockContext(ctx, lockRetry)
	if err != nil {
		return fmt.Errorf("acquire cache lock: %w", err)
	}
	if !locked {
		return errors.New("timeout acquiring cache lock")
	}
	defer fl.Unlock() //nolint:errcheck // unlock failure is non-fatal

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write cache tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}

// GetClusterNodes returns the cached cluster nodes. The second return value
// indicates whether the cache entry is fresh (not expired).
func (c ClusterCache) GetClusterNodes(cluster string) ([]string, bool) {
	entry := c[cluster]
	if entry == nil || len(entry.Nodes) == 0 {
		return nil, false
	}
	return entry.Nodes, time.Now().Before(entry.NodesExpiresAt)
}

// SetClusterNodes stores cluster nodes with the given TTL.
func (c ClusterCache) SetClusterNodes(cluster string, nodes []string, ttl time.Duration) {
	entry := c[cluster]
	if entry == nil {
		entry = &ClusterEntry{}
		c[cluster] = entry
	}
	entry.Nodes = nodes
	entry.NodesExpiresAt = time.Now().Add(ttl)
}

// GetRepoNodes returns cached hosting nodes for a repo. The second return
// value indicates freshness.
func (c ClusterCache) GetRepoNodes(cluster, repoPath string) ([]string, bool) {
	entry := c[cluster]
	if entry == nil || entry.Repos == nil {
		return nil, false
	}
	repo := entry.Repos[repoPath]
	if repo == nil || len(repo.Nodes) == 0 {
		return nil, false
	}
	return repo.Nodes, time.Now().Before(repo.ExpiresAt)
}

// SetRepoNodes caches hosting nodes for a specific repo.
func (c ClusterCache) SetRepoNodes(cluster, repoPath string, nodes []string, ttl time.Duration) {
	entry := c[cluster]
	if entry == nil {
		entry = &ClusterEntry{}
		c[cluster] = entry
	}
	if entry.Repos == nil {
		entry.Repos = make(map[string]*RepoEntry)
	}
	entry.Repos[repoPath] = &RepoEntry{
		Nodes:     nodes,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// InvalidateRepo removes the cached repo entry.
func (c ClusterCache) InvalidateRepo(cluster, repoPath string) {
	if entry := c[cluster]; entry != nil && entry.Repos != nil {
		delete(entry.Repos, repoPath)
	}
}

// InvalidateCluster removes all cached data for a cluster.
func (c ClusterCache) InvalidateCluster(cluster string) {
	delete(c, cluster)
}
