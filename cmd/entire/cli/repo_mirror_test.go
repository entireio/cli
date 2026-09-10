package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// mirrorsAPIPath is the control-plane mirrors collection endpoint, shared by the
// fake servers in these tests.
const mirrorsAPIPath = "/api/v1/mirrors"

func TestExplainSuspendedMirror(t *testing.T) {
	t.Parallel()
	const id = "01KS6KFJR2XS6PZ188MVYE07AN"
	var buf bytes.Buffer
	explainSuspendedMirror(&buf, id)
	out := buf.String()
	require.Contains(t, out, id, "message must name the mirror")
	require.Contains(t, out, "suspended")
	require.Contains(t, out, "Contact support", "must point at support, not an internal admin command")
	require.NotContains(t, out, "entire-core", "must not leak internal terminology")
}

// fakeMirrorGetter feeds awaitMirrorReady a scripted sequence of statuses (the
// last entry repeats) or a fixed error, standing in for *coreapi.Client.GetMirror.
// errsBefore makes the first N calls return a transient error before the status
// sequence begins, to exercise the poll's retry tolerance.
type fakeMirrorGetter struct {
	statuses   []coreapi.MirrorStatus
	err        error
	errsBefore int
	calls      int
}

func (f *fakeMirrorGetter) GetMirror(_ context.Context, _ coreapi.GetMirrorParams) (*coreapi.Mirror, error) {
	n := f.calls
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if n < f.errsBefore {
		return nil, errors.New("transient: connection reset")
	}
	i := n - f.errsBefore
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	m := &coreapi.Mirror{}
	m.Status = coreapi.NewOptMirrorStatus(f.statuses[i])
	return m, nil
}

// TestAwaitMirrorReady covers the clone-status poll that replaced the info/refs
// probe: terminal statuses resolve, processing keeps polling, and an exhausted
// deadline reports a timeout.
//
// Not parallel: shortens the package-level mirrorPollInterval.
func TestAwaitMirrorReady(t *testing.T) {
	prev := mirrorPollInterval
	mirrorPollInterval = time.Millisecond
	t.Cleanup(func() { mirrorPollInterval = prev })
	ctx := t.Context()

	t.Run("ready resolves with no error", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusReady}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
	})

	t.Run("processing then ready keeps polling", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{
			coreapi.MirrorStatusProcessing, coreapi.MirrorStatusProcessing, coreapi.MirrorStatusReady,
		}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
		require.GreaterOrEqual(t, f.calls, 3)
	})

	t.Run("failed returns errMirrorCloneFailed", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusFailed}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.ErrorIs(t, err, errMirrorCloneFailed)
		require.Equal(t, coreapi.MirrorStatusFailed, status)
	})

	t.Run("suspended returns errMirrorSuspended", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusSuspended}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.ErrorIs(t, err, errMirrorSuspended)
		require.Equal(t, coreapi.MirrorStatusSuspended, status)
	})

	t.Run("never-ready times out", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusProcessing}}
		_, err := awaitMirrorReady(ctx, f, "m", 20*time.Millisecond)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("transient errors are tolerated, then ready", func(t *testing.T) {
		// Fewer consecutive errors than the cap, so the poll rides them out.
		f := &fakeMirrorGetter{errsBefore: maxConsecutivePollErrors - 1, statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusReady}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
	})

	t.Run("persistent errors give up after the cap", func(t *testing.T) {
		f := &fakeMirrorGetter{err: errors.New("boom")}
		_, err := awaitMirrorReady(ctx, f, "m", time.Second)
		require.ErrorContains(t, err, "poll mirror status")
		require.Equal(t, maxConsecutivePollErrors, f.calls, "should stop at the cap, not spin to the deadline")
	})
}

func TestRepoMirrorCreate_WaitTimeoutHelp(t *testing.T) {
	t.Parallel()

	flag := newRepoMirrorCreateCmd().Flags().Lookup("wait-timeout")
	require.NotNil(t, flag)
	require.Equal(t, "How long to wait for mirror request submission, placement, and clone readiness", flag.Usage)
}

// TestReportOneShotMirror exercises the one-shot create's presentation across
// the shared lifecycle outcomes driven by mirrorCreateOutcome.
func TestReportOneShotMirror(t *testing.T) {
	t.Parallel()
	const id = "01KS6KFJR2XS6PZ188MVYE07AN"
	const mirrorURL = "entire://eu-west-1.entire.io/gh/octocat/hello-world"
	mk := func() *coreapi.CreatedMirror {
		return &coreapi.CreatedMirror{MirrorId: id, MirrorUrl: mirrorURL}
	}

	t.Run("create failure surfaces with nothing printed", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		wantErr := errors.New("boom")
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{}, wantErr)
		require.ErrorIs(t, err, wantErr)
		require.Empty(t, out.String())
	})

	t.Run("no-wait prints in-progress hint", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{created: mk()}, nil)
		require.NoError(t, err)
		require.Contains(t, out.String(), "Mirror placed at "+mirrorURL)
		require.Contains(t, out.String(), "Mirror ID: "+id)
		require.Contains(t, out.String(), "still be in progress")
	})

	t.Run("ready prints clone hint", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(), status: coreapi.MirrorStatusReady, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, nil)
		require.NoError(t, err)
		require.Contains(t, out.String(), "git clone "+mirrorURL)
	})

	t.Run("suspended surfaces support guidance as SilentError", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(), status: coreapi.MirrorStatusSuspended, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, errMirrorSuspended)
		var silent *SilentError
		require.ErrorAs(t, err, &silent)
		require.Contains(t, errW.String(), "Contact support")
		require.NotContains(t, errW.String(), "entire-core")
		require.NotContains(t, out.String(), "git clone")
	})

	t.Run("failed returns an error naming the mirror", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(), status: coreapi.MirrorStatusFailed, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, errMirrorCloneFailed)
		require.Error(t, err)
		require.Contains(t, err.Error(), id)
	})

	t.Run("timeout propagates the wait error", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		wantErr := errors.New("timed out waiting for initial clone")
		outcome := mirrorCreateOutcome{created: mk(), status: coreapi.MirrorStatusProcessing, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, wantErr)
		require.ErrorIs(t, err, wantErr)
	})
}

// recordedRequest captures the routing facts a command-level test asserts on:
// which endpoint the list command hit and with what query.
type recordedRequest struct {
	method string
	path   string
	query  url.Values
}

// onboardedEntry builds a /repos index entry for a mirrored/native repo with a
// single ready placement on the given cluster slug.
func onboardedEntry(fullName, visibility, slug string) coreapi.RepoIndexEntry {
	return coreapi.RepoIndexEntry{
		FullName:   fullName,
		Visibility: visibility,
		Placements: []coreapi.RepoPlacement{{ClusterSlug: slug, Status: coreapi.RepoPlacementStatusReady, Mirror: true}},
	}
}

// nativeEntry is an onboarded repo with a non-mirror (native Entire) placement,
// e.g. one created by `entire repo create`. `repo mirror list` must not
// synthesize a GitHub clone URL for it and drops it from the directory.
func nativeEntry(fullName, visibility, slug string) coreapi.RepoIndexEntry {
	return coreapi.RepoIndexEntry{
		FullName:   fullName,
		Visibility: visibility,
		Placements: []coreapi.RepoPlacement{{ClusterSlug: slug, Status: coreapi.RepoPlacementStatusReady, Mirror: false}},
	}
}

// onboardedMulti builds a /repos entry placed on several clusters (all ready),
// so multi-placement grouping and the CLUSTERS cell are observable.
func onboardedMulti(fullName, visibility string, slugs ...string) coreapi.RepoIndexEntry {
	e := coreapi.RepoIndexEntry{FullName: fullName, Visibility: visibility}
	for _, s := range slugs {
		e.Placements = append(e.Placements, coreapi.RepoPlacement{ClusterSlug: s, Status: coreapi.RepoPlacementStatusReady, Mirror: true})
	}
	return e
}

// candidateEntry builds a /repos index entry for an onboardable GitHub repo.
func candidateEntry(fullName, visibility string, access coreapi.RepoCandidateAccess, onboardable bool) coreapi.RepoIndexEntry {
	return coreapi.RepoIndexEntry{
		FullName:   fullName,
		Visibility: visibility,
		Candidate:  coreapi.NewOptRepoCandidate(coreapi.RepoCandidate{Access: access, Onboardable: onboardable}),
	}
}

// Paths the fake control-plane servers below route on.
const (
	testClustersPath = "/api/v1/clusters"
	testReposPath    = "/api/v1/repos"
)

// bulkEntries builds n onboarded /repos entries named <prefix>/repo-0000…,
// for tests that need to cross the fetch budget.
func bulkEntries(prefix string, n int) []coreapi.RepoIndexEntry {
	entries := make([]coreapi.RepoIndexEntry, 0, n)
	for i := range n {
		entries = append(entries, onboardedEntry(fmt.Sprintf("%s/repo-%04d", prefix, i), "private", "us"))
	}
	return entries
}

