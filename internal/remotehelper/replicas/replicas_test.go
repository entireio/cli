package replicas

import (
	"net/url"
	"slices"
	"testing"
)

func TestExtractRepoPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		urlPath string
		want    string
	}{
		{"/et/alice/repo", "et/alice/repo"},
		{"/et/alice/repo/", "et/alice/repo"},
		{"/et/a/b", "et/a/b"},
		{"/et/deeply/nested/path/repo", "et/deeply/nested/path/repo"},
		{"", ""},
		{"/et/", "et"},
		{"/et/repo", "et/repo"},
		// /gh/<owner>/<repo> is the mirror prefix; the prefix stays in
		// the cache key so a native /et/foo/bar and a same-coord mirror
		// /gh/foo/bar on the same cluster don't collide.
		{"/gh/alice/repo", "gh/alice/repo"},
		{"/gh/alice/repo/", "gh/alice/repo"},
	}
	for _, tt := range tests {
		got := ExtractRepoPath(tt.urlPath)
		if got != tt.want {
			t.Errorf("ExtractRepoPath(%q) = %q, want %q", tt.urlPath, got, tt.want)
		}
	}
}

func TestResolveDerivesFromURL(t *testing.T) {
	// Isolate from the user's real cache dir by pointing XDG_CACHE_HOME
	// at an empty tempdir.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	parsed, _ := url.Parse("entire://eu.cluster.example/et/alice/repo") //nolint:errcheck // test URL is static
	cfg := Resolve(parsed)
	if cfg.EntryURL != "https://eu.cluster.example" {
		t.Errorf("EntryURL = %q, want https://eu.cluster.example", cfg.EntryURL)
	}
	if cfg.ClusterHost != "eu.cluster.example" {
		t.Errorf("ClusterHost = %q, want eu.cluster.example", cfg.ClusterHost)
	}
	if cfg.RepoPath != "et/alice/repo" {
		t.Errorf("RepoPath = %q, want et/alice/repo", cfg.RepoPath)
	}
	if len(cfg.InitialNodes) != 0 {
		t.Errorf("InitialNodes = %v, want empty on cold start", cfg.InitialNodes)
	}
	if !cfg.Caching() {
		t.Error("expected Caching to be enabled when repo path is parseable")
	}
}

func TestPersistAndLoadFresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"
	want := []string{"https://n1.eu.cluster.example", "https://n2.eu.cluster.example"}

	Persist(cluster, repo, want)

	got := LoadFresh(cluster, repo)
	if !slices.Equal(got, want) {
		t.Errorf("LoadFresh = %v, want %v", got, want)
	}

	Invalidate(cluster, repo)
	if got := LoadFresh(cluster, repo); len(got) != 0 {
		t.Errorf("after Invalidate, LoadFresh = %v, want empty", got)
	}
}
