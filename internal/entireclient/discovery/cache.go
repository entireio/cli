package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
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

// LoadCache reads the node cache from disk. Returns an empty cache if the file
// does not exist. This is an unlocked read — fine for read-only callers; use
// ModifyCache for read-modify-write sequences.
func LoadCache(cacheDir string) (ClusterCache, error) {
	return readCacheNoLock(filepath.Join(cacheDir, cacheFileName))
}

// ModifyCache atomically applies fn to the node cache under a single
// exclusive flock — load, mutate, and write all happen with the lock held.
// Use this for any read-modify-write sequence, so concurrent writers
// (e.g. two parallel clone/fetch/push processes updating nodes.json) can't
// lose each other's entries.
func ModifyCache(cacheDir string, fn func(ClusterCache) error) error {
	return modifyCacheFile(cacheDir, cacheFileName, readCacheNoLock, writeCacheNoLock, fn)
}

func readCacheNoLock(path string) (ClusterCache, error) {
	cache := make(ClusterCache)
	err := loadCacheFile(path, &cache, func() ClusterCache { return make(ClusterCache) })
	return cache, err
}

func writeCacheNoLock(path string, cache ClusterCache) error {
	return writeCacheFile(path, cache)
}

// --- shared cache-file primitives (used by every cache file in this
// package: nodes.json, cluster_cores.json) ---

// withCacheFileLock ensures cacheDir exists, takes the exclusive flock for
// the named cache file, and runs fn with the file's path.
func withCacheFileLock(cacheDir, fileName string, fn func(path string) error) error {
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	path := filepath.Join(cacheDir, fileName)
	unlock, err := lockCache(path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(path)
}

// modifyCacheFile runs a load → mutate → write cycle for one cache file with
// the file's flock held throughout, so concurrent processes filling the same
// entry don't clobber each other.
func modifyCacheFile[T any](cacheDir, fileName string, read func(string) (T, error), write func(string, T) error, fn func(T) error) error {
	return withCacheFileLock(cacheDir, fileName, func(path string) error {
		c, err := read(path)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		return write(path, c)
	})
}

func lockCache(path string) (func(), error) {
	fl := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	locked, err := fl.TryLockContext(ctx, lockRetry)
	if err != nil {
		return nil, fmt.Errorf("acquire cache lock: %w", err)
	}
	if !locked {
		return nil, errors.New("timeout acquiring cache lock")
	}
	return func() { _ = fl.Unlock() }, nil //nolint:errcheck // unlock failure is non-fatal
}

// loadCacheFile reads path and unmarshals it into dst. A missing file leaves
// dst at its caller-initialized (empty) value; a corrupt file resets dst via
// newEmpty so a damaged cache self-heals on the next write instead of wedging
// callers. Returns an error only on a genuine read failure. Returning error
// (rather than the cache value itself) keeps this generic helper off the
// ireturn linter while still sharing the read/unmarshal logic across caches.
func loadCacheFile[T any](path string, dst *T, newEmpty func() T) error {
	data, exists, err := readCacheBytes(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if json.Unmarshal(data, dst) != nil {
		*dst = newEmpty() // corrupt → start fresh
	}
	return nil
}

// writeCacheFile marshals v and writes it atomically (tmp + rename).
func writeCacheFile[T any](path string, v T) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	return writeCacheBytesAtomic(path, data)
}

// cacheRoot returns the root over the directory holding path, and path's name
// inside it. Anchored on the file's own directory so the explicit-cacheDir
// callers (tests, XDG_CACHE_HOME) take the same code path as the default.
//
// Cache file names are fixed today (nodes.json, cluster_cores.json,
// api_discovery.json), but the entries inside them are keyed by cluster slug and
// jurisdiction — the kind of value that becomes a filename the moment someone
// shards the cache per cluster. The root means that change cannot reach outside
// the cache directory.
func cacheRoot(path string) (*os.Root, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve cache path: %w", err)
	}
	root, err := osroot.Shared(filepath.Dir(abs))
	if err != nil {
		if os.IsNotExist(err) {
			// Unwrapped so readCacheBytes can classify a cold cache.
			return nil, "", err //nolint:wrapcheck // see comment
		}
		return nil, "", fmt.Errorf("open cache dir: %w", err)
	}
	return root, filepath.Base(abs), nil
}

// readCacheBytes returns the file contents and whether the file exists. A
// missing file is (nil, false, nil); other read errors propagate.
func readCacheBytes(path string) ([]byte, bool, error) {
	root, name, err := cacheRoot(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No cache directory means no cache, the same as no file. Callers
			// treat both as a cold cache and refetch.
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := osroot.ReadFile(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read cache: %w", err)
	}
	return data, true, nil
}

// writeCacheBytesAtomic writes data via a tmp file + rename so a reader never
// observes a half-written cache.
func writeCacheBytesAtomic(path string, data []byte) error {
	root, name, err := cacheRoot(path)
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := osroot.WriteFile(root, tmp, data, 0600); err != nil {
		return fmt.Errorf("write cache tmp: %w", err)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp) //nolint:errcheck // cleanup best-effort
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