// serveRepoList stands up a fake control-plane serving the two endpoints the
// merged `list` calls: GET /clusters (the slug→host catalog used to synthesise
// clone URLs) and GET /repos?scope=all (the directory). It points the
// active-context client seam at the server for the test. Only the /repos
// request is delivered on the returned channel — receiving it after the command
// runs is the happens-before edge that synchronises handler-goroutine writes
// with test-goroutine reads (HTTP completion alone is not an edge the race
// detector recognises; see TestBearerOnlySource_NoCookieOnTheWire). Buffered so
// the handler never blocks on the send.
func serveRepoList(t *testing.T, repos []coreapi.RepoIndexEntry, clusters []coreapi.Cluster, truncated bool) <-chan recordedRequest {
	t.Helper()
	recCh := make(chan recordedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testClustersPath:
			if err := printJSON(w, &coreapi.ListClustersOutputBody{Clusters: clusters}); err != nil {
				t.Errorf("encode clusters response: %v", err)
			}
		case testReposPath:
			if err := printJSON(w, &coreapi.ListReposOutputBody{Repos: repos, Truncated: truncated}); err != nil {
				t.Errorf("encode repos response: %v", err)
			}
			recCh <- recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return recCh
}

// serveRepoListPaged is serveRepoList with a keyset-paginated /repos: each call
// answers with the page addressed by the pageToken query param ("" is the first
// page), echoing that page's NextPageToken so the client can walk the chain.
// Every /repos request is delivered on the returned channel (buffered to the
// page count so the handler never blocks).
func serveRepoListPaged(t *testing.T, pages []coreapi.ListReposOutputBody, clusters []coreapi.Cluster) <-chan recordedRequest {
	t.Helper()
	tokenToPage := make(map[string]coreapi.ListReposOutputBody, len(pages))
	for i, p := range pages {
		token := ""
		if i > 0 {
			token = pages[i-1].NextPageToken.Or("")
			require.NotEmpty(t, token, "every page but the last needs a NextPageToken linking to the next one")
		}
		tokenToPage[token] = p
	}
	recCh := make(chan recordedRequest, len(pages))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testClustersPath:
			if err := printJSON(w, &coreapi.ListClustersOutputBody{Clusters: clusters}); err != nil {
				t.Errorf("encode clusters response: %v", err)
			}
		case testReposPath:
			page, ok := tokenToPage[r.URL.Query().Get("pageToken")]
			if !ok {
				t.Errorf("unexpected pageToken %q", r.URL.Query().Get("pageToken"))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if err := printJSON(w, &page); err != nil {
				t.Errorf("encode repos response: %v", err)
			}
			recCh <- recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return recCh
}

// serveRepoListClustersError is serveRepoList with a failing /clusters catalog:
// /repos answers normally but the slug→host lookup 500s, so `list` must fail
// instead of returning mirror rows with silently empty clone URLs. Points the
// active-context client seam at the server for the test.
func serveRepoListClustersError(t *testing.T, repos []coreapi.RepoIndexEntry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testClustersPath:
			w.WriteHeader(http.StatusInternalServerError)
		case testReposPath:
			w.Header().Set("Content-Type", "application/json")
			if err := printJSON(w, &coreapi.ListReposOutputBody{Repos: repos}); err != nil {
				t.Errorf("encode repos response: %v", err)
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
}

// execMirrorList runs `list` under a parent that carries the control-plane
// persistent flags (--insecure-http-auth); --json is a local flag on the list
// command itself, so tests can exercise --json and the client-side --name/--sort
// together.
func execMirrorList(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: "mirror"}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoMirrorListCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"list"}, args...))
	err = parent.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// runMirrorList executes `repo mirror list` with args against the fake server,
// returning stdout (the table/JSON) and stderr (the routing banner).
func runMirrorList(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := execMirrorList(t, args...)
	require.NoError(t, err)
	return stdout, stderr
}

// TestRepoMirrorList_Merged pins the merged `repo mirror list`: one table from a
// single GET /repos?scope=all, with existing mirrors (one row per repo: clusters
// + clone status) and onboardable candidates (access + availability)
// interleaved. Per-row formatting is covered by TestRepoDirCells; this pins
// the end-to-end routing and rendering.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorList_Merged(t *testing.T) {
	clusters := []coreapi.Cluster{{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"}}

	t.Run("mirrors and candidates render in one table via scope=all", func(t *testing.T) {
		recCh := serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			candidateEntry("acme/marketing", "public", coreapi.RepoCandidateAccessAdmin, true),
			candidateEntry("alice/dotfiles", "private", coreapi.RepoCandidateAccessRead, false),
		}, clusters, false)
		stdout, stderr := runMirrorList(t)
		rec := <-recCh

		require.Equal(t, http.MethodGet, rec.method)
		require.Equal(t, testReposPath, rec.path)
		require.Equal(t, "all", rec.query.Get("scope"), "list must request the unified directory")
		require.Contains(t, stderr, "Listing repos on")
		for _, h := range []string{"NAME", "CLUSTERS", "VISIBILITY", "STATUS", "ACCESS"} {
			require.Contains(t, stdout, h)
		}
		// Mirror row: cluster slug + clone status, access dashed.
		require.Regexp(t, `acme/web\s+us\s+Private\s+ready`, stdout)
		// Candidate rows: availability status + access, clusters dashed.
		require.Contains(t, stdout, "acme/marketing")
		require.Contains(t, stdout, "available")
		require.Contains(t, stdout, "admin")
		require.Contains(t, stdout, "alice/dotfiles")
		require.Contains(t, stdout, "owner-only")
		// The NAME cell is the handle into the detail view.
		require.Contains(t, stderr, "entire repo mirror get <owner/repo>")
	})

	t.Run("a multi-cluster repo lists once, clusters joined in one cell", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedMulti("acme/web", "private", "us", "eu"),
		}, []coreapi.Cluster{
			{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
			{Slug: "eu", PublicUrl: "https://eu-west-1.entire.io"},
		}, false)
		stdout, _ := runMirrorList(t)
		require.Equal(t, 1, strings.Count(stdout, "acme/web"), "one row per repo, not one per placement")
		require.Regexp(t, `acme/web\s+us, eu\s+Private\s+ready`, stdout)
	})

	t.Run("the detail hint is withheld from empty tables and --json", func(t *testing.T) {
		serveRepoList(t, nil, clusters, false)
		_, stderr := runMirrorList(t)
		require.NotContains(t, stderr, "mirror get", "no rows, nothing to drill into")

		serveRepoList(t, []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")}, clusters, false)
		_, stderr = runMirrorList(t, "--json")
		require.NotContains(t, stderr, "mirror get", "scripts already get nested placements in the rows")
	})

	t.Run("empty directory prints the empty sentence", func(t *testing.T) {
		serveRepoList(t, nil, clusters, false)
		stdout, _ := runMirrorList(t)
		require.Contains(t, stdout, "No repos found.")
	})

	t.Run("the directory follows nextPageToken across every page", func(t *testing.T) {
		recCh := serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")},
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{candidateEntry("acme/marketing", "public", coreapi.RepoCandidateAccessAdmin, true)},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t)

		// The command has returned, so every request it made is already
		// buffered; a non-blocking receive distinguishes "never fetched the
		// second page" from a hang.
		first := <-recCh
		require.Empty(t, first.query.Get("pageToken"), "the first request starts the chain")
		select {
		case second := <-recCh:
			require.Equal(t, "p2", second.query.Get("pageToken"), "the second request passes the cursor back")
		default:
			t.Fatal("the directory stopped after page 1 instead of following nextPageToken")
		}
		require.Contains(t, stdout, "acme/web", "page-1 row renders")
		require.Contains(t, stdout, "acme/marketing", "page-2 row renders")
		require.NotContains(t, stderr, "truncated", "a fully-walked chain is not a truncated directory")
	})

	t.Run("--limit caps rows after the default name sort", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("zeta/last", "private", "us"),
			onboardedEntry("acme/web", "private", "us"),
			onboardedEntry("mid/way", "private", "us"),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--limit", "2")
		require.Contains(t, stdout, "acme/web")
		require.Contains(t, stdout, "mid/way")
		require.NotContains(t, stdout, "zeta/last", "sorted last, so --limit 2 drops it")
	})

	t.Run("--limit composes with filters before capping", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			candidateEntry("acme/marketing", "public", coreapi.RepoCandidateAccessAdmin, true),
			candidateEntry("acme/site", "public", coreapi.RepoCandidateAccessAdmin, true),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--status", "available", "--limit", "1")
		require.Contains(t, stdout, "acme/marketing", "first available candidate by name survives")
		require.NotContains(t, stdout, "acme/site", "capped after the filter")
		require.NotContains(t, stdout, "acme/web", "filtered out before the cap")
	})

	t.Run("a negative --limit fails fast", func(t *testing.T) {
		serveRepoList(t, nil, clusters, false)
		err := runMirrorListErr(t, "--limit", "-1")
		require.Error(t, err)
		require.Contains(t, err.Error(), "--limit")
	})
}

