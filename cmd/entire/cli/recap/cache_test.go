package recap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCache_RoundTrip(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	c, err := NewAnalysisCache(tmp)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := c.Get("aa11bb22cc33")
	if ok {
		t.Errorf("fresh cache should be empty, got %+v", got)
	}

	resp := &CheckpointAnalysisResponse{
		PipelineVersion: "2026-04-10.v3",
		Extraction:      CheckpointExtraction{Labels: []string{"feature_build"}},
	}
	if err := c.Put("aa11bb22cc33", resp); err != nil {
		t.Fatal(err)
	}

	got, ok = c.Get("aa11bb22cc33")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Extraction.Labels[0] != "feature_build" {
		t.Errorf("Labels = %v", got.Extraction.Labels)
	}
}

func TestCache_InvalidatesOnPipelineVersionChange(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	c, err := NewAnalysisCache(tmp)
	if err != nil {
		t.Fatal(err)
	}
	old := &CheckpointAnalysisResponse{PipelineVersion: "2026-04-01.v1"}
	if err := c.Put("aa11bb22cc33", old); err != nil {
		t.Fatal(err)
	}

	// Reader with newer required version treats the stale entry as a miss.
	got, ok := c.GetAtVersion("aa11bb22cc33", "2026-04-10.v3")
	if ok {
		t.Errorf("expected miss on version mismatch, got %+v", got)
	}

	// Same version is a hit.
	got, ok = c.GetAtVersion("aa11bb22cc33", "2026-04-01.v1")
	if !ok {
		t.Error("expected hit for matching version")
	}
	_ = got
}

func TestCache_CreatesDirLazily(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "entire-recap-cache")
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatal("cache dir should not exist yet")
	}
	c, err := NewAnalysisCache(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("xx", &CheckpointAnalysisResponse{PipelineVersion: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache dir should exist after Put: %v", err)
	}
}

func TestCache_RejectsPathTraversalKeys(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	c, err := NewAnalysisCache(tmp)
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"../escape", "a/b", "", "with space", "a\x00b"}
	for _, k := range bad {
		if err := c.Put(k, &CheckpointAnalysisResponse{PipelineVersion: "v"}); err == nil {
			t.Errorf("Put(%q) should have errored", k)
		}
		if _, ok := c.Get(k); ok {
			t.Errorf("Get(%q) should be a miss", k)
		}
	}
}

func TestCache_AtomicWriteLeavesNoPartialFiles(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	c, err := NewAnalysisCache(tmp)
	if err != nil {
		t.Fatal(err)
	}
	resp := &CheckpointAnalysisResponse{PipelineVersion: "v"}
	if err := c.Put("aa11bb22cc33", resp); err != nil {
		t.Fatal(err)
	}
	// After a successful Put, only the final file should be present in the dir.
	entries, err := os.ReadDir(filepath.Join(tmp, "entire-recap-cache"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "aa11bb22cc33.json" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only aa11bb22cc33.json; got %v", names)
	}
}
