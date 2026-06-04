package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModifyCache_AtomicReadModifyWrite(t *testing.T) {
	dir := t.TempDir()

	// First mutation persists.
	if err := ModifyCache(dir, func(c ClusterCache) error {
		c.SetRepoNodes("host", "/r1", []string{"n1"}, time.Hour)
		return nil
	}); err != nil {
		t.Fatalf("ModifyCache r1: %v", err)
	}
	// Second mutation sees the first's write (read-modify-write under one
	// lock) and adds to it rather than clobbering.
	if err := ModifyCache(dir, func(c ClusterCache) error {
		c.SetRepoNodes("host", "/r2", []string{"n2"}, time.Hour)
		return nil
	}); err != nil {
		t.Fatalf("ModifyCache r2: %v", err)
	}

	cache, err := LoadCache(dir)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if n, ok := cache.GetRepoNodes("host", "/r1"); !ok || len(n) != 1 || n[0] != "n1" {
		t.Fatalf("r1 entry lost: %v ok=%v", n, ok)
	}
	if n, ok := cache.GetRepoNodes("host", "/r2"); !ok || len(n) != 1 || n[0] != "n2" {
		t.Fatalf("r2 entry missing: %v ok=%v", n, ok)
	}

	// A fn error aborts the write — the mutation must not persist.
	if err := ModifyCache(dir, func(c ClusterCache) error {
		c.SetRepoNodes("host", "/r3", []string{"n3"}, time.Hour)
		return errors.New("boom")
	}); err == nil {
		t.Fatal("expected ModifyCache to return the fn error")
	}
	cache, err = LoadCache(dir)
	if err != nil {
		t.Fatalf("LoadCache after abort: %v", err)
	}
	if _, ok := cache.GetRepoNodes("host", "/r3"); ok {
		t.Fatal("aborted mutation must not have persisted")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cache := make(ClusterCache)
	cache.SetClusterNodes("rc.partial.to", []string{
		"https://node1.rc.partial.to",
		"https://node2.rc.partial.to",
	}, 24*time.Hour)
	cache.SetRepoNodes("rc.partial.to", "alice/repo", []string{
		"https://node1.rc.partial.to",
	}, 24*time.Hour)

	if err := SaveCache(dir, cache); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	loaded, err := LoadCache(dir)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}

	nodes, fresh := loaded.GetClusterNodes("rc.partial.to")
	if !fresh {
		t.Fatal("expected cluster nodes to be fresh")
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d cluster nodes, want 2", len(nodes))
	}

	repoNodes, fresh := loaded.GetRepoNodes("rc.partial.to", "alice/repo")
	if !fresh {
		t.Fatal("expected repo nodes to be fresh")
	}
	if len(repoNodes) != 1 || repoNodes[0] != "https://node1.rc.partial.to" {
		t.Fatalf("unexpected repo nodes: %v", repoNodes)
	}
}

func TestCacheExpiry(t *testing.T) {
	cache := make(ClusterCache)
	cache.SetClusterNodes("x.com", []string{"https://n1.x.com"}, -1*time.Second)
	cache.SetRepoNodes("x.com", "a/b", []string{"https://n1.x.com"}, -1*time.Second)

	_, fresh := cache.GetClusterNodes("x.com")
	if fresh {
		t.Error("expected expired cluster nodes")
	}

	_, fresh = cache.GetRepoNodes("x.com", "a/b")
	if fresh {
		t.Error("expected expired repo nodes")
	}
}

func TestCacheMiss(t *testing.T) {
	cache := make(ClusterCache)

	nodes, fresh := cache.GetClusterNodes("nope.com")
	if fresh || nodes != nil {
		t.Error("expected miss for unknown cluster")
	}

	nodes, fresh = cache.GetRepoNodes("nope.com", "a/b")
	if fresh || nodes != nil {
		t.Error("expected miss for unknown repo")
	}
}

func TestCacheInvalidation(t *testing.T) {
	cache := make(ClusterCache)
	cache.SetClusterNodes("x.com", []string{"https://n1.x.com"}, 24*time.Hour)
	cache.SetRepoNodes("x.com", "a/b", []string{"https://n1.x.com"}, 24*time.Hour)

	cache.InvalidateRepo("x.com", "a/b")
	_, fresh := cache.GetRepoNodes("x.com", "a/b")
	if fresh {
		t.Error("repo should be invalidated")
	}

	_, fresh = cache.GetClusterNodes("x.com")
	if !fresh {
		t.Error("cluster nodes should still be fresh")
	}

	cache.InvalidateCluster("x.com")
	_, fresh = cache.GetClusterNodes("x.com")
	if fresh {
		t.Error("cluster should be invalidated")
	}
}

func TestLoadCacheMissingFile(t *testing.T) {
	dir := t.TempDir()
	cache, err := LoadCache(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("LoadCache on missing dir: %v", err)
	}
	if len(cache) != 0 {
		t.Fatalf("expected empty cache, got %d entries", len(cache))
	}
}

func TestLoadCacheCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCache(dir, make(ClusterCache)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	cache, err := LoadCache(dir)
	if err != nil {
		t.Fatalf("expected graceful handling of corrupt cache, got: %v", err)
	}
	if len(cache) != 0 {
		t.Fatal("expected empty cache from corrupt file")
	}
}