// TestRepoMirrorList_FetchBudget pins the bounded cursor walk: by default at
// most coreListFetchBudget entries are fetched (raised to --limit when larger),
// a partial window is disclosed on stderr — including for --json — and --all
// lifts the bound entirely.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorList_FetchBudget(t *testing.T) {
	clusters := []coreapi.Cluster{{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"}}

	t.Run("the default fetch budget stops the walk and discloses the partial window", func(t *testing.T) {
		recCh := serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         bulkEntries("bulk", 1000),
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t)

		require.NotContains(t, stdout, "tail/end", "the walk must stop at the budget, not fetch page 2")
		<-recCh
		select {
		case rec := <-recCh:
			t.Fatalf("no second page request expected, got one with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.Contains(t, stderr, "first 1000", "the note says how much was fetched")
		require.Contains(t, stderr, "local", "the note says filters/sort ran locally over the window")
		require.Contains(t, stderr, "--all", "the note points at the escape hatch")
	})

	t.Run("--all walks past the budget and prints no note", func(t *testing.T) {
		serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         bulkEntries("bulk", 1000),
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t, "--all")
		require.Contains(t, stdout, "tail/end", "--all fetches the full directory")
		require.NotContains(t, stderr, "--all", "a complete walk needs no note")
	})

	t.Run("--limit above the budget raises the fetch budget to match", func(t *testing.T) {
		serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         bulkEntries("aaa", 1000),
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos:         bulkEntries("bbb", 500),
				NextPageToken: coreapi.NewOptString("p3"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t, "--limit", "1200")
		require.Contains(t, stdout, "bbb/repo-0100", "rows past the default budget are shown when --limit asks for them")
		require.NotContains(t, stdout, "tail/end", "the walk still stops once --limit is satisfiable")
		require.Contains(t, stderr, "first 1500", "the note reports the real fetched count")
	})

	t.Run("a capped page mid-chain does not warn once the cursor walks past it", func(t *testing.T) {
		serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")},
				NextPageToken: coreapi.NewOptString("p2"),
				Truncated:     true,
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("acme/api", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t)
		require.Contains(t, stdout, "acme/api", "the chain was walked to the end")
		require.NotContains(t, stderr, "truncated", "nothing was left unseen, so no warning")
	})

	t.Run("truncated result warns on stderr, not stdout", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
		}, clusters, true)
		stdout, stderr := runMirrorList(t)
		require.Contains(t, stderr, "truncated")
		require.NotContains(t, stdout, "truncated")
	})

	t.Run("truncated warning reaches --json runs on stderr", func(t *testing.T) {
		// Same rationale as the partial-window note: a script acting on
		// silently truncated data is the worst outcome, and stderr never
		// corrupts the stdout JSON.
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
		}, clusters, true)
		stdout, stderr := runMirrorList(t, "--json")
		require.Contains(t, stderr, "truncated")
		require.Contains(t, stdout, `"cloneUrl"`, "--json emits the directory rows with nested placements")
	})

	t.Run("a catalog fetch failure fails the whole command", func(t *testing.T) {
		// The clone URL is the payload of a mirror listing and --json suppresses
		// the banner, so a catalog error must abort rather than hand back rows
		// with silently empty clone URLs.
		serveRepoListClustersError(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
		})
		err := runMirrorListErr(t)
		require.Error(t, err)
	})

	t.Run("a malformed catalog publicUrl lists the mirror without a clone URL, never a spoofed one", func(t *testing.T) {
		// The `bad` cluster's publicUrl smuggles evil.com via userinfo; it must
		// never produce a clone URL. The mirror still lists — its slug in
		// CLUSTERS — and its --json placement carries no cloneUrl. The healthy
		// cluster's clone URL is unaffected.
		serve := func(t *testing.T) {
			t.Helper()
			serveRepoList(t, []coreapi.RepoIndexEntry{
				onboardedEntry("acme/web", "private", "us"),
				onboardedEntry("acme/bad", "private", "bad"),
			}, []coreapi.Cluster{
				{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
				{Slug: "bad", PublicUrl: "https://aws-us-east-2.entire.io@evil.com"},
			}, false)
		}
		serve(t)
		stdout, stderr, err := execMirrorList(t)
		require.NoError(t, err)
		require.Contains(t, stdout, "acme/bad", "the mirror is still listed")
		require.Regexp(t, `acme/bad\s+bad\s+`, stdout, "the unresolvable cluster still names itself in CLUSTERS")
		require.NotContains(t, stdout, "evil.com", "a spoofed host must never reach the output")
		require.NotContains(t, stderr, "omitted", "no warning: the row is kept")

		serve(t)
		stdout, _, err = execMirrorList(t, "--json")
		require.NoError(t, err)
		require.Contains(t, stdout, "entire://aws-us-east-2.entire.io/gh/acme/web", "the healthy placement keeps its clone URL")
		require.NotContains(t, stdout, "evil.com", "a spoofed host must never reach a clone URL")
	})
}

// runMirrorListErr is runMirrorList for the error paths (bad --sort column): it
// returns the command error instead of asserting success.
func runMirrorListErr(t *testing.T, args ...string) error {
	t.Helper()
	_, _, err := execMirrorList(t, args...)
	return err
}

// requireOrder asserts each needle appears in s, in the given order. It guards
// presence first: strings.Index returns -1 for an absent needle, so a bare
// index comparison would pass when the earlier needle is missing entirely
// (-1 < anyPresentIndex). This fails loudly instead.
func requireOrder(t *testing.T, s string, needles ...string) {
	t.Helper()
	prev := -1
	for _, n := range needles {
		i := strings.Index(s, n)
		require.GreaterOrEqualf(t, i, 0, "expected %q in output", n)
		require.Greaterf(t, i, prev, "expected %q to come after the previous item", n)
		prev = i
	}
}

