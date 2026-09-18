package cli

import (
	"testing"
	"time"
)

func TestUnattributedAuthorsCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	ok := cachedDetection{Tip: "tip-a", RepoID: "01REPO", Candidates: []string{"me@h.local"}, Authors: []unattributedAuthor{{Email: "me@h.local", Count: 2}}, FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, ok); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(time.Hour)); !hit || len(got.Authors) != 1 || got.RepoID != "01REPO" {
		t.Fatalf("same tip, success outcome never expires: hit=%v got=%+v", hit, got)
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-b", now); hit {
		t.Fatal("moved tip must miss")
	}
	if _, hit := readUnattributedAuthorsCache(dir, "", now); hit {
		t.Fatal("empty tip must miss (no key)")
	}
	// a skipped outcome is cached, but only briefly; RepoID may be empty
	skipped := cachedDetection{Tip: "tip-a", Skipped: "could not reach Entire", FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, skipped); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(5*time.Minute)); !hit || got.Skipped == "" {
		t.Fatalf("skipped within TTL must hit: hit=%v got=%+v", hit, got)
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(11*time.Minute)); hit {
		t.Fatal("skipped past TTL must miss")
	}
	// a no-candidates outcome (logged in, clean history) is also cached, but
	// only briefly — like skipped, it must not block doctor from noticing a
	// freshly-created reserved-host commit for longer than transientCacheTTL.
	noCandidates := cachedDetection{Tip: "tip-a", FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, noCandidates); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(5*time.Minute)); !hit || len(got.Candidates) != 0 {
		t.Fatalf("no-candidates within TTL must hit: hit=%v got=%+v", hit, got)
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(11*time.Minute)); hit {
		t.Fatal("no-candidates past TTL must miss")
	}
	invalidateUnattributedAuthorsCache(t.Context(), dir)
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", now); hit {
		t.Fatal("invalidated cache must miss")
	}
}
