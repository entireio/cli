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
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
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
		"COLEDRIVER@Old-Mac.local.",              // case + trailing dot — kept, normalized, dot NOT stripped
		"coledriver@Coles-MacBook-Pro.local",     // duplicate — deduped
		"coledriver@host.local..",                // two trailing dots — dropped, same as the predicate
	}
	got := filterCandidateAuthors(authors, "coledriver")
	want := []string{"coledriver@coles-macbook-pro.local", "coledriver@new-laptop.local", "coledriver@old-mac.local."}
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
	body := `{"authors":[{"email":"me@h.local","unattributedCommitCount":9},{"email":"me@old.local","unattributedCommitCount":0}]}`
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
	var gotPath, gotMethod string
	var gotBody struct {
		Emails []string `json:"emails"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		// errcheck runs with check-blank in this repo, so no `_ =` discards:
		// t.Error (not Fatal — this is the server goroutine) and fmt.Fprint
		// (exempted by the std-error-handling preset), as defaultAPIHandler in
		// checkpoint_api_reader_test.go does.
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"authors":[{"email":"me@h.local","unattributedCommitCount":3}]}`)
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("test-token", srv.URL)
	got, err := fetchUnattributedAuthors(t.Context(), client, "01REPO", []string{"me@h.local"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/repos/01REPO/authors:resolve-unattributed" || len(gotBody.Emails) != 1 {
		t.Fatalf("path=%s body=%+v", gotPath, gotBody)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
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
		{errors.New(strings.Repeat("é", 100)), strings.Repeat("é", 79) + "…"}, // rune-not-byte truncation
	}
	for _, c := range cases {
		if got := shortNetErr(c.err); got != c.want {
			t.Errorf("shortNetErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

type detectRecorder struct {
	fetched bool
	wrote   *cachedDetection
}

// detectDepsForTest returns fully-populated fakes: logged in via context, one
// candidate, cache miss, placement + fetch succeed. Cases override fields.
func detectDepsForTest(t *testing.T, rec *detectRecorder) unattributedAuthorsDeps {
	t.Helper()
	return unattributedAuthorsDeps{
		username:      func() string { return "me" },
		localAuthors:  func(context.Context, string) []string { return []string{"me@h.local"} },
		lookupEnv:     func(string) (string, bool) { return "", false },
		activeContext: func() (*contexts.Context, bool, error) { return &contexts.Context{}, true, nil },
		commonDir:     func(context.Context) (string, error) { return t.TempDir(), nil },
		originTip:     func(context.Context) string { return "tip" },
		readCache:     func(string, string, time.Time) (cachedDetection, bool) { return cachedDetection{}, false },
		writeCache: func(_ string, c cachedDetection) error {
			rec.wrote = &c
			return nil
		},
		resolvePlacement: func(context.Context) (repoCellPlacement, error) {
			return repoCellPlacement{RepoID: "01REPO", Target: &auth.CellTarget{}}, nil
		},
		cellClient: func(context.Context, *auth.CellTarget) (*api.Client, error) {
			return api.NewClientWithBaseURL("t", "http://unused"), nil
		},
		fetch: func(context.Context, *api.Client, string, []string) ([]unattributedAuthor, error) {
			rec.fetched = true
			return []unattributedAuthor{{Email: "me@h.local", Count: 9}}, nil
		},
		now:            time.Now,
		networkTimeout: time.Second,
	}
}

func TestDetectUnattributedAuthors(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		mod  func(*unattributedAuthorsDeps)
		want func(t *testing.T, out detectionOutcome, rec detectRecorder)
	}{
		{"no candidates → empty, nothing else runs", func(d *unattributedAuthorsDeps) {
			d.localAuthors = func(context.Context, string) []string { return nil }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if len(out.Candidates) != 0 || rec.fetched {
				t.Fatalf("%+v fetched=%v", out, rec.fetched)
			}
		}},
		{"logged out (no env token, no context) → candidates, no fetch", func(d *unattributedAuthorsDeps) {
			d.activeContext = func() (*contexts.Context, bool, error) { return nil, false, nil }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.LoggedIn || len(out.Candidates) != 1 || rec.fetched {
				t.Fatalf("%+v fetched=%v", out, rec.fetched)
			}
		}},
		{"ENTIRE_TOKEN counts as logged in", func(d *unattributedAuthorsDeps) {
			d.activeContext = func() (*contexts.Context, bool, error) { return nil, false, nil }
			d.lookupEnv = func(string) (string, bool) { return "tok", true }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if !out.LoggedIn || !rec.fetched {
				t.Fatalf("%+v fetched=%v", out, rec.fetched)
			}
		}},
		{"activeContext error → treated as logged out", func(d *unattributedAuthorsDeps) {
			d.activeContext = func() (*contexts.Context, bool, error) { return nil, false, errors.New("bad --context") }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.LoggedIn || rec.fetched {
				t.Fatalf("%+v", out)
			}
		}},
		{"placement error → Skipped names the reason, cached with empty RepoID", func(d *unattributedAuthorsDeps) {
			d.resolvePlacement = func(context.Context) (repoCellPlacement, error) {
				return repoCellPlacement{}, errors.New("control plane unavailable: dial tcp")
			}
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.Skipped == "" || rec.fetched || rec.wrote == nil || rec.wrote.Skipped == "" || rec.wrote.RepoID != "" {
				t.Fatalf("%+v wrote=%+v", out, rec.wrote)
			}
		}},
		{"placement timeout → 'timed out reaching Entire'", func(d *unattributedAuthorsDeps) {
			d.resolvePlacement = func(context.Context) (repoCellPlacement, error) {
				return repoCellPlacement{}, fmt.Errorf("resolve: %w", context.DeadlineExceeded)
			}
		}, func(t *testing.T, out detectionOutcome, _ detectRecorder) {
			if out.Skipped != "timed out reaching Entire" {
				t.Fatalf("%+v", out)
			}
		}},
		{"fetch error → Skipped, cached with RepoID", func(d *unattributedAuthorsDeps) {
			d.fetch = func(context.Context, *api.Client, string, []string) ([]unattributedAuthor, error) {
				return nil, errors.New("cell: 503")
			}
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.Skipped == "" || rec.wrote == nil || rec.wrote.Skipped == "" || rec.wrote.RepoID != "01REPO" {
				t.Fatalf("%+v wrote=%+v", out, rec.wrote)
			}
		}},
		{"cellClient error → Skipped, no fetch, cached with RepoID", func(d *unattributedAuthorsDeps) {
			d.cellClient = func(context.Context, *auth.CellTarget) (*api.Client, error) {
				return nil, errors.New("no token for cell")
			}
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.Skipped == "" || rec.fetched || rec.wrote == nil || rec.wrote.RepoID != "01REPO" {
				t.Fatalf("%+v wrote=%+v", out, rec.wrote)
			}
		}},
		{"no tip → nothing cached", func(d *unattributedAuthorsDeps) {
			d.originTip = func(context.Context) string { return "" }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if len(out.Authors) != 1 || rec.wrote != nil {
				t.Fatalf("%+v wrote=%+v", out, rec.wrote)
			}
		}},
		{"happy → Authors + RepoID, success cached, FetchedAt pinned", func(d *unattributedAuthorsDeps) {
			d.now = func() time.Time { return fixedNow }
		}, func(t *testing.T, out detectionOutcome, rec detectRecorder) {
			if out.RepoID != "01REPO" || len(out.Authors) != 1 || rec.wrote == nil || rec.wrote.RepoID != "01REPO" || rec.wrote.Skipped != "" {
				t.Fatalf("%+v wrote=%+v", out, rec.wrote)
			}
			if !rec.wrote.FetchedAt.Equal(fixedNow) {
				t.Fatalf("FetchedAt = %v, want %v", rec.wrote.FetchedAt, fixedNow)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec detectRecorder
			d := detectDepsForTest(t, &rec)
			if tc.mod != nil {
				tc.mod(&d)
			}
			out := detectUnattributedAuthors(t.Context(), d)
			tc.want(t, out, rec)
		})
	}
}

// A warm cache short-circuits placement and fetch — and still carries RepoID,
// which the doctor fix path needs to declare against. It also short-circuits
// `git shortlog` itself: a logged-in cache hit must never walk history, and
// the Candidates it returns come from the cache, not a fresh shortlog.
func TestDetectUnattributedAuthors_CacheHitSkipsNetworkKeepsRepoID(t *testing.T) {
	t.Parallel()
	var rec detectRecorder
	d := detectDepsForTest(t, &rec)
	d.localAuthors = func(context.Context, string) []string {
		t.Fatal("shortlog must not run on a warm cache")
		return nil
	}
	d.readCache = func(_, tip string, _ time.Time) (cachedDetection, bool) {
		if tip != "tip" {
			t.Fatalf("readCache(tip=%q), want \"tip\"", tip)
		}
		return cachedDetection{
			Tip:        "tip",
			RepoID:     "01REPO",
			Candidates: []string{"me@h.local"},
			Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 4}},
		}, true
	}
	d.resolvePlacement = func(context.Context) (repoCellPlacement, error) {
		t.Fatal("placement must not be resolved on a warm cache")
		return repoCellPlacement{}, nil
	}
	out := detectUnattributedAuthors(t.Context(), d)
	if rec.fetched || out.RepoID != "01REPO" || len(out.Authors) != 1 || out.Authors[0].Count != 4 || rec.wrote != nil {
		t.Fatalf("%+v fetched=%v wrote=%+v", out, rec.fetched, rec.wrote)
	}
	if len(out.Candidates) != 1 || out.Candidates[0] != "me@h.local" {
		t.Fatalf("out.Candidates = %v, want cached [\"me@h.local\"]", out.Candidates)
	}
}

// TestDetectUnattributedAuthors_NoCandidatesCachedTransiently exercises the
// real cache (not the fixture stubs) end-to-end: a logged-in user with a
// clean history is the common case, so that outcome must be cached too —
// briefly — or every `entire status` pays for a full `git shortlog --all`.
// A second call within transientCacheTTL is served from the cache without
// running shortlog again; one past the TTL runs shortlog again.
func TestDetectUnattributedAuthors_NoCandidatesCachedTransiently(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	shortlogCalls := 0

	var rec detectRecorder
	d := detectDepsForTest(t, &rec)
	d.localAuthors = func(context.Context, string) []string {
		shortlogCalls++
		return nil
	}
	d.commonDir = func(context.Context) (string, error) { return dir, nil }
	d.readCache = readUnattributedAuthorsCache
	d.writeCache = writeUnattributedAuthorsCache
	d.now = func() time.Time { return now }

	out := detectUnattributedAuthors(t.Context(), d)
	if shortlogCalls != 1 || len(out.Candidates) != 0 || !out.LoggedIn {
		t.Fatalf("first call: shortlogCalls=%d out=%+v", shortlogCalls, out)
	}

	d.now = func() time.Time { return now.Add(5 * time.Minute) }
	out = detectUnattributedAuthors(t.Context(), d)
	if shortlogCalls != 1 || len(out.Candidates) != 0 || !out.LoggedIn {
		t.Fatalf("within TTL: shortlogCalls=%d out=%+v, want cache hit (no new shortlog)", shortlogCalls, out)
	}

	d.now = func() time.Time { return now.Add(11 * time.Minute) }
	out = detectUnattributedAuthors(t.Context(), d)
	if shortlogCalls != 2 || len(out.Candidates) != 0 || !out.LoggedIn {
		t.Fatalf("past TTL: shortlogCalls=%d out=%+v, want a second shortlog", shortlogCalls, out)
	}
}

// TestDetectUnattributedAuthors_LocalAuthorsTimeoutBounded proves both
// localAuthors call sites — logged-out (immediate shortlog) and logged-in on
// a cache miss — are bounded by networkTimeout. Production wraps `git
// shortlog` in its own subprocess timeout, but the deps seam itself must not
// let an injected (or future) localAuthors implementation that ignores ctx
// hang detection forever; a fake that only returns once its ctx is Done
// proves the call is actually cancelled, not just slow.
func TestDetectUnattributedAuthors_LocalAuthorsTimeoutBounded(t *testing.T) {
	t.Parallel()
	blocking := func(ctx context.Context, _ string) []string {
		<-ctx.Done()
		return nil
	}
	cases := []struct {
		name string
		mod  func(*unattributedAuthorsDeps)
	}{
		{"logged out", func(d *unattributedAuthorsDeps) {
			d.activeContext = func() (*contexts.Context, bool, error) { return nil, false, nil }
		}},
		{"logged in, cache miss", func(*unattributedAuthorsDeps) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec detectRecorder
			d := detectDepsForTest(t, &rec)
			d.localAuthors = blocking
			d.networkTimeout = 50 * time.Millisecond
			tc.mod(&d)

			start := time.Now()
			out := detectUnattributedAuthors(t.Context(), d)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("detectUnattributedAuthors took %v, want well under 1s", elapsed)
			}
			if len(out.Candidates) != 0 {
				t.Fatalf("out.Candidates = %v, want none", out.Candidates)
			}
		})
	}
}

// A nil-field guard for the production wiring used by doctor and status;
// `unused` (U1000) would fail the lint gate without a reference. This one
// reference clears it for defaultUnattributedAuthorsDeps and, through it,
// defaultOSUsername, localCandidateAuthors and originDefaultTip.
func TestDefaultUnattributedAuthorsDeps_Wired(t *testing.T) {
	t.Parallel()
	d := defaultUnattributedAuthorsDeps(time.Second)
	if d.username == nil || d.localAuthors == nil || d.lookupEnv == nil || d.activeContext == nil ||
		d.commonDir == nil || d.originTip == nil || d.readCache == nil || d.writeCache == nil ||
		d.resolvePlacement == nil || d.cellClient == nil || d.fetch == nil || d.now == nil {
		t.Fatal("defaultUnattributedAuthorsDeps left a func field nil")
	}
	if d.networkTimeout != time.Second {
		t.Fatalf("networkTimeout = %v, want 1s", d.networkTimeout)
	}
}