// TestRepoMirrorList_FilterSort pins the client-side --name/--owner/--cluster
// filters and --sort applied to the merged `repo mirror list` before rendering,
// so they shape both the table and --json output.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorList_FilterSort(t *testing.T) {
	clusters := []coreapi.Cluster{
		{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
		{Slug: "eu", PublicUrl: "https://eu-west-1.entire.io"},
	}
	repos := func() []coreapi.RepoIndexEntry {
		return []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			onboardedEntry("acme/cli", "public", "us"),
			onboardedEntry("other/api", "public", "eu"),
		}
	}

	t.Run("--name narrows the table by owner/repo substring", func(t *testing.T) {
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--name", "cli")
		require.Contains(t, stdout, "acme/cli")
		require.NotContains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "other/api")
	})

	t.Run("--name matches the owner/repo form shown in the NAME column", func(t *testing.T) {
		// A value copied straight from the displayed NAME column must match the
		// row it came from; filtering on the bare repo name would drop it.
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--name", "acme/web")
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "acme/cli")
		require.NotContains(t, stdout, "other/api")
	})

	t.Run("--owner narrows to a single owner login", func(t *testing.T) {
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--owner", "acme")
		require.Contains(t, stdout, "acme/web")
		require.Contains(t, stdout, "acme/cli")
		require.NotContains(t, stdout, "other/api")
	})

	t.Run("--cluster keeps only mirrors on that cluster and drops candidates", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			onboardedEntry("other/api", "public", "eu"),
			candidateEntry("acme/mkt", "public", coreapi.RepoCandidateAccessAdmin, true),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--cluster", "us")
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "other/api", "eu mirror must be dropped by --cluster us")
		require.NotContains(t, stdout, "acme/mkt", "candidates are cluster-agnostic and dropped by --cluster")
	})

	t.Run("--cluster accepts the public host, not just the slug", func(t *testing.T) {
		// The clone URLs this command prints identify clusters by host, so a
		// host value copied from one must filter the same as its slug ("us").
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			onboardedEntry("other/api", "public", "eu"),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--cluster", "aws-us-east-2.entire.io")
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "other/api", "eu mirror must be dropped by --cluster <us host>")
	})

	t.Run("--status filters by exact status across both row types", func(t *testing.T) {
		mixed := func() []coreapi.RepoIndexEntry {
			return []coreapi.RepoIndexEntry{
				onboardedEntry("acme/web", "private", "us"),                                  // mirror → STATUS "ready"
				candidateEntry("acme/mkt", "public", coreapi.RepoCandidateAccessAdmin, true), // candidate → STATUS "available"
			}
		}
		serveRepoList(t, mixed(), clusters, false)
		stdout, _ := runMirrorList(t, "--status", "ready")
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "acme/mkt", "available candidate dropped by --status ready")

		serveRepoList(t, mixed(), clusters, false)
		stdout, _ = runMirrorList(t, "--status", "available")
		require.Contains(t, stdout, "acme/mkt")
		require.NotContains(t, stdout, "acme/web", "ready mirror dropped by --status available")
	})

	t.Run("--status is case-insensitive", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "public", "us")}, clusters, false)
		stdout, _ := runMirrorList(t, "--status", "READY")
		require.Contains(t, stdout, "acme/web")
	})

	t.Run("--status matches any placement of a mixed-status repo", func(t *testing.T) {
		// One placement failed, the other ready: the STATUS cell reads
		// "mixed", but hunting failures with --status failed must still
		// surface the repo — and --status mixed matches the displayed cell.
		mixedRepo := func() []coreapi.RepoIndexEntry {
			return []coreapi.RepoIndexEntry{
				{FullName: "acme/web", Visibility: "private", Placements: []coreapi.RepoPlacement{
					{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
					{ClusterSlug: "eu", Status: coreapi.RepoPlacementStatusFailed, Mirror: true},
				}},
				onboardedEntry("acme/fine", "private", "us"),
			}
		}
		serveRepoList(t, mixedRepo(), clusters, false)
		stdout, _ := runMirrorList(t, "--status", "failed")
		require.Contains(t, stdout, "acme/web", "a failed placement must surface the repo")
		require.NotContains(t, stdout, "acme/fine")

		serveRepoList(t, mixedRepo(), clusters, false)
		stdout, _ = runMirrorList(t, "--status", "mixed")
		require.Contains(t, stdout, "acme/web", "--status matches the displayed cell too")
		require.NotContains(t, stdout, "acme/fine")
	})

	t.Run("--mirrored and --available split the two row types", func(t *testing.T) {
		both := func() []coreapi.RepoIndexEntry {
			return []coreapi.RepoIndexEntry{
				onboardedEntry("acme/web", "private", "us"),
				candidateEntry("acme/mkt", "public", coreapi.RepoCandidateAccessAdmin, true),
				candidateEntry("alice/x", "private", coreapi.RepoCandidateAccessRead, false), // owner-only candidate
			}
		}
		serveRepoList(t, both(), clusters, false)
		stdout, _ := runMirrorList(t, "--mirrored")
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "acme/mkt", "candidate dropped by --mirrored")
		require.NotContains(t, stdout, "alice/x")

		serveRepoList(t, both(), clusters, false)
		stdout, _ = runMirrorList(t, "--available")
		require.Contains(t, stdout, "acme/mkt")
		require.Contains(t, stdout, "alice/x", "--available keeps every candidate, owner-only included")
		require.NotContains(t, stdout, "acme/web", "mirror dropped by --available")

		serveRepoList(t, both(), clusters, false)
		err := runMirrorListErr(t, "--mirrored", "--available")
		require.Error(t, err, "the two type filters are mutually exclusive")
	})

	t.Run("--access filters candidates and drops mirrors, which have no access", func(t *testing.T) {
		serveRepoList(t, []coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"), // mirror → empty ACCESS
			candidateEntry("acme/adm", "public", coreapi.RepoCandidateAccessAdmin, true),
			candidateEntry("acme/rdo", "public", coreapi.RepoCandidateAccessRead, true),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--access", "admin")
		require.Contains(t, stdout, "acme/adm")
		require.NotContains(t, stdout, "acme/rdo", "read candidate dropped by --access admin")
		require.NotContains(t, stdout, "acme/web", "mirror has no access dimension and is dropped by --access")
	})

	t.Run("--private is tri-state (private / public / all)", func(t *testing.T) {
		mixed := func() []coreapi.RepoIndexEntry {
			return []coreapi.RepoIndexEntry{
				onboardedEntry("acme/secret", "private", "us"),
				onboardedEntry("acme/open", "public", "us"),
			}
		}
		// --private → private only.
		serveRepoList(t, mixed(), clusters, false)
		stdout, _ := runMirrorList(t, "--private")
		require.Contains(t, stdout, "acme/secret")
		require.NotContains(t, stdout, "acme/open", "public dropped by --private")

		// --private=false → public only.
		serveRepoList(t, mixed(), clusters, false)
		stdout, _ = runMirrorList(t, "--private=false")
		require.Contains(t, stdout, "acme/open")
		require.NotContains(t, stdout, "acme/secret", "private dropped by --private=false")

		// omitted → both (the flag is not Changed, so no filtering).
		serveRepoList(t, mixed(), clusters, false)
		stdout, _ = runMirrorList(t)
		require.Contains(t, stdout, "acme/secret")
		require.Contains(t, stdout, "acme/open")
	})

	t.Run("default output is owner/repo sorted", func(t *testing.T) {
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t)
		// acme/cli < acme/web < other/api by owner/repo
		requireOrder(t, stdout, "acme/cli", "acme/web", "other/api")
	})

	t.Run("--sort -name reverses the order", func(t *testing.T) {
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--sort", "-name")
		requireOrder(t, stdout, "other/api", "acme/web", "acme/cli")
	})

	t.Run("--sort name resolves the NAME column by its key", func(t *testing.T) {
		// The NAME header carries an inline "(owner/repo)" display hint, but the
		// sort key is the plain "name" — --sort matches on key, not header.
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--sort", "name")
		requireOrder(t, stdout, "acme/cli", "acme/web", "other/api")
	})

	t.Run("--name applies to --json and keeps [] not null", func(t *testing.T) {
		serveRepoList(t, repos(), clusters, false)
		stdout, _ := runMirrorList(t, "--name", "cli", "--json")
		require.Contains(t, stdout, `"repo": "acme/cli"`)
		require.NotContains(t, stdout, `"repo": "acme/web"`)

		serveRepoList(t, repos(), clusters, false)
		stdout, _ = runMirrorList(t, "--name", "zzz", "--json")
		require.Contains(t, stdout, "[]")
		require.NotContains(t, stdout, "null")
	})

	t.Run("--sort access resolves the ACCESS column by its key", func(t *testing.T) {
		// A candidate-only column, so unit-level TestSortRepoDir (which uses
		// mirror rows) can't reach it: this pins that --sort resolves the kebab
		// key "access" end-to-end. "admin" < "read" ascending, so the admin row
		// sorts before the read row. The sort's tiebreak/direction/whitespace/
		// bad-column semantics are covered by TestSortRepoDir.
		serveRepoList(t, []coreapi.RepoIndexEntry{
			candidateEntry("acme/read-repo", "public", coreapi.RepoCandidateAccessRead, true),
			candidateEntry("acme/admin-repo", "public", coreapi.RepoCandidateAccessAdmin, true),
		}, clusters, false)
		stdout, _ := runMirrorList(t, "--sort", "access")
		requireOrder(t, stdout, "acme/admin-repo", "acme/read-repo")
	})
}

// TestParseHostedGitHubURL pins the one distinction between the two GitHub
// parsers: parseGitHubURL accepts an unqualified `owner/repo` (its callers took
// a GitHub argument and nothing else), while parseHostedGitHubURL requires the
// host to be named, because its caller must not attribute a forge-less pair to
// GitHub — `repo clone` offers such a pair both readings instead.
func TestParseHostedGitHubURL(t *testing.T) {
	t.Parallel()
	hosted := []string{
		"https://github.com/acme/app",
		"https://github.com/acme/app.git",
		"git@github.com:acme/app",
		"git@github.com:acme/app.git",
		"github.com/acme/app",
		"github.com/acme/app.git",
	}
	for _, raw := range hosted {
		owner, repo, err := parseHostedGitHubURL(raw)
		require.NoErrorf(t, err, "parseHostedGitHubURL(%q)", raw)
		require.Equal(t, "acme", owner)
		require.Equal(t, "app", repo)
	}

	// An unqualified pair names no forge; parseGitHubURL still takes it.
	_, _, err := parseHostedGitHubURL("acme/app")
	require.Error(t, err)
	owner, repo, err := parseGitHubURL("acme/app")
	require.NoError(t, err)
	require.Equal(t, "acme", owner)
	require.Equal(t, "app", repo)

	// The dot-only guard applies to the host-qualified form too, so a
	// `github.com/owner/..` ref is not attributed to GitHub either.
	_, _, err = parseHostedGitHubURL("github.com/acme/..")
	require.Error(t, err)
}

