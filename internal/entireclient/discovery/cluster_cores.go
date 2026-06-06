package discovery

import (
	"path/filepath"
	"time"
)

const (
	clusterCoresFileName = "cluster_cores.json"

	// ClusterCoresTTL bounds how long a cached cluster→core_urls mapping is
	// treated as fresh. Which control plane(s) front a data-plane cluster is
	// near-static infra — once a cluster is homed to a core it stays — so a
	// long TTL is fine. On expiry we re-fetch /.well-known and only fall back
	// to the stale entry if that fetch fails.
	ClusterCoresTTL = 24 * time.Hour
)

// ClusterCoresCache maps a cluster host to the control-plane core URLs that
// front it, memoizing /.well-known/entire-cluster.json so routine git ops
// don't re-fetch it every time. It stores only the objective cluster→core
// fact — never which local account to authenticate as. The account is chosen
// fresh on every operation from the user's contexts, so a multi-account user
// is never silently pinned to one identity.
//
// Cache file: cluster_cores.json in the cache dir (alongside nodes.json).
// Safe to delete by hand to force re-discovery.
type ClusterCoresCache map[string]*CoresEntry

// CoresEntry is one cluster's cached core URLs plus when they were fetched.
// Freshness is fetched_at + ClusterCoresTTL, computed at read time so a TTL
// change re-interprets existing entries without a migration.
type CoresEntry struct {
	CoreURLs  []string  `json:"core_urls"`
	FetchedAt time.Time `json:"fetched_at"`
}

// LoadClusterCores reads the cluster→cores cache. A missing or corrupt file
// yields an empty cache. Unlocked read; use ModifyClusterCores for a
// read-modify-write sequence.
func LoadClusterCores(cacheDir string) (ClusterCoresCache, error) {
	return readClusterCoresNoLock(filepath.Join(cacheDir, clusterCoresFileName))
}

// ModifyClusterCores atomically applies fn to the cluster→cores cache under a
// single exclusive flock.
func ModifyClusterCores(cacheDir string, fn func(ClusterCoresCache) error) error {
	return modifyCacheFile(cacheDir, clusterCoresFileName, readClusterCoresNoLock, writeClusterCoresNoLock, fn)
}

func readClusterCoresNoLock(path string) (ClusterCoresCache, error) {
	cache := make(ClusterCoresCache)
	err := loadCacheFile(path, &cache, func() ClusterCoresCache { return make(ClusterCoresCache) })
	return cache, err
}

func writeClusterCoresNoLock(path string, cache ClusterCoresCache) error {
	return writeCacheFile(path, cache)
}

// Get returns a cluster's cached core URLs, whether the entry is still fresh,
// and whether it exists at all. A present-but-stale entry returns
// (urls, false, true) so callers can attempt a re-fetch yet fall back to the
// stale URLs if that fetch fails.
func (c ClusterCoresCache) Get(cluster string) (urls []string, fresh, ok bool) {
	entry := c[cluster]
	if entry == nil || len(entry.CoreURLs) == 0 {
		return nil, false, false
	}
	return entry.CoreURLs, time.Now().Before(entry.FetchedAt.Add(ClusterCoresTTL)), true
}

// Set records a cluster's core URLs, stamping the fetch time to now. The
// slice is copied so later mutation by the caller can't corrupt the cache.
func (c ClusterCoresCache) Set(cluster string, urls []string) {
	c[cluster] = &CoresEntry{
		CoreURLs:  append([]string(nil), urls...),
		FetchedAt: time.Now(),
	}
}
