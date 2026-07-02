package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
		status, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
	})

	t.Run("processing then ready keeps polling", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{
			coreapi.MirrorStatusProcessing, coreapi.MirrorStatusProcessing, coreapi.MirrorStatusReady,
		}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
		require.GreaterOrEqual(t, f.calls, 3)
	})

	t.Run("failed returns errMirrorCloneFailed", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusFailed}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.ErrorIs(t, err, errMirrorCloneFailed)
		require.Equal(t, coreapi.MirrorStatusFailed, status)
	})

	t.Run("suspended returns errMirrorSuspended", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusSuspended}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.ErrorIs(t, err, errMirrorSuspended)
		require.Equal(t, coreapi.MirrorStatusSuspended, status)
	})

	t.Run("never-ready times out", func(t *testing.T) {
		f := &fakeMirrorGetter{statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusProcessing}}
		_, err := awaitMirrorReady(ctx, f, "m", 20*time.Millisecond, nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("transient errors are tolerated, then ready", func(t *testing.T) {
		// Fewer consecutive errors than the cap, so the poll rides them out.
		f := &fakeMirrorGetter{errsBefore: maxConsecutivePollErrors - 1, statuses: []coreapi.MirrorStatus{coreapi.MirrorStatusReady}}
		status, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, status)
	})

	t.Run("persistent errors give up after the cap", func(t *testing.T) {
		f := &fakeMirrorGetter{err: errors.New("boom")}
		_, err := awaitMirrorReady(ctx, f, "m", time.Second, nil)
		require.ErrorContains(t, err, "poll mirror status")
		require.Equal(t, maxConsecutivePollErrors, f.calls, "should stop at the cap, not spin to the deadline")
	})
}