// TestParseGitHubURL is ported from entiredb's cmd/entire-repo/cli
// mirror_test.go, since parseGitHubURL was carried over verbatim.
func TestParseGitHubURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "HTTPS", url: "https://github.com/entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTPS with .git", url: "https://github.com/entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH", url: "git@github.com:entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH with .git", url: "git@github.com:entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTP", url: "http://github.com/owner/repo", wantOwner: "owner", wantRepo: "repo"},
		{name: "bare with github.com prefix", url: "github.com/octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare github.com prefix with .git", url: "github.com/octocat/hello-world.git", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare owner/repo", url: "octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare lowercased", url: "OctoCat/Hello-World", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "repo with dot", url: "github.com/octocat/hello.world", wantOwner: "octocat", wantRepo: "hello.world"},
		{name: "repo with underscore", url: "octocat/hello_world", wantOwner: "octocat", wantRepo: "hello_world"},
		{name: "GitLab", url: "https://gitlab.com/owner/repo", wantErr: true},
		{name: "missing repo", url: "https://github.com/owner", wantErr: true},
		{name: "not a URL", url: "not-a-url", wantErr: true},
		{name: "entire URL", url: "entire://host/git/owner/repo", wantErr: true},
		// Parameter-smuggling shapes the tightened owner/repo charset rejects:
		// these would otherwise mutate the audience / probe URL built from
		// owner/repo.
		{name: "repo with query smuggle", url: "octocat/repo?bypass=1", wantErr: true},
		{name: "repo with fragment", url: "octocat/repo#anchor", wantErr: true},
		{name: "owner with at-sign", url: "a@b/repo", wantErr: true},
		{name: "repo with encoded slash", url: "octocat/repo%2fevil", wantErr: true},
		{name: "owner with dot-dot", url: "../repo", wantErr: true},
		{name: "owner with underscore (not a GitHub login)", url: "oct_cat/repo", wantErr: true},
		// Dot-only repo names pass the gitHubRepoPat charset (which allows
		// dots) but would embed a literal "." or ".." in the audience and
		// probe URL — reject at the boundary.
		{name: "dot-only repo", url: "github.com/owner/..", wantErr: true},
		{name: "single-dot repo", url: "github.com/owner/.", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGitHubURL(%q) expected error, got %q/%q", tt.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubURL(%q) unexpected error: %v", tt.url, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("parseGitHubURL(%q) = %q/%q, want %q/%q", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestParseMirrorCloneURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                             string
		raw                              string
		wantCluster, wantOwner, wantRepo string
		wantErr                          bool
	}{
		{name: "github clone URL", raw: "entire://aws-eu-central-1.entire.io/gh/entirehq/entire-api",
			wantCluster: "aws-eu-central-1.entire.io", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "owner and repo lowercased", raw: "entire://c.entire.io/gh/OctoCat/Hello-World",
			wantCluster: "c.entire.io", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "trailing .git is trimmed", raw: "entire://c.entire.io/gh/entireio/cli.git",
			wantCluster: "c.entire.io", wantOwner: "entireio", wantRepo: "cli"},
		{name: "interior dots in repo name are kept", raw: "entire://c.entire.io/gh/entirehq/entire-trails.el",
			wantCluster: "c.entire.io", wantOwner: "entirehq", wantRepo: "entire-trails.el"},
		{name: "wrong scheme", raw: "https://c.entire.io/gh/a/b", wantErr: true},
		{name: "non-gh provider segment", raw: "entire://c.entire.io/git/a/b", wantErr: true},
		{name: "missing repo", raw: "entire://c.entire.io/gh/a", wantErr: true},
		{name: "extra path segment", raw: "entire://c.entire.io/gh/a/b/c", wantErr: true},
		{name: "not a URL", raw: "not-a-url", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster, provider, owner, repo, err := parseMirrorCloneURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMirrorCloneURL(%q) = (%q,%q,%q,%q), want error", tt.raw, cluster, provider, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMirrorCloneURL(%q): %v", tt.raw, err)
			}
			if provider != string(coreapi.CreateMirrorInputBodyProviderGithub) {
				t.Errorf("provider = %q, want github", provider)
			}
			if cluster != tt.wantCluster || owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("= (%q,%q,%q), want (%q,%q,%q)", cluster, owner, repo, tt.wantCluster, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestResolveMirrorRef(t *testing.T) {
	t.Parallel()
	// 26 Crockford base32 chars (no I/L/O/U) so the ULID short-circuit fires.
	const mirrorULID = "0123456789ABCDEFGHJKMNPQRS"
	const otherULID = "0123456789ABCDEFGHJKMNPQRT"
	const cloneURL = "entire://aws-eu-central-1.entire.io/gh/entirehq/entire-api"

	t.Run("ULID passes through without a network call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for a ULID ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		got, err := resolveMirrorRef(context.Background(), c, mirrorULID)
		if err != nil {
			t.Fatalf("resolveMirrorRef: %v", err)
		}
		if got != mirrorULID {
			t.Errorf("resolveMirrorRef = %q, want the ULID unchanged", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ULID ref made %d HTTP calls, want 0", n)
		}
	})

	t.Run("clone URL resolves to the matching mirror's ULID", func(t *testing.T) {
		t.Parallel()
		var gotCluster, gotProvider, gotOwner string
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			gotCluster, gotProvider, gotOwner = q.Get("cluster"), q.Get("provider"), q.Get("owner")
			if err := printJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
				{MirrorId: otherULID, Owner: "entirehq", Repo: "other", ClusterHost: "aws-eu-central-1.entire.io"},
				{MirrorId: mirrorULID, Owner: "entirehq", Repo: "entire-api", ClusterHost: "aws-eu-central-1.entire.io"},
			}}); err != nil {
				t.Errorf("encode mirrors: %v", err)
			}
		})
		got, err := resolveMirrorRef(context.Background(), c, cloneURL)
		if err != nil {
			t.Fatalf("resolveMirrorRef: %v", err)
		}
		if got != mirrorULID {
			t.Errorf("resolveMirrorRef = %q, want %q", got, mirrorULID)
		}
		// The (cluster, provider, owner) narrowing must be server-side; only the
		// repo is matched client-side (ListMirrors has no repo filter).
		if gotCluster != "aws-eu-central-1.entire.io" || gotProvider != string(coreapi.CreateMirrorInputBodyProviderGithub) || gotOwner != "entirehq" {
			t.Errorf("filters = cluster %q provider %q owner %q, want the clone URL's coords", gotCluster, gotProvider, gotOwner)
		}
	})

	t.Run("no matching repo is a friendly error", func(t *testing.T) {
		t.Parallel()
		c, _ := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if err := printJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
				{MirrorId: otherULID, Owner: "entirehq", Repo: "other", ClusterHost: "aws-eu-central-1.entire.io"},
			}}); err != nil {
				t.Errorf("encode mirrors: %v", err)
			}
		})
		_, err := resolveMirrorRef(context.Background(), c, cloneURL)
		if err == nil || !strings.Contains(err.Error(), "no mirror matching") {
			t.Errorf("resolveMirrorRef no match: err = %v, want a \"no mirror matching\" error", err)
		}
	})

	t.Run("unparseable ref errors before any call", func(t *testing.T) {
		t.Parallel()
		c, calls := resolveTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("unexpected HTTP call for an unparseable ref")
			w.WriteHeader(http.StatusInternalServerError)
		})
		if _, err := resolveMirrorRef(context.Background(), c, "not-a-url"); err == nil {
			t.Fatal("resolveMirrorRef unparseable: want an error")
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("unparseable ref made %d HTTP calls, want 0", n)
		}
	})
}

// TestRepoMirrorGet_Routing pins which core `mirror get <ref>` dials. A clone
// URL names its cluster, so it must be resolved on the core fronting that
// cluster (clusterCoreClient), not the active context — the original bug:
// `mirror get entire://<cluster>/…` for a cluster in a federation other than
// the active login failed with "no mirror matching" until the user switched
// contexts. A ULID carries no cluster coordinate and stays on the active
// context; an unparseable ref must error before dialing anything.
//
// Not parallel: swaps the package-level activeCoreClient/clusterCoreClient
// seams.
func TestRepoMirrorGet_Routing(t *testing.T) {
	const mirrorULID = "0123456789ABCDEFGHJKMNPQRS"
	const clusterHost = "eukanuba.partial.to"
	const cloneURL = "entire://" + clusterHost + "/gh/entirehq/librarian"

	// mirrorServer answers both the list (clone-URL resolution) and the
	// GetMirror-by-ULID calls for the librarian mirror.
	mirrorServer := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case mirrorsAPIPath:
				assert.NoError(t, printJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
					{MirrorId: mirrorULID, Owner: "entirehq", Repo: "librarian", ClusterHost: clusterHost},
				}}))
			case mirrorsAPIPath + "/" + mirrorULID:
				assert.NoError(t, printJSON(w, &coreapi.Mirror{
					MirrorId: mirrorULID, Owner: "entirehq", Repo: "librarian", ClusterHost: clusterHost,
					IsPrivate: coreapi.NewOptBool(true),
				}))
			default:
				t.Errorf("unexpected request path %q", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	seamActive := func(t *testing.T, fn func(context.Context) (*coreapi.Client, error)) {
		t.Helper()
		prev := activeCoreClient
		activeCoreClient = fn
		t.Cleanup(func() { activeCoreClient = prev })
	}
	seamCluster := func(t *testing.T, fn func(context.Context, string) (*coreapi.Client, error)) {
		t.Helper()
		prev := clusterCoreClient
		clusterCoreClient = fn
		t.Cleanup(func() { clusterCoreClient = prev })
	}
	runGet := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		cmd := newRepoCmd()
		var out, errW bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errW)
		cmd.SetArgs(append([]string{"mirror", "get"}, args...))
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}

	t.Run("clone URL dials the cluster's core, not the active context", func(t *testing.T) {
		srv := mirrorServer(t)
		seamActive(t, func(context.Context) (*coreapi.Client, error) {
			t.Error("clone-URL get dialed the active context's core")
			return nil, errors.New("wrong core")
		})
		var gotHost string
		seamCluster(t, func(_ context.Context, host string) (*coreapi.Client, error) {
			gotHost = host
			return coreapi.NewWithBearer(srv.URL, "tok")
		})
		out, err := runGet(t, cloneURL)
		require.NoError(t, err)
		require.Equal(t, clusterHost, gotHost, "must resolve on the clone URL's cluster")
		require.Contains(t, out, "entirehq/librarian")
		require.Contains(t, out, cloneURL)
	})

	t.Run("ULID dials the active context", func(t *testing.T) {
		srv := mirrorServer(t)
		seamActive(t, func(context.Context) (*coreapi.Client, error) {
			return coreapi.NewWithBearer(srv.URL, "tok")
		})
		seamCluster(t, func(_ context.Context, host string) (*coreapi.Client, error) {
			t.Errorf("ULID get dialed cluster core %q; a ULID has no cluster coordinate", host)
			return nil, errors.New("wrong core")
		})
		out, err := runGet(t, mirrorULID)
		require.NoError(t, err)
		require.Contains(t, out, "entirehq/librarian")
	})

	t.Run("unparseable ref errors before dialing any core", func(t *testing.T) {
		seamActive(t, func(context.Context) (*coreapi.Client, error) {
			t.Error("unparseable ref dialed the active context's core")
			return nil, errors.New("no dial expected")
		})
		seamCluster(t, func(context.Context, string) (*coreapi.Client, error) {
			t.Error("unparseable ref dialed a cluster core")
			return nil, errors.New("no dial expected")
		})
		_, err := runGet(t, "not-a-url")
		require.Error(t, err)
		require.ErrorContains(t, err, "pass <owner>/<repo>, a mirror ULID, or a clone URL")
	})

	// serveRepoDetail answers the two endpoints the owner/repo form uses: the
	// exact-match /repos?filter= lookup and the cluster catalog.
	serveRepoDetail := func(t *testing.T, repos []coreapi.RepoIndexEntry, clusters []coreapi.Cluster) *string {
		t.Helper()
		var gotFilter string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case testClustersPath:
				assert.NoError(t, printJSON(w, &coreapi.ListClustersOutputBody{Clusters: clusters}))
			case testReposPath:
				gotFilter = r.URL.Query().Get("filter")
				assert.NoError(t, printJSON(w, &coreapi.ListReposOutputBody{Repos: repos}))
			default:
				t.Errorf("unexpected path %q", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		seamActive(t, func(context.Context) (*coreapi.Client, error) {
			return coreapi.NewWithBearer(srv.URL, "tok")
		})
		seamCluster(t, func(_ context.Context, host string) (*coreapi.Client, error) {
			t.Errorf("owner/repo get dialed cluster core %q; it has no cluster coordinate", host)
			return nil, errors.New("wrong core")
		})
		return &gotFilter
	}
	detailClusters := []coreapi.Cluster{
		{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
		{Slug: "eu", PublicUrl: "https://eu-west-1.entire.io"},
	}

	t.Run("owner/repo renders the record view with a per-cluster table", func(t *testing.T) {
		// The drill-down from the grouped `mirror list` NAME cell: identity
		// fields, then one row per cluster mirror with clone URL + status,
		// deterministic (cluster-slug) order — the entry delivers eu-first.
		gotFilter := serveRepoDetail(t, []coreapi.RepoIndexEntry{
			{FullName: "entirehq/entiredb", Visibility: "private", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "eu", Status: coreapi.RepoPlacementStatusFailed, Mirror: true},
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
			}},
		}, detailClusters)

		out, err := runGet(t, "entirehq/entiredb")
		require.NoError(t, err)
		require.Equal(t, "entirehq/entiredb", *gotFilter, "the lookup must be the server-side exact-match filter")
		requireOrder(t, out,
			"Name:", "entirehq/entiredb",
			"Visibility:", "Private",
			"CLUSTER", "CLONE URL", "STATUS",
			"eu", "entire://eu-west-1.entire.io/gh/entirehq/entiredb", "failed",
			"us", "entire://aws-us-east-2.entire.io/gh/entirehq/entiredb", "ready",
		)
	})

	t.Run("owner/repo on a candidate shows access and availability, no table", func(t *testing.T) {
		serveRepoDetail(t, []coreapi.RepoIndexEntry{
			candidateEntry("entirehq/notyet", "private", coreapi.RepoCandidateAccessWrite, true),
		}, detailClusters)

		out, err := runGet(t, "entirehq/notyet")
		require.NoError(t, err)
		requireOrder(t, out,
			"Name:", "entirehq/notyet",
			"Visibility:", "Private",
			"Access:", "write",
			"Not mirrored on any cluster (available).",
		)
		require.NotContains(t, out, "CLONE URL", "a candidate has no placements table")
	})

	t.Run("owner/repo --json emits the list's row shape, placements nested", func(t *testing.T) {
		serveRepoDetail(t, []coreapi.RepoIndexEntry{
			{FullName: "entirehq/entiredb", Visibility: "private", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
			}},
		}, detailClusters)

		out, err := runGet(t, "entirehq/entiredb", "--json")
		require.NoError(t, err)
		var row repoDirRow
		require.NoError(t, json.Unmarshal([]byte(out), &row))
		require.Equal(t, repoDirRow{Repo: "entirehq/entiredb", Private: true, Status: "ready", Placements: []repoDirPlacement{
			{Cluster: "us", Status: "ready", CloneURL: "entire://aws-us-east-2.entire.io/gh/entirehq/entiredb"},
		}}, row)
	})

	t.Run("owner/repo with no matching repo is a friendly error", func(t *testing.T) {
		serveRepoDetail(t, nil, detailClusters)
		_, err := runGet(t, "entirehq/ghost")
		require.Error(t, err)
		require.ErrorContains(t, err, "no repo matching")
	})
}

func TestIsOwnerRepoRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  string
		want bool
	}{
		{ref: "acme/web", want: true},
		{ref: "entire://host/gh/acme/web"},  // clone URL, not this form
		{ref: "acme/web/extra"},             // too many segments
		{ref: "/web"},                       // empty owner
		{ref: "acme/"},                      // empty repo
		{ref: "0123456789ABCDEFGHJKMNPQRS"}, // no separator (ULID shape)
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isOwnerRepoRef(tt.ref))
		})
	}
}

func TestMirrorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.Mirror
		want   []string
	}{
		{
			name:   "private mirror synthesises clone URL",
			mirror: coreapi.Mirror{Owner: "entirehq", Repo: "entire.io", ClusterHost: "aws-us-east-2.entire.io", IsPrivate: coreapi.NewOptBool(true)},
			want:   []string{"entirehq/entire.io", "entire://aws-us-east-2.entire.io/gh/entirehq/entire.io", "Private"},
		},
		{
			name:   "public mirror, unset IsPrivate defaults to Public",
			mirror: coreapi.Mirror{Owner: "octocat", Repo: "hello", ClusterHost: "eu-west-1.entire.io"},
			want:   []string{"octocat/hello", "entire://eu-west-1.entire.io/gh/octocat/hello", "Public"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mirrorRow(tt.mirror)
			if len(got) != len(tt.want) {
				t.Fatalf("mirrorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mirrorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRepoDirCells(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		row  repoDirRow
		want []string
	}{
		{
			name: "mirror row: clusters + status, access dashed",
			row: repoDirRow{Repo: "acme/web", Private: true, Status: "ready", Placements: []repoDirPlacement{
				{Cluster: "us", Status: "ready", CloneURL: "entire://h/gh/acme/web"},
			}},
			want: []string{"acme/web", "us", "Private", "ready", "-"},
		},
		{
			name: "multi-cluster mirror row joins its slugs in one cell",
			row: repoDirRow{Repo: "acme/web", Private: true, Status: "ready", Placements: []repoDirPlacement{
				{Cluster: "us", Status: "ready"},
				{Cluster: "eu", Status: "ready"},
			}},
			want: []string{"acme/web", "us, eu", "Private", "ready", "-"},
		},
		{
			name: "candidate row: access + availability, clusters dashed",
			row:  repoDirRow{Repo: "acme/mkt", Private: false, Status: "available", Access: "admin"},
			want: []string{"acme/mkt", "-", "Public", "available", "admin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, repoDirCells(tt.row))
		})
	}
}

// TestRepoStatusColor pins the STATUS→color mapping shared by the list and
// the get views: lifecycle states get a color, owner-only and unknown values
// stay uncolored. The concrete styles come from statusStyles, tested with the
// rest of the styling infra; here only the routing is observable.
func TestRepoStatusColor(t *testing.T) {
	t.Parallel()
	st := newStatusStyles(io.Discard)
	for _, status := range []string{"ready", "available", "processing", "mixed", "failed", "suspended"} {
		if _, ok := repoStatusColor(st, status); !ok {
			t.Errorf("repoStatusColor(%q) ok = false, want a lifecycle color", status)
		}
	}
	for _, status := range []string{"owner-only", "", "unheard-of"} {
		if _, ok := repoStatusColor(st, status); ok {
			t.Errorf("repoStatusColor(%q) ok = true, want uncolored", status)
		}
	}
}

// TestStyledCellsDisabledGate pins that the styled cell/header wrappers are
// exact identities when color is off (pipes, tests, NO_COLOR): agents and
// scripts must see the bare text, byte for byte.
func TestStyledCellsDisabledGate(t *testing.T) {
	t.Parallel()
	st := newStatusStyles(io.Discard) // never a TTY → color disabled
	require.False(t, st.colorEnabled)

	row := repoDirRow{Repo: "acme/web", Private: true, Status: "ready", Placements: []repoDirPlacement{
		{Cluster: "us", Status: "ready", CloneURL: "entire://h/gh/acme/web"},
	}}
	require.Equal(t, repoDirCells(row), repoDirCellsStyled(st)(row))

	headers := columnHeaders(repoDirColumns)
	require.Equal(t, headers, styledHeaders(st, headers))
}

func TestClusterHostBySlug(t *testing.T) {
	t.Parallel()
	m := clusterHostBySlug([]coreapi.Cluster{
		{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
		{Slug: "bare", PublicUrl: "eu-west-1.entire.io"}, // no scheme: normalized safely
	})
	require.Equal(t, "aws-us-east-2.entire.io", m["us"])
	require.Equal(t, "eu-west-1.entire.io", m["bare"])

	// A publicUrl that smuggles a host via userinfo is rejected and omitted, so
	// its slug has no entry — a mirror there renders a dashed clone URL, never
	// a spoofed one.
	m = clusterHostBySlug([]coreapi.Cluster{
		{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io@evil.com"},
	})
	require.Empty(t, m)
}

func TestBuildRepoDir(t *testing.T) {
	t.Parallel()
	hosts := map[string]string{"us": "aws-us-east-2.entire.io"}

	t.Run("groups mirrors one row per repo and maps candidates", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			onboardedEntry("acme/web", "private", "us"),
			candidateEntry("acme/mkt", "public", coreapi.RepoCandidateAccessAdmin, true),
			candidateEntry("alice/x", "private", coreapi.RepoCandidateAccessRead, false),
		}, hosts)
		require.Equal(t, []repoDirRow{
			{Repo: "acme/web", Private: true, Status: "ready", Placements: []repoDirPlacement{
				{Cluster: "us", Status: "ready", CloneURL: "entire://aws-us-east-2.entire.io/gh/acme/web"},
			}},
			{Repo: "acme/mkt", Private: false, Status: "available", Access: "admin"},
			{Repo: "alice/x", Private: true, Status: "owner-only", Access: "read"},
		}, rows)
	})

	t.Run("a multi-cell mirror stays one row, its placements nested in order", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			onboardedMulti("acme/web", "private", "us", "eu"),
		}, map[string]string{"us": "aws-us-east-2.entire.io", "eu": "eu-west-1.entire.io"})
		require.Len(t, rows, 1)
		require.Equal(t, []repoDirPlacement{
			{Cluster: "us", Status: "ready", CloneURL: "entire://aws-us-east-2.entire.io/gh/acme/web"},
			{Cluster: "eu", Status: "ready", CloneURL: "entire://eu-west-1.entire.io/gh/acme/web"},
		}, rows[0].Placements)
		require.Equal(t, "ready", rows[0].Status, "placements agree, so the row carries their shared status")
	})

	t.Run("disagreeing placement statuses roll up to mixed", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			{FullName: "acme/web", Visibility: "private", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
				{ClusterSlug: "eu", Status: coreapi.RepoPlacementStatusFailed, Mirror: true},
			}},
		}, hosts)
		require.Len(t, rows, 1)
		require.Equal(t, repoDirStatusMixed, rows[0].Status)
		require.Equal(t, "failed", rows[0].Placements[1].Status, "per-placement statuses stay exact")
	})

	t.Run("unknown cluster slug keeps the placement with an empty clone URL", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			onboardedEntry("a/b", "public", "ghost"),
		}, map[string]string{})
		require.Len(t, rows, 1)
		require.Equal(t, []repoDirPlacement{
			{Cluster: "ghost", Status: "ready"},
		}, rows[0].Placements, "unresolved host → no clone URL, but the slug still names the placement")
	})

	t.Run("native (non-mirror) placements are dropped, not given fabricated clone URLs", func(t *testing.T) {
		t.Parallel()
		// A repo created by `entire repo create` is placed but not mirrored;
		// it must not appear in the mirror directory with a fake gh clone URL.
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			nativeEntry("acme/native", "private", "us"),
			onboardedEntry("acme/web", "public", "us"),
		}, hosts)
		require.Equal(t, []repoDirRow{
			{Repo: "acme/web", Private: false, Status: "ready", Placements: []repoDirPlacement{
				{Cluster: "us", Status: "ready", CloneURL: "entire://aws-us-east-2.entire.io/gh/acme/web"},
			}},
		}, rows, "only the mirror row survives; the native repo is dropped")
	})

	t.Run("a repo with mixed placements keeps only its mirror placements", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir([]coreapi.RepoIndexEntry{
			{FullName: "acme/web", Visibility: "public", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: false},
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
			}},
		}, hosts)
		require.Equal(t, []repoDirRow{
			{Repo: "acme/web", Private: false, Status: "ready", Placements: []repoDirPlacement{
				{Cluster: "us", Status: "ready", CloneURL: "entire://aws-us-east-2.entire.io/gh/acme/web"},
			}},
		}, rows)
	})
}

