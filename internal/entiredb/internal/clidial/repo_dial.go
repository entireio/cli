package clidial

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"

	"github.com/entireio/cli/internal/entiredb/client"
	"github.com/entireio/cli/internal/entiredb/client/clilogin"
	"github.com/entireio/cli/internal/entiredb/client/discovery"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
)

// errResolveCacheMiss is returned from inside the cached-host attempt when
// Resolve fails on a cached replica, signalling the caller to invalidate the
// cache entry and fall back to the configured host. It must not escape
// ConnectForRepo.
var errResolveCacheMiss = errors.New("resolve failed on cached replica")

// ConnectForRepo opens an authenticated client for a repo-scoped operation
// and hands fn a configured *client.Client plus the resolved repo ULID.
//
// cluster is the data-plane host parsed out of the user's positional argument
// (e.g. "aws-us-east-2.entire.io"). It's used as the dial target and as the
// STS audience, eliminating the ambient-cluster footgun where the audience
// silently fell back to entire.io.
//
// The helper consults the on-disk replica cache (shared with
// git-remote-entire), keyed by cluster host + repo path, and dials a hosting
// replica when known. Cache miss / stale entry / cached host unreachable all
// fall back to the cluster's entry URL; the fallback Resolve sets
// includeReplicas so the next call gets affinity.
//
// ENTIRE_TOKEN, when set, bypasses the credential store and is sent on every
// dial — useful when the keyring is broken or for CI workloads injecting a
// JWT directly.
func ConnectForRepo(cfg cliauth.Config, cluster, repoPath string, fn func(c *client.Client, repoID string) error) error {
	host, baseURL, err := normaliseCluster(cluster)
	if err != nil {
		return err
	}

	connect, err := repoConnectFunc(cfg, host, baseURL)
	if err != nil {
		return err
	}

	cacheDir := discovery.DefaultCacheDir()

	if cachedURI, ok := pickCachedReplicaURI(cacheDir, host, repoPath); ok {
		err := connect(cachedURI, func(c *client.Client) error {
			repoID, rerr := c.RepoResolve(context.Background(), repoPath)
			if rerr != nil {
				return fmt.Errorf("%w: %w", errResolveCacheMiss, rerr)
			}
			return fn(c, repoID)
		})
		if !errors.Is(err, errResolveCacheMiss) {
			if err != nil {
				return fmt.Errorf("dial cached replica %s: %w", cachedURI, err)
			}
			return nil
		}
		invalidateCachedReplicas(cacheDir, host, repoPath)
	}

	if err := connect(baseURL, func(c *client.Client) error {
		repoID, replicas, rerr := c.RepoResolveWithReplicas(context.Background(), repoPath)
		if rerr != nil {
			return fmt.Errorf("resolve %q: %w", repoPath, rerr)
		}
		if len(replicas) > 0 {
			storeCachedReplicas(cacheDir, host, repoPath, replicas)
		}
		return fn(c, repoID)
	}); err != nil {
		return fmt.Errorf("dial %s: %w", baseURL, err)
	}
	return nil
}

// normaliseCluster accepts a bare host ("cluster.example") or a full URL
// ("https://cluster.example") and returns the host (used for cache keys) and
// a full https base URL (used for dialing). http:// is preserved when
// explicitly given; that keeps the local-dev story working.
func normaliseCluster(cluster string) (host, baseURL string, err error) {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return "", "", errors.New("cluster is empty")
	}
	if !strings.Contains(cluster, "://") {
		return cluster, "https://" + cluster, nil
	}
	u, perr := url.Parse(cluster)
	if perr != nil {
		return "", "", fmt.Errorf("parse cluster %q: %w", cluster, perr)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("cluster URL %q is missing a host", cluster)
	}
	return u.Host, strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}

// repoConnectFunc returns a connector for a repo-scoped op. ENTIRE_TOKEN, when
// set, attaches the literal JWT and bypasses the credential store. Otherwise
// the helper resolves a context for the explicit cluster host (the
// `cluster_contexts[host]` binding when set, else /.well-known discovery and
// auto-bind), uses its keychain slot, and wires up the auto-refresh HTTP
// client.
func repoConnectFunc(cfg cliauth.Config, clusterHost, clusterBaseURL string) (func(target string, fn client.ConnFunc) error, error) {
	httpClient := cliauth.NewHTTPClient(cfg.SkipTLSVerify)
	if envToken := os.Getenv("ENTIRE_TOKEN"); envToken != "" {
		return func(target string, fn client.ConnFunc) error {
			return client.ConnectRepoWithAuth(target, envToken, fn, client.WithRepoHTTPClient(httpClient))
		}, nil
	}

	c, err := cliauth.ResolveClusterContext(context.Background(), cfg, clusterHost, httpClient)
	if err != nil {
		return nil, err
	}

	authCfg := client.AuthRefreshConfig{
		KeyringService: c.KeychainService,
		BaseURL:        clusterBaseURL,
		Username:       c.Handle,
		CoreBaseURL:    c.CoreURL,
		HTTPClient:     httpClient,
		ClientID:       clilogin.DefaultClientID,
	}
	return func(target string, fn client.ConnFunc) error {
		// target may be a per-replica node URI; the scoped-JWT audience must
		// be the canonical cluster URL (the host the server trusts), not the
		// replica we happen to dial. Pin it explicitly via WithScopedExchange.
		return client.ConnectRepoWithRefresh(target, authCfg, fn,
			client.WithRepoHTTPClient(httpClient),
			client.WithScopedExchange(c.CoreURL, clusterBaseURL),
		)
	}, nil
}

// pickCachedReplicaURI reads the disk cache and returns a random fresh
// replica HTTP base URI. The second return value is false when the cache is
// missing, stale, or contains no URI.
func pickCachedReplicaURI(cacheDir, host, repoPath string) (string, bool) {
	cache, err := discovery.LoadCache(cacheDir)
	if err != nil {
		return "", false
	}
	uris, fresh := cache.GetRepoNodes(host, repoPath)
	if !fresh || len(uris) == 0 {
		return "", false
	}
	return uris[rand.IntN(len(uris))], true
}

// storeCachedReplicas writes a fresh replica set into the disk cache,
// best-effort. Errors are ignored — a missing cache write only costs the
// next invocation one extra round-trip through the LB.
func storeCachedReplicas(cacheDir, host, repoPath string, replicas []string) {
	cache, err := discovery.LoadCache(cacheDir)
	if err != nil {
		return
	}
	cache.SetRepoNodes(host, repoPath, replicas, discovery.DefaultTTL)
	_ = discovery.SaveCache(cacheDir, cache) //nolint:errcheck // best-effort
}

// invalidateCachedReplicas drops the cache entry for a repo, best-effort.
func invalidateCachedReplicas(cacheDir, host, repoPath string) {
	cache, err := discovery.LoadCache(cacheDir)
	if err != nil {
		return
	}
	cache.InvalidateRepo(host, repoPath)
	_ = discovery.SaveCache(cacheDir, cache) //nolint:errcheck // best-effort
}
