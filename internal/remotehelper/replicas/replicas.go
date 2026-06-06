// Package replicas manages the on-disk cache of Entire data-plane
// replicas and the per-invocation NodeConfig that drives the transport's
// warm/cold paths.
//
// A NodeConfig is built up front from the remote URL: the entry URL is
// derived from the cluster host, and the initial node list is whatever
// the local cache contains for (cluster, repo) — possibly empty on a
// cold invocation, in which case the transport's cold path probes the
// entry domain and populates the set from the first X-Entire-Replicas
// header it sees.
package replicas

import (
	"net/url"
	"strings"

	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// NodeConfig is the discovery-time state the transport needs: an
// entry-domain URL to use when the cached replica set is empty or
// exhausted, whatever replicas are currently cached, the cluster host
// for same-cluster redirect checks, and the repo path used as the
// cache key (empty when the URL has no path, in which case caching is
// disabled).
type NodeConfig struct {
	InitialNodes []string
	EntryURL     string
	ClusterHost  string
	RepoPath     string
}

// Caching reports whether this config participates in the persistent
// replica cache. False when the URL had no recognisable repo path to
// key on.
func (n NodeConfig) Caching() bool { return n.RepoPath != "" }

// Resolve builds the NodeConfig for this invocation. No network
// operations: the entry URL is derived from the cluster host in the
// remote URL and the initial node list is whatever the local replica
// cache contains.
func Resolve(parsedURL *url.URL) NodeConfig {
	clusterHost := parsedURL.Host
	repoPath := ExtractRepoPath(parsedURL.Path)
	entryURL := "https://" + clusterHost
	cached := LoadFresh(clusterHost, repoPath)
	if len(cached) > 0 {
		debuglog.Printf("loaded %d cached replicas for %s: %v", len(cached), repoPath, cached)
	} else {
		debuglog.Printf("no fresh replica cache for %s; will use entry URL on first request", repoPath)
	}
	return NodeConfig{
		InitialNodes: cached,
		EntryURL:     entryURL,
		ClusterHost:  clusterHost,
		RepoPath:     repoPath,
	}
}

// ExtractRepoPath builds the per-repo cache key from a remote URL's
// path. The path is used verbatim (minus leading/trailing slashes), so
// /et/<project>/<repo> (or legacy /git/<owner>/<repo>) and
// /gh/<owner>/<repo> live in distinct keyspaces — they're separate
// repos with potentially-different replica sets, even when the
// segments after the prefix match.
//
// entire://host/et/project/repo -> "et/project/repo"
// entire://host/gh/owner/repo   -> "gh/owner/repo"
func ExtractRepoPath(urlPath string) string {
	return strings.Trim(urlPath, "/")
}

// LoadFresh returns the cached replica set for (host, repoPath) if one
// exists and is fresh, otherwise nil.
func LoadFresh(host, repoPath string) []string {
	cache, err := discovery.LoadCache(discovery.DefaultCacheDir())
	if err != nil || cache == nil {
		return nil
	}
	nodes, fresh := cache.GetRepoNodes(host, repoPath)
	if !fresh {
		return nil
	}
	return nodes
}

// Persist stores the given replica set in the on-disk cache. Best
// effort: cache write failures are logged but never fatal. The
// load-mutate-save runs under ModifyCache's single lock so concurrent
// clone/fetch/push processes don't clobber each other's entries.
func Persist(host, repoPath string, nodes []string) {
	if host == "" || repoPath == "" || len(nodes) == 0 {
		return
	}
	if err := discovery.ModifyCache(discovery.DefaultCacheDir(), func(cache discovery.ClusterCache) error {
		cache.SetRepoNodes(host, repoPath, nodes, discovery.DefaultTTL)
		return nil
	}); err != nil {
		debuglog.Printf("cache save failed for %s: %v", repoPath, err)
	}
}

// Invalidate removes the cached entry for (host, repoPath). Best
// effort: failures are logged but never fatal. Runs under ModifyCache's
// lock so it doesn't race a concurrent Persist.
func Invalidate(host, repoPath string) {
	if host == "" || repoPath == "" {
		return
	}
	if err := discovery.ModifyCache(discovery.DefaultCacheDir(), func(cache discovery.ClusterCache) error {
		cache.InvalidateRepo(host, repoPath)
		return nil
	}); err != nil {
		debuglog.Printf("cache invalidate failed for %s: %v", repoPath, err)
	}
}