func TestClusterArg(t *testing.T) {
	t.Parallel()
	if got := clusterArg([]string{"github.com/o/r", "eu-west-1.entire.io"}); got != "eu-west-1.entire.io" {
		t.Errorf("explicit cluster = %q, want eu-west-1.entire.io", got)
	}
	if got := clusterArg([]string{"github.com/o/r"}); got != defaultClusterHost {
		t.Errorf("omitted cluster = %q, want default %q", got, defaultClusterHost)
	}
}

// TestResolveOneShotClusterHost_NonInteractive locks in that a non-interactive
// `repo mirror create <github-url>` keeps the fixed defaultClusterHost without
// dialing the control plane — scripts must get a stable, offline default. Under
// `go test`, CanPromptInteractively() is false, so this exercises exactly the
// script path; no server is running, so any catalog fetch would error.
func TestResolveOneShotClusterHost_NonInteractive(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	got, err := resolveOneShotClusterHost(cmd)
	if err != nil {
		t.Fatalf("resolveOneShotClusterHost() error = %v", err)
	}
	if got != defaultClusterHost {
		t.Errorf("resolveOneShotClusterHost() = %q, want default %q", got, defaultClusterHost)
	}
}

func TestClusterArgAt(t *testing.T) {
	t.Parallel()
	// clusterArgAt reads the cluster from the optional positional at an
	// arbitrary index — here index 2, after two leading positionals.
	if got := clusterArgAt([]string{"github.com/o/r", "github:alice", "eu-west-1.entire.io"}, 2); got != "eu-west-1.entire.io" {
		t.Errorf("explicit cluster = %q, want eu-west-1.entire.io", got)
	}
	if got := clusterArgAt([]string{"github.com/o/r", "github:alice"}, 2); got != defaultClusterHost {
		t.Errorf("omitted cluster = %q, want default %q", got, defaultClusterHost)
	}
}

func TestMirrorCollaboratorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   coreapi.MirrorCollaborator
		want []string
	}{
		{
			name: "resolved handle",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Handle: coreapi.NewOptString("github:alice"), Role: "writer"},
			want: []string{"github:alice", "writer", "01ACCT"},
		},
		{
			name: "no handle falls back to dash",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Role: "reader"},
			want: []string{"-", "reader", "01ACCT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mirrorCollaboratorRow(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("mirrorCollaboratorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mirrorCollaboratorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateClusterHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "default cluster", host: defaultClusterHost},
		{name: "other region", host: "eu-west-1.entire.io"},
		{name: "single label", host: "localhost"},
		{name: "host with port", host: "localhost:8080"},
		{name: "ipv4", host: "10.0.0.1"},
		{name: "ipv4 with port", host: "10.0.0.1:8080"},
		// IPv6 takes a different path through validateClusterHost: the
		// host must be bracketed for url.Parse to round-trip, and
		// u.Hostname() strips the brackets before net.ParseIP sees it.
		{name: "ipv6 with port", host: "[::1]:8080"},
		// The token-leak primitive: userinfo demotes the real cluster so the
		// request (and basic-auth token) targets evil.com.
		{name: "userinfo smuggle", host: "aws-us-east-2.entire.io@evil.com", wantErr: true},
		{name: "path smuggle", host: "aws-us-east-2.entire.io/../evil", wantErr: true},
		{name: "query smuggle", host: "aws-us-east-2.entire.io?x=1", wantErr: true},
		{name: "fragment smuggle", host: "aws-us-east-2.entire.io#x", wantErr: true},
		{name: "scheme prefix", host: "https://evil.com", wantErr: true},
		{name: "empty", host: "", wantErr: true},
		{name: "whitespace", host: "   ", wantErr: true},
		{name: "leading hyphen label", host: "-bad.entire.io", wantErr: true},
		{name: "space in host", host: "evil .com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateClusterHost(tt.host)
			if tt.wantErr && err == nil {
				t.Errorf("validateClusterHost(%q) = nil, want error", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateClusterHost(%q) = %v, want nil", tt.host, err)
			}
		})
	}
}

// TestRemoveMirror covers `repo mirror remove`'s DeleteMirror call:
// removeMirror dials via runCoreForCluster, which the activeCoreClient test
// seam does not intercept, so this drives the helper directly against an
// httptest server the way the createAndAwaitMirror tests do.
func TestRemoveMirror(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(srv.Close)
		c, err := coreapi.NewWithBearer(srv.URL, "tok")
		require.NoError(t, err)

		var out bytes.Buffer
		err = removeMirror(t.Context(), &out, c, "octocat", "hello-world", "aws-us-east-2.entire.io")
		require.NoError(t, err)
		require.Contains(t, out.String(), "✓ Removed mirror github.com/octocat/hello-world from aws-us-east-2.entire.io")
	})

	t.Run("decoded 404 appends server detail", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeNotFoundProblem(t, w)
		}))
		t.Cleanup(srv.Close)
		c, err := coreapi.NewWithBearer(srv.URL, "tok")
		require.NoError(t, err)

		var out bytes.Buffer
		err = removeMirror(t.Context(), &out, c, "octocat", "hello-world", "aws-us-east-2.entire.io")
		require.Error(t, err)
		require.ErrorContains(t, err, "may be on a different cluster")
		require.ErrorContains(t, err, "(server: not found)")
		require.Empty(t, out.String())
	})

	t.Run("non-404 server error passes through the problem detail", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			if _, werr := w.Write([]byte(`{"status":500,"detail":"boom"}`)); werr != nil {
				t.Errorf("write problem: %v", werr)
			}
		}))
		t.Cleanup(srv.Close)
		c, err := coreapi.NewWithBearer(srv.URL, "tok")
		require.NoError(t, err)

		var out bytes.Buffer
		err = removeMirror(t.Context(), &out, c, "octocat", "hello-world", "aws-us-east-2.entire.io")
		require.Error(t, err)
		require.Equal(t, "boom", coreapi.APIError(err))
		require.Empty(t, out.String())
	})
}

// repoDirKeys renders each row as "repo@clusters" so a sorted slice's order
// (including the CLUSTERS-cell tiebreak) is asserted in one line.
func repoDirKeys(rows []repoDirRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Repo + "@" + repoDirClusters(r)
	}
	return out
}

