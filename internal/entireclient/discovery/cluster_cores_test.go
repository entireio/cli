package discovery

import (
	"testing"
	"time"
)

func TestClusterCores_RoundTripFresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.SetEntry("aws-us-east-2.entire.io", CoresEntry{CoreURLs: []string{"https://us.auth.entire.io", "https://eu.auth.entire.io"}})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores: %v", err)
	}

	cache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	entry, fresh, ok := cache.GetEntry("aws-us-east-2.entire.io")
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if !fresh {
		t.Fatal("expected freshly-set entry to be fresh")
	}
	if urls := entry.CoreURLs; len(urls) != 2 || urls[0] != "https://us.auth.entire.io" || urls[1] != "https://eu.auth.entire.io" {
		t.Fatalf("unexpected core URLs: %v", urls)
	}
}

func TestClusterCores_Miss(t *testing.T) {
	t.Parallel()
	cache, err := LoadClusterCores(t.TempDir())
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	if _, _, ok := cache.GetEntry("unknown.example"); ok {
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
	entry, fresh, ok := cache.GetEntry("old.example")
	if !ok {
		t.Fatal("stale entry should still report ok=true so callers can fall back to it")
	}
	if fresh {
		t.Fatal("entry older than the TTL should report fresh=false")
	}
	if urls := entry.CoreURLs; len(urls) != 1 || urls[0] != "https://core.example" {
		t.Fatalf("unexpected stale core URLs: %v", urls)
	}
}

func TestClusterCores_ModifyAccumulates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.SetEntry("a.example", CoresEntry{CoreURLs: []string{"https://core-a.example"}})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores a: %v", err)
	}
	// Second modify must see the first's write (single locked RMW) rather
	// than clobbering it.
	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.SetEntry("b.example", CoresEntry{CoreURLs: []string{"https://core-b.example"}})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores b: %v", err)
	}

	cache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	if _, _, ok := cache.GetEntry("a.example"); !ok {
		t.Fatal("first entry lost after second modify")
	}
	if _, _, ok := cache.GetEntry("b.example"); !ok {
		t.Fatal("second entry missing")
	}
}

func TestClusterCores_SetEntryCopiesSlice(t *testing.T) {
	t.Parallel()
	cache := make(ClusterCoresCache)
	urls := []string{"https://core.example"}
	cache.SetEntry("c.example", CoresEntry{CoreURLs: urls})
	urls[0] = "https://evil.example" // mutate caller's slice after SetEntry

	got, _, ok := cache.GetEntry("c.example")
	if !ok {
		t.Fatal("expected entry")
	}
	if got.CoreURLs[0] != "https://core.example" {
		t.Fatalf("SetEntry did not copy the slice; cache corrupted to %v", got.CoreURLs)
	}
}
