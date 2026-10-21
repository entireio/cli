package cli

import (
	"strings"
	"testing"
	"time"
)

func TestUnattributedAuthorsCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	ok := cachedDetection{Key: "tip-a", RepoID: "01REPO", Candidates: []string{"me@h.local"}, Authors: []unattributedAuthor{{Email: "me@h.local", Count: 2}}, FetchedAt: now}
	if err := writeUnattributedAuthorsCache(dir, ok); err != nil {
		t.Fatal(err)
	}
	if got, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(30*time.Minute)); !hit || len(got.Authors) != 1 || got.RepoID != "01REPO" {
		t.Fatalf("same key, success outcome within its TTL must hit: hit=%v got=%+v", hit, got)
	}
	// Linking is account-wide and can happen in another repo or on the web,
	// so even a success entry is re-checked eventually.
	if _, hit := readUnattributedAuthorsCache(dir, "tip-a", now.Add(2*time.Hour)); hit {
		t.Fatal("success outcome past its TTL must miss")
	}
	if _, hit := readUnattributedAuthorsCache(dir, "tip-b", now); hit {
		t.Fatal("moved tip must miss")
	}
	if _, hit := readUnattributedAuthorsCache(dir, "", now); hit {
		t.Fatal("empty tip must miss (no key)")
	}
	// a skipped outcome is cached, but only briefly; RepoID may be empty
	skipped := cachedDetection{Key: "tip-a", Skipped: "could not reach Entire", FetchedAt: now}
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
	noCandidates := cachedDetection{Key: "tip-a", FetchedAt: now}
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

func TestUnattributedAuthorsCacheKey(t *testing.T) {
	t.Parallel()
	base := unattributedAuthorsCacheKey("env\x00secret-token", "me", "abc HEAD\n")
	if base == "" || strings.Contains(base, "secret-token") {
		t.Fatalf("key = %q, want a non-empty hash that does not carry the token", base)
	}
	for _, other := range []string{
		unattributedAuthorsCacheKey("env\x00other-token", "me", "abc HEAD\n"),
		unattributedAuthorsCacheKey("env\x00secret-token", "you", "abc HEAD\n"),
		unattributedAuthorsCacheKey("env\x00secret-token", "me", "def HEAD\n"),
	} {
		if other == base {
			t.Fatal("changing identity, username or refs must change the key")
		}
	}
	if got := unattributedAuthorsCacheKey("id", "me", ""); got != "" {
		t.Fatalf("no refs: key = %q, want \"\" (no caching)", got)
	}
}