// placedOn builds the nested placements for a sort fixture row, one ready
// placement per slug.
func placedOn(slugs ...string) []repoDirPlacement {
	out := make([]repoDirPlacement, len(slugs))
	for i, s := range slugs {
		out[i] = repoDirPlacement{Cluster: s, Status: "ready"}
	}
	return out
}

func TestSortRepoDir(t *testing.T) {
	t.Parallel()

	// Two same-owner repos plus a row colliding with one of them on every
	// non-name column, so both the primary key and the name/clusters tiebreak
	// are observable.
	base := func() []repoDirRow {
		return []repoDirRow{
			{Repo: "acme/web", Private: true, Status: "ready", Placements: placedOn("us", "eu")},
			{Repo: "beta/api", Private: false, Status: "ready", Placements: placedOn("eu")},
			{Repo: "acme/api", Private: false, Status: "ready", Placements: placedOn("us")},
		}
	}

	t.Run("default sorts by repo name ascending", func(t *testing.T) {
		t.Parallel()
		r := base()
		require.NoError(t, sortRepoDir(r, ""))
		require.Equal(t, []string{
			"acme/api@us",
			"acme/web@us, eu",
			"beta/api@eu",
		}, repoDirKeys(r))
	})

	t.Run("-name reverses the ordering", func(t *testing.T) {
		t.Parallel()
		r := base()
		require.NoError(t, sortRepoDir(r, "-name"))
		require.Equal(t, []string{
			"beta/api@eu",
			"acme/web@us, eu",
			"acme/api@us",
		}, repoDirKeys(r))
	})

	t.Run("non-name column sort falls back to the name tiebreak", func(t *testing.T) {
		t.Parallel()
		// beta/api and acme/api collide on "ready"+Public; within the tie the
		// order must fall back to repo name, not the input order.
		r := base()
		require.NoError(t, sortRepoDir(r, "visibility"))
		require.Equal(t, []string{
			// "private" sorts before "public"; the Private row leads, the
			// Public group follows ordered by repo name.
			"acme/web@us, eu",
			"acme/api@us",
			"beta/api@eu",
		}, repoDirKeys(r))
	})

	t.Run("clusters sorts by the joined CLUSTERS cell", func(t *testing.T) {
		t.Parallel()
		r := base()
		require.NoError(t, sortRepoDir(r, "clusters"))
		require.Equal(t, []string{
			// "eu" < "us" < "us, eu"; the eu-only row leads.
			"beta/api@eu",
			"acme/api@us",
			"acme/web@us, eu",
		}, repoDirKeys(r))
	})

	t.Run("whitespace spec parses direction from the trimmed spec", func(t *testing.T) {
		t.Parallel()
		r := base()
		require.NoError(t, sortRepoDir(r, " -name"))
		require.Equal(t, []string{
			"beta/api@eu",
			"acme/web@us, eu",
			"acme/api@us",
		}, repoDirKeys(r))
	})

	t.Run("unknown column errors naming valid columns", func(t *testing.T) {
		t.Parallel()
		err := sortRepoDir(base(), "nope")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown sort column")
		require.Contains(t, err.Error(), "name")
	})
}

// TestRepoMirrorList_PageMode pins single-page cursor passthrough on the
// merged directory: one /repos request per call, an --json envelope carrying
// nextPageToken, a table resume hint on stderr, and the (experimental)
// client-side filters/sort applying to just that page.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorList_PageMode(t *testing.T) {
	clusters := []coreapi.Cluster{{Slug: "us", PublicUrl: "https://aws-us-east-2.entire.io"}}

	t.Run("--page-size makes one request and hints the resume token", func(t *testing.T) {
		recCh := serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")},
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t, "--page-size", "1")
		rec := <-recCh
		require.Equal(t, "1", rec.query.Get("pageSize"))
		select {
		case rec := <-recCh:
			t.Fatalf("page mode must make exactly one request, got a second with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "tail/end")
		require.Contains(t, stderr, "--page-token p2")
	})

	t.Run("--json page mode emits the envelope and filters apply to the page", func(t *testing.T) {
		serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos: []coreapi.RepoIndexEntry{
					onboardedEntry("acme/web", "private", "us"),
					candidateEntry("acme/marketing", "public", coreapi.RepoCandidateAccessAdmin, true),
				},
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, _ := runMirrorList(t, "--json", "--page-size", "2", "--status", "available")
		var envelope struct {
			Items         []repoDirRow `json:"items"`
			NextPageToken string       `json:"nextPageToken"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &envelope))
		require.Len(t, envelope.Items, 1, "the client-side --status filter applies to the fetched page")
		require.Equal(t, "acme/marketing", envelope.Items[0].Repo)
		require.Equal(t, "p2", envelope.NextPageToken, "the cursor survives local filtering")
	})

	t.Run("an explicitly empty --page-token still selects page mode", func(t *testing.T) {
		// A script's resume loop naturally starts with an empty cursor; the
		// output shape must not flip to the multi-page walk on the token's
		// value — page mode is opted into by setting the flag.
		recCh := serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")},
				NextPageToken: coreapi.NewOptString("p2"),
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		stdout, stderr := runMirrorList(t, "--page-token", "")
		rec := <-recCh
		require.Empty(t, rec.query.Get("pageToken"), "an empty cursor addresses the first page")
		select {
		case rec := <-recCh:
			t.Fatalf("page mode must make exactly one request, got a second with pageToken=%q", rec.query.Get("pageToken"))
		default:
		}
		require.Contains(t, stdout, "acme/web")
		require.NotContains(t, stdout, "tail/end")
		require.Contains(t, stderr, "--page-token p2")
	})

	t.Run("a truncated page with no cursor warns on stderr, --json included", func(t *testing.T) {
		// The server said the directory was cut short and offered no cursor to
		// continue from: the page must not read as complete, in either output
		// mode — a script acting on silently truncated data is the worst
		// outcome, and stderr never corrupts the stdout JSON.
		entries := []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")}
		serveRepoList(t, entries, clusters, true)
		_, stderr := runMirrorList(t, "--page-size", "5")
		require.Contains(t, stderr, "truncated")

		serveRepoList(t, entries, clusters, true)
		stdout, stderr := runMirrorList(t, "--json", "--page-size", "5")
		require.Contains(t, stderr, "truncated")
		require.Contains(t, stdout, `"items"`, "the envelope still renders")
	})

	t.Run("a truncated page with a cursor to continue from does not warn", func(t *testing.T) {
		// A capped page the cursor can walk past leaves nothing unreachable;
		// the resume hint already tells the caller how to continue.
		serveRepoListPaged(t, []coreapi.ListReposOutputBody{
			{
				Repos:         []coreapi.RepoIndexEntry{onboardedEntry("acme/web", "private", "us")},
				NextPageToken: coreapi.NewOptString("p2"),
				Truncated:     true,
			},
			{
				Repos: []coreapi.RepoIndexEntry{onboardedEntry("tail/end", "private", "us")},
			},
		}, clusters)
		_, stderr := runMirrorList(t, "--page-size", "1")
		require.NotContains(t, stderr, "truncated")
		require.Contains(t, stderr, "--page-token p2")
	})

	t.Run("page mode excludes the walk flags", func(t *testing.T) {
		serveRepoListPaged(t, nil, clusters)
		for _, combo := range [][]string{
			{"--page-size", "5", "--all"},
			{"--page-token", "p2", "--limit", "3"},
		} {
			err := runMirrorListErr(t, combo...)
			require.Error(t, err, "combo %v must be rejected", combo)
		}
	})
}

// TestRepoMirrorList_FilterGroupNote pins the fetched-window caveat: stated
// once, at the Filtering & Sorting group level (between the section header
// and its first flag), telling the user what the flags apply to and how to
// widen it. Every flag in the group runs on the client today (/repos offers
// the server no filter or sort params); a flag that gains a server-side
// implementation must leave the group.
func TestRepoMirrorList_FilterGroupNote(t *testing.T) {
	stdout, _, err := execMirrorList(t, "--help")
	require.NoError(t, err)
	const note = "Applied only to the fetched rows; combine with --all to filter/sort the complete mirror list."
	require.Equal(t, 1, strings.Count(stdout, note), "the window note appears exactly once, at group level")
	idx := strings.Index(stdout, "Filtering & Sorting Flags:")
	require.GreaterOrEqual(t, idx, 0, "expected a Filtering & Sorting Flags section")
	requireOrder(t, stdout[idx:],
		"Filtering & Sorting Flags:", note, "--access",
	)
}

// TestRepoMirrorList_GroupedFlagHelp pins the grouped help layout: flags are
// presented by usage — navigation (how much is fetched / which page),
// filtering & sorting (client-side, window-scoped), formatting — so the
// window semantics are legible at a glance.
func TestRepoMirrorList_GroupedFlagHelp(t *testing.T) {
	stdout, _, err := execMirrorList(t, "--help")
	require.NoError(t, err)
	// Anchor past the Long text (which mentions flags by name) so the order
	// assertions see only the flag sections.
	idx := strings.Index(stdout, "Navigation Flags:")
	require.GreaterOrEqual(t, idx, 0, "expected a Navigation Flags section")
	requireOrder(t, stdout[idx:],
		"Navigation Flags:", "--all", "--limit", "--page-size", "--page-token",
		"Filtering & Sorting Flags:", "--access", "--available", "--cluster", "--mirrored", "--name", "--owner", "--private", "--sort", "--status",
		"Formatting Flags:", "--json", "--no-pager",
	)
}
