package discovery

import (
	"path/filepath"
	"strings"
	"time"
)

const (
	// clusterRegistryFileName caches "core X lists cluster Y" answers. Its own
	// file (not cluster_cores.json) because it caches a different fact keyed on
	// a different thing: a (core, cluster) pair rather than a host, and
	// membership in a core's registry rather than a host's advertised issuers.
	clusterRegistryFileName = "cluster_registry.json"

	// ClusterRegistryTTL bounds how long a confirmed (core, cluster) pairing is
	// treated as fresh. Which clusters a core fronts is near-static infra — a
	// cluster is onboarded once and stays — so the same long TTL the
	// host→cores cache uses applies, and it is what keeps routine git ops off
	// the control plane's critical path.
	ClusterRegistryTTL = 24 * time.Hour
)

// ClusterRegistryCache memoizes POSITIVE cluster-registry lookups: a core
// confirmed that it fronts a cluster host. It exists so routine git operations
// (every clone/fetch/push resolves credentials) don't each pay a synchronous
// control-plane round trip, and so a brief core outage doesn't break git
// against a cluster we already know is legitimate.
//
// Only successes are stored, and deliberately so. A "not registered" answer is
// the security-relevant one — it is what refuses to hand credentials to an
// unknown host — and a registry error is transient by nature; caching either
// would trade a fresh check for a stale refusal. So a host the core does not
// list is re-checked every single time, and only the boring "yes, still one of
// mine" answer is allowed to go stale.
//
// It stores no credential and no identity — just the objective fact that a core
// listed a cluster, keyed by both, so switching logins never reuses another
// core's answer.
//
// Cache file: cluster_registry.json in the cache dir (alongside nodes.json).
// Safe to delete by hand to force re-verification.
type ClusterRegistryCache map[string]*ClusterRegistryEntry

// ClusterRegistryEntry records when a (core, cluster) pairing was last
// confirmed. Freshness is VerifiedAt + ClusterRegistryTTL, computed at read
// time so a TTL change re-interprets existing entries without a migration.
type ClusterRegistryEntry struct {
	VerifiedAt time.Time `json:"verified_at"`
}

// LoadClusterRegistry reads the (core, cluster)→verified cache. A missing or
// corrupt file yields an empty cache. Unlocked read; use ModifyClusterRegistry
// for a read-modify-write sequence.
func LoadClusterRegistry(cacheDir string) (ClusterRegistryCache, error) {
	return readClusterRegistryNoLock(filepath.Join(cacheDir, clusterRegistryFileName))
}

// ModifyClusterRegistry atomically applies fn to the (core, cluster)→verified
// cache under a single exclusive flock, so two concurrent git processes filling
// the same entry can't clobber each other.
func ModifyClusterRegistry(cacheDir string, fn func(ClusterRegistryCache) error) error {
	return modifyCacheFile(cacheDir, clusterRegistryFileName, readClusterRegistryNoLock, writeClusterRegistryNoLock, fn)
}

func readClusterRegistryNoLock(path string) (ClusterRegistryCache, error) {
	cache := make(ClusterRegistryCache)
	err := loadCacheFile(path, &cache, func() ClusterRegistryCache { return make(ClusterRegistryCache) })
	return cache, err
}

func writeClusterRegistryNoLock(path string, cache ClusterRegistryCache) error {
	return writeCacheFile(path, cache)
}

// Verified reports whether coreOrigin was recently confirmed to front
// clusterHost. A missing or expired entry is false — the caller then asks the
// core and, on a yes, calls MarkVerified.
func (c ClusterRegistryCache) Verified(coreOrigin, clusterHost string) bool {
	entry := c[clusterRegistryKey(coreOrigin, clusterHost)]
	if entry == nil {
		return false
	}
	return time.Now().Before(entry.VerifiedAt.Add(ClusterRegistryTTL))
}

// MarkVerified records that coreOrigin currently fronts clusterHost, stamped
// now. Only ever called for a confirmed match — see the type's doc comment for
// why misses and errors are never stored.
func (c ClusterRegistryCache) MarkVerified(coreOrigin, clusterHost string) {
	c[clusterRegistryKey(coreOrigin, clusterHost)] = &ClusterRegistryEntry{VerifiedAt: time.Now()}
}

// clusterRegistryKey folds a (core, cluster) pair into one cache key. Both
// halves are normalised the way their comparisons are elsewhere — trailing
// slashes and case are insignificant in a core URL, DNS is case-insensitive —
// so a host typed in mixed case hits the entry its lowercase twin wrote. The
// separator is a space: neither a URL origin nor a validated cluster host can
// contain one, so the two halves can never be confused for each other.
func clusterRegistryKey(coreOrigin, clusterHost string) string {
	core := strings.ToLower(strings.TrimRight(strings.TrimSpace(coreOrigin), "/"))
	host := strings.ToLower(strings.TrimSpace(clusterHost))
	return core + " " + host
}
