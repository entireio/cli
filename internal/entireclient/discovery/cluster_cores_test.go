package discovery

import (
	"testing"
	"time"
)

func TestClusterCores_RoundTripFresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.Set("aws-us-east-2.entire.io", []string{"https://us.auth.entire.io", "https://eu.auth.entire.io"})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores: %v", err)
	}

	cache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	urls, fresh, ok := cache.Get("aws-us-east-2.entire.io")
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if !fresh {
		t.Fatal("expected freshly-set entry to be fresh")
	}
	if len(urls) != 2 || urls[0] != "https://us.auth.entire.io" || urls[1] != "https://eu.auth.entire.io" {
		t.Fatalf("unexpected core URLs: %v", urls)
	}
}

func TestClusterCores_Miss(t *testing.T) {
	t.Parallel()
	cache, err := LoadClusterCores(t.TempDir())
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	if _, _, ok := cache.Get("unknown.example"); ok {
		t.Fatal("expected miss for unknown cluster")
	}
}

func TestClusterCores_StaleEntryStillReturned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write an entry whose fetch time is older than the TTL.
	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c["old.example"] = &CoresEntry{
			CoreURLs:  []string{"https://core.example"},
			FetchedAt: time.Now().Add(-ClusterCoresTTL - time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores: %v", err)
	}

	cache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	urls, fresh, ok := cache.Get("old.example")
	if !ok {
		t.Fatal("stale entry should still report ok=true so callers can fall back to it")
	}
	if fresh {
		t.Fatal("entry older than the TTL should report fresh=false")
	}
	if len(urls) != 1 || urls[0] != "https://core.example" {
		t.Fatalf("unexpected stale core URLs: %v", urls)
	}
}

func TestClusterCores_ModifyAccumulates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.Set("a.example", []string{"https://core-a.example"})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores a: %v", err)
	}
	// Second modify must see the first's write (single locked RMW) rather
	// than clobbering it.
	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.Set("b.example", []string{"https://core-b.example"})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores b: %v", err)
	}

	cache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	if _, _, ok := cache.Get("a.example"); !ok {
		t.Fatal("first entry lost after second modify")
	}
	if _, _, ok := cache.Get("b.example"); !ok {
		t.Fatal("second entry missing")
	}
}

func TestClusterCores_SetCopiesSlice(t *testing.T) {
	t.Parallel()
	cache := make(ClusterCoresCache)
	urls := []string{"https://core.example"}
	cache.Set("c.example", urls)
	urls[0] = "https://evil.example" // mutate caller's slice after Set

	got, _, ok := cache.Get("c.example")
	if !ok {
		t.Fatal("expected entry")
	}
	if got[0] != "https://core.example" {
		t.Fatalf("Set did not copy the slice; cache corrupted to %v", got)
	}
}
