package discovery

import "testing"

// TestAPICores_SeparateFileFromClusterCores: the API and cluster caches share a
// type + TTL but must live in distinct files, so a cluster host and an API host
// with the same name can't clobber each other.
func TestAPICores_SeparateFileFromClusterCores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyClusterCores(dir, func(c ClusterCoresCache) error {
		c.Set("shared.example", []string{"https://cluster-core.example"})
		return nil
	}); err != nil {
		t.Fatalf("ModifyClusterCores: %v", err)
	}
	if err := ModifyAPICores(dir, func(c ClusterCoresCache) error {
		c.Set("shared.example", []string{"https://api-core.example"})
		return nil
	}); err != nil {
		t.Fatalf("ModifyAPICores: %v", err)
	}

	clusterCache, err := LoadClusterCores(dir)
	if err != nil {
		t.Fatalf("LoadClusterCores: %v", err)
	}
	apiCache, err := LoadAPICores(dir)
	if err != nil {
		t.Fatalf("LoadAPICores: %v", err)
	}

	clusterURLs, _, ok := clusterCache.Get("shared.example")
	if !ok || clusterURLs[0] != "https://cluster-core.example" {
		t.Fatalf("cluster cache = %v (ok=%v), want the cluster core", clusterURLs, ok)
	}
	apiURLs, fresh, ok := apiCache.Get("shared.example")
	if !ok || !fresh || apiURLs[0] != "https://api-core.example" {
		t.Fatalf("api cache = %v (fresh=%v ok=%v), want the api core", apiURLs, fresh, ok)
	}
}