// serveMirrorCreate stands up a control plane that answers POST /mirrors with
// the given CreatedMirror (or a 500 when createErr) and GET /mirrors/{id} with
// a Ready status, then points createAndAwaitMirror's client at it. It records
// the ordered request paths so tests can assert create-before-poll sequencing.
func serveMirrorCreate(t *testing.T, created *coreapi.CreatedMirror, createErr bool) (*coreapi.Client, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == mirrorsAPIPath:
			if createErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			if err := writeJSON(w, created); err != nil {
				t.Errorf("encode created response: %v", err)
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/mirrors/"):
			m := &coreapi.Mirror{}
			m.Status = coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)
			if err := writeJSON(w, m); err != nil {
				t.Errorf("encode mirror response: %v", err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)
	return c, &paths
}

// TestCreateAndAwaitMirror_OnCreated pins the onCreated callback contract: it
// delimits the placing vs cloning phases, so it must fire exactly once on
// CreateMirror success (before any clone polling / onStatus), and never on a
// CreateMirror error.
//
// Not parallel: shortens the package-level mirrorPollInterval.
func TestCreateAndAwaitMirror_OnCreated(t *testing.T) {
	prev := mirrorPollInterval
	mirrorPollInterval = time.Millisecond
	t.Cleanup(func() { mirrorPollInterval = prev })
	ctx := t.Context()

	mk := func() *coreapi.CreatedMirror {
		return &coreapi.CreatedMirror{Created: true, MirrorId: "m1", MirrorUrl: "entire://c/gh/o/r"}
	}

	t.Run("fires once before onStatus on success", func(t *testing.T) {
		c, _ := serveMirrorCreate(t, mk(), false)
		var events []string
		outcome, err := createAndAwaitMirror(ctx, c, "o", "r", "c", false, time.Second,
			func(m *coreapi.CreatedMirror) {
				require.Equal(t, "m1", m.MirrorId, "onCreated receives the create response")
				events = append(events, "created")
			},
			func(coreapi.MirrorStatus) { events = append(events, "status") },
		)
		require.NoError(t, err)
		require.Equal(t, coreapi.MirrorStatusReady, outcome.status)
		require.NotEmpty(t, events)
		require.Equal(t, "created", events[0], "onCreated must fire before any onStatus")
		require.Equal(t, 1, countEq(events, "created"), "onCreated fires exactly once")
	})

	t.Run("does not fire on CreateMirror error", func(t *testing.T) {
		c, _ := serveMirrorCreate(t, nil, true)
		fired := 0
		outcome, err := createAndAwaitMirror(ctx, c, "o", "r", "c", false, time.Second,
			func(*coreapi.CreatedMirror) { fired++ }, nil)
		require.Error(t, err)
		require.Nil(t, outcome.created)
		require.Zero(t, fired, "onCreated must not fire when create fails")
	})

	t.Run("fires once even with no-wait (no polling)", func(t *testing.T) {
		c, paths := serveMirrorCreate(t, mk(), false)
		fired := 0
		_, err := createAndAwaitMirror(ctx, c, "o", "r", "c", true, time.Second,
			func(*coreapi.CreatedMirror) { fired++ }, nil)
		require.NoError(t, err)
		require.Equal(t, 1, fired)
		require.Equal(t, []string{mirrorsAPIPath}, *paths, "no-wait must not poll GetMirror")
	})

	t.Run("suspended placement short-circuits without polling or error", func(t *testing.T) {
		suspended := &coreapi.CreatedMirror{MirrorId: "m1", MirrorUrl: "entire://c/gh/o/r", Suspended: true}
		c, paths := serveMirrorCreate(t, suspended, false)
		fired := 0
		outcome, err := createAndAwaitMirror(ctx, c, "o", "r", "c", false, time.Second,
			func(*coreapi.CreatedMirror) { fired++ }, nil)
		require.NoError(t, err, "an admin-suspended placement is non-fatal")
		require.Equal(t, 1, fired, "onCreated still fires for a suspended placement")
		require.False(t, outcome.polled, "a suspended placement is never polled for readiness")
		require.Equal(t, []string{mirrorsAPIPath}, *paths, "suspended must not poll GetMirror")
	})
}

func countEq(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}

// TestReportOneShotMirror exercises the one-shot create's presentation across
// the shared lifecycle outcomes — the branching finishMirrorCreate used to own,
// now driven by mirrorCreateOutcome (and shared with the wizard).
func TestReportOneShotMirror(t *testing.T) {
	t.Parallel()
	const id = "01KS6KFJR2XS6PZ188MVYE07AN"
	const mirrorURL = "entire://eu-west-1.entire.io/gh/octocat/hello-world"
	mk := func(created, empty bool) *coreapi.CreatedMirror {
		return &coreapi.CreatedMirror{Created: created, Empty: empty, MirrorId: id, MirrorUrl: mirrorURL}
	}

	t.Run("create failure surfaces with nothing printed", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		wantErr := errors.New("boom")
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{}, wantErr)
		require.ErrorIs(t, err, wantErr)
		require.Empty(t, out.String())
	})

	t.Run("empty upstream prints nothing-to-clone", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{created: mk(true, true)}, nil)
		require.NoError(t, err)
		require.Contains(t, out.String(), "Registered mirror "+id)
		require.Contains(t, out.String(), "nothing to clone")
	})

	t.Run("no-wait prints in-progress hint", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{created: mk(true, false)}, nil)
		require.NoError(t, err)
		require.Contains(t, out.String(), "still be in progress")
	})

	t.Run("ready prints clone hint", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(true, false), status: coreapi.MirrorStatusReady, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, nil)
		require.NoError(t, err)
		require.Contains(t, out.String(), "git clone "+mirrorURL)
	})

	t.Run("suspended surfaces support guidance as SilentError", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(false, false), status: coreapi.MirrorStatusSuspended, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, errMirrorSuspended)
		var silent *SilentError
		require.ErrorAs(t, err, &silent)
		require.Contains(t, errW.String(), "Contact support")
		require.NotContains(t, errW.String(), "entire-core")
		require.NotContains(t, out.String(), "git clone")
	})

	t.Run("suspended placement warns after the placement and succeeds", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: false, MirrorId: id, MirrorUrl: mirrorURL, Suspended: true}
		err := reportOneShotMirror(&out, &errW, mirrorCreateOutcome{created: created}, nil)
		require.NoError(t, err, "a suspended re-create is a non-fatal warning")
		require.Contains(t, out.String(), "Mirror exists ("+id, "the placement is still echoed")
		require.Contains(t, errW.String(), "WARNING: this mirror has been suspended by an admin and won't be usable.")
		require.NotContains(t, out.String(), "git clone")
		require.NotContains(t, out.String(), "still be in progress")
	})

	t.Run("failed returns an error naming the mirror", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		outcome := mirrorCreateOutcome{created: mk(true, false), status: coreapi.MirrorStatusFailed, polled: true}
		err := reportOneShotMirror(&out, &errW, outcome, errMirrorCloneFailed)
		require.Error(t, err)
		require.Contains(t, err.Error(), id)
	})

	t.Run("timeout propagates the wait error", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		wantErr := errors.New("timed out waiting for initial clone")
		outcome := mirrorCreateOutcome{created: mk(true, false), status: coreapi.MirrorStatusProcessing, polled: true}
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

// serveMirrorList stands up a fake control-plane that records the inbound
// request and answers /mirrors and /mirrors/available with the given payloads,
// then points the active-context client seam at it for the duration of the
// test. Each request is delivered on the returned channel: receiving from it
// after the command runs is the happens-before edge that synchronises the
// handler-goroutine writes with the test-goroutine reads — HTTP completion
// alone is not an edge the race detector recognises (see
// TestBearerOnlySource_NoCookieOnTheWire). Buffered so the handler never
// blocks on the send.
func serveMirrorList(t *testing.T, mirrors []coreapi.Mirror, available []coreapi.AvailableMirror) <-chan recordedRequest {
	t.Helper()
	recCh := make(chan recordedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/mirrors/available":
			if err := writeJSON(w, &coreapi.ListAvailableMirrorsOutputBody{Available: available}); err != nil {
				t.Errorf("encode available response: %v", err)
			}
		case mirrorsAPIPath:
			if err := writeJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: mirrors}); err != nil {
				t.Errorf("encode mirrors response: %v", err)
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		recCh <- recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return recCh
}

// runMirrorList executes `repo mirror list` with args against the fake server,
// returning stdout (the table/JSON) and stderr (the routing banner).
func runMirrorList(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := newRepoMirrorListCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	return out.String(), errOut.String()
}

// TestRepoMirrorList_ShowAvailableRouting locks in the flag-driven branch of
// `repo mirror list`: --show-available must hit the /mirrors/available endpoint
// with the available-repo columns and its own banner, while the default lists
// existing mirrors from /mirrors. --owner flows into the query on both paths;
// --cluster/--provider apply only to the existing-mirror path. These are the
// behaviors the seam was added to pin — the per-row formatting is covered by
// TestMirrorRow / TestAvailableMirrorRow.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorList_ShowAvailableRouting(t *testing.T) {
	t.Run("--show-available routes to /mirrors/available with available columns", func(t *testing.T) {
		recCh := serveMirrorList(t,
			[]coreapi.Mirror{{Owner: "acme", Repo: "web", ClusterHost: "aws-us-east-2.entire.io"}},
			[]coreapi.AvailableMirror{{Owner: "acme", Repo: "web", Access: "write", Status: "available"}},
		)
		stdout, stderr := runMirrorList(t, "--show-available")
		rec := <-recCh

		require.Equal(t, http.MethodGet, rec.method)
		require.Equal(t, "/api/v1/mirrors/available", rec.path)
		require.Contains(t, stderr, "Listing repos you could mirror")
		// Available columns, not the existing-mirror "CLONE URL" view.
		require.Contains(t, stdout, "ACCESS")
		require.Contains(t, stdout, "STATUS")
		require.Contains(t, stdout, "acme/web")
		require.Contains(t, stdout, "available")
		require.NotContains(t, stdout, "CLONE URL")
	})

	t.Run("default lists existing mirrors from /mirrors", func(t *testing.T) {
		recCh := serveMirrorList(t,
			[]coreapi.Mirror{{Owner: "acme", Repo: "web", ClusterHost: "aws-us-east-2.entire.io"}},
			nil,
		)
		stdout, stderr := runMirrorList(t)
		rec := <-recCh

		require.Equal(t, mirrorsAPIPath, rec.path)
		require.Contains(t, stderr, "Listing mirrors on")
		require.Contains(t, stdout, "CLONE URL")
		require.Contains(t, stdout, "entire://aws-us-east-2.entire.io/gh/acme/web")
	})

	t.Run("--owner flows into the available query", func(t *testing.T) {
		recCh := serveMirrorList(t, nil,
			[]coreapi.AvailableMirror{{Owner: "acme", Repo: "web", Access: "write", Status: "available"}},
		)
		runMirrorList(t, "--show-available", "--owner", "acme")
		rec := <-recCh

		require.Equal(t, "/api/v1/mirrors/available", rec.path)
		require.Equal(t, "acme", rec.query.Get("owner"))
	})

	t.Run("--owner flows into the existing-mirror query", func(t *testing.T) {
		recCh := serveMirrorList(t,
			[]coreapi.Mirror{{Owner: "acme", Repo: "web", ClusterHost: "aws-us-east-2.entire.io"}}, nil,
		)
		runMirrorList(t, "--owner", "acme")
		rec := <-recCh

		require.Equal(t, mirrorsAPIPath, rec.path)
		require.Equal(t, "acme", rec.query.Get("owner"))
	})

	t.Run("--cluster/--provider apply to /mirrors but are ignored by --show-available", func(t *testing.T) {
		recCh := serveMirrorList(t,
			[]coreapi.Mirror{{Owner: "acme", Repo: "web", ClusterHost: "aws-us-east-2.entire.io"}}, nil,
		)
		runMirrorList(t, "--cluster", "eu-west-1.entire.io", "--provider", "github")
		rec := <-recCh
		require.Equal(t, mirrorsAPIPath, rec.path)
		require.Equal(t, "eu-west-1.entire.io", rec.query.Get("cluster"))
		require.Equal(t, "github", rec.query.Get("provider"))

		recCh = serveMirrorList(t, nil,
			[]coreapi.AvailableMirror{{Owner: "acme", Repo: "web", Access: "write", Status: "available"}},
		)
		runMirrorList(t, "--show-available", "--cluster", "eu-west-1.entire.io", "--provider", "github")
		rec = <-recCh
		require.Equal(t, "/api/v1/mirrors/available", rec.path)
		require.Empty(t, rec.query.Get("cluster"), "show-available is cluster-agnostic; must not send --cluster")
		require.Empty(t, rec.query.Get("provider"), "show-available is GitHub-only; must not send --provider")
	})
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
			if err := writeJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
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
			if err := writeJSON(w, &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
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
			want:   []string{"entirehq/entire.io", "entire://aws-us-east-2.entire.io/gh/entirehq/entire.io", "yes"},
		},
		{
			name:   "public mirror, unset IsPrivate defaults to no",
			mirror: coreapi.Mirror{Owner: "octocat", Repo: "hello", ClusterHost: "eu-west-1.entire.io"},
			want:   []string{"octocat/hello", "entire://eu-west-1.entire.io/gh/octocat/hello", "no"},
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

func TestAvailableMirrorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		repo coreapi.AvailableMirror
		want []string
	}{
		{
			name: "onboardable org repo",
			repo: coreapi.AvailableMirror{Owner: "acme", Repo: "web", Access: "write", Status: "available"},
			want: []string{"acme/web", "write", "available"},
		},
		{
			name: "already mirrored",
			repo: coreapi.AvailableMirror{Owner: "acme", Repo: "api", Access: "admin", Status: "mirrored"},
			want: []string{"acme/api", "admin", "mirrored"},
		},
		{
			name: "someone else's personal repo",
			repo: coreapi.AvailableMirror{Owner: "alice", Repo: "secret", Access: "read", Status: "owner-only"},
			want: []string{"alice/secret", "read", "owner-only"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := availableMirrorRow(tt.repo)
			if len(got) != len(tt.want) {
				t.Fatalf("availableMirrorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("availableMirrorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
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
