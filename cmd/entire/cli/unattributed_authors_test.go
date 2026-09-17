package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// A superset of entire-core's TestIsReservedHostEmail so parity
// drift with regional.IsReservedHostEmail fails here, not as a server 400.
func TestReservedHostEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"coledriver@coles-macbook-pro.local", true},
		{"lizziesiegle@lizzies-macbook-pro.local", true},
		{"peytonmontei@mac.localdomain", true},
		{"coledriver@Coles-MacBook-Pro.local", true}, // predicate lowercases
		{"dev@localhost", true},
		{"dev@box.localhost", true},
		{"me@host.internal", true},
		{"me@host.lan", true},
		{"me@host.home", true},
		{"me@host.home.arpa", true},
		{"me@host.test", true},
		{"me@host.example", true},
		{"me@host.invalid", true},
		{"me@host.local.", true},   // one trailing dot tolerated
		{"me@host.local..", false}, // only one
		{"me@local", true},         // bare suffix is intentional
		{"me@.local", false},
		{"   ", false},
		{"bob@company.com", false},
		{"jdoe@lt-4471.corp.acme.com", false},
		{"me@fritz.box", false},
		{"me@attlocal.net", false},
		{"12345+octo@users.noreply.github.com", false},
		{"me@notlocal", false},
		{"me@host.notlocal", false},
		{"me@somehost", false}, // dotless host is NOT reserved
		{"", false},
		{"nolocalpart@", false},
		{"@host.local", false},
		{"two@at@host.local", false},
		{"me@xn--bcher-kva.local", false},
		{"me@hóst.local", false},
	}
	for _, c := range cases {
		if got := reservedHostEmail(c.in); got != c.want {
			t.Errorf("reservedHostEmail(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The authorship gate: only addresses whose local part is the current OS user
// are candidates. Hostname is deliberately not matched (renamed / new laptop).
func TestFilterCandidateAuthors(t *testing.T) {
	t.Parallel()
	authors := []string{
		"coledriver@Coles-MacBook-Pro.local",
		"coledriver@New-Laptop.local",            // same person, new machine — kept
		"lizziesiegle@Lizzies-MacBook-Pro.local", // colleague — dropped
		"coledriver@company.com",                 // not reserved-host — dropped
		"COLEDRIVER@Old-Mac.local.",              // case + trailing dot — kept, normalized
		"coledriver@Coles-MacBook-Pro.local",     // duplicate — deduped
		"coledriver@host.local..",                // two trailing dots — dropped, same as the predicate
	}
	got := filterCandidateAuthors(authors, "coledriver")
	want := []string{"coledriver@coles-macbook-pro.local", "coledriver@new-laptop.local", "coledriver@old-mac.local"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := filterCandidateAuthors(authors, ""); got != nil {
		t.Fatalf("empty username must yield no candidates, got %v", got)
	}
	got = filterCandidateAuthors(authors, "ColeDriver")
	if !slices.Equal(got, want) {
		t.Fatalf("ColeDriver (unnormalized) got %v, want %v", got, want)
	}
}

func TestNormalizeOSUsername(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"coledriver", "coledriver"},
		{"ColeDriver", "coledriver"},
		{`CORP\jdoe`, "jdoe"}, // Windows DOMAIN\user
		{"  coledriver ", "coledriver"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeOSUsername(c.in); got != c.want {
			t.Errorf("normalizeOSUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// git shortlog -se lines are "<tab-padded count>\t<Name> <email>".
func TestAuthorsFromShortlog(t *testing.T) {
	t.Parallel()
	in := "    12\tCole Driver <coledriver@Coles-MacBook-Pro.local>\n     1\tLizzie <lizziesiegle@lizzies.local>\n\n"
	got := authorsFromShortlog([]byte(in))
	want := []string{"coledriver@Coles-MacBook-Pro.local", "lizziesiegle@lizzies.local"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseUnattributedAuthorsResponse(t *testing.T) {
	t.Parallel()
	body := `{"authors":[{"email":"me@h.local","unattributedCommits":9},{"email":"me@old.local","unattributedCommits":0}]}`
	var wire unattributedAuthorsWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatal(err)
	}
	got := wire.nonZero()
	// zero-count entries are dropped: there is nothing on the web to repair
	if len(got) != 1 || got[0].Email != "me@h.local" || got[0].Count != 9 {
		t.Fatalf("got %+v", got)
	}
}

func TestFetchUnattributedAuthors_PostsEmailsAndRepoID(t *testing.T) {
	t.Parallel()
	var gotPath string
	var gotBody struct {
		Emails []string `json:"emails"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// errcheck runs with check-blank in this repo, so no `_ =` discards:
		// t.Error (not Fatal — this is the server goroutine) and fmt.Fprint
		// (exempted by the std-error-handling preset), as defaultAPIHandler in
		// checkpoint_api_reader_test.go does.
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"authors":[{"email":"me@h.local","unattributedCommits":3}]}`)
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("test-token", srv.URL)
	got, err := fetchUnattributedAuthors(t.Context(), client, "01REPO", []string{"me@h.local"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/repos/01REPO/authors/unattributed" || len(gotBody.Emails) != 1 {
		t.Fatalf("path=%s body=%+v", gotPath, gotBody)
	}
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestFetchUnattributedAuthors_NonOKIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	if _, err := fetchUnattributedAuthors(t.Context(), api.NewClientWithBaseURL("t", srv.URL), "01REPO", []string{"me@h.local"}); err == nil {
		t.Fatal("403 must be an error")
	}
}

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

func TestShortNetErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out reaching Entire"},
		{fmt.Errorf("resolve experts cell: %w", context.DeadlineExceeded), "timed out reaching Entire"},
		{errors.New("control plane unavailable: dial tcp 1.2.3.4: connection refused"), "control plane unavailable: dial tcp 1.2.3.4: connection refused"},
		{errors.New("first line\nsecond line"), "first line"},
		{errors.New(strings.Repeat("x", 100)), strings.Repeat("x", 79) + "…"},
	}
	for _, c := range cases {
		if got := shortNetErr(c.err); got != c.want {
			t.Errorf("shortNetErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
