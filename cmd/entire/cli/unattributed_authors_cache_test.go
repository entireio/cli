package cli

import (
	"testing"
	"time"
)

func TestUnattributedAuthorsCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	ok := cachedDetection{Tip: "tip-a", RepoID: "01REPO", Authors: []unattributedAuthor{{Email: "me@h.local", Count: 2}}, FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, ok); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", "01REPO", now.Add(time.Hour)); !hit || len(got.Authors) != 1 {
		t.Fatalf("same tip+repo, success outcome never expires: hit=%v got=%+v", hit, got)
	}
	// "" accepts whatever repo id is stored — detection reads before placement
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", "", now); !hit || got.RepoID != "01REPO" {
		t.Fatalf(`repoID "" must accept the stored id: hit=%v got=%+v`, hit, got)
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-b", "", now); hit {
		t.Fatal("moved tip must miss even with repoID \"\"")
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", "01OTHER", now); hit {
		t.Fatal("a non-empty, different repo id must miss")
	}
	if _, hit := readUnattributedAuthorsCache(dir, "", "01REPO", now); hit {
		t.Fatal("empty tip must miss (no key)")
	}
	// a skipped outcome is cached, but only briefly; RepoID may be empty
	skipped := cachedDetection{Tip: "tip-a", Skipped: "could not reach Entire", FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, skipped); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", "", now.Add(5*time.Minute)); !hit || got.Skipped == "" {
		t.Fatalf("skipped within TTL must hit: hit=%v got=%+v", hit, got)
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", "", now.Add(11*time.Minute)); hit {
		t.Fatal("skipped past TTL must miss")
	}
	invalidateUnattributedAuthorsCache(t.Context(), dir)
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", "", now); hit {
		t.Fatal("invalidated cache must miss")
	}
}
