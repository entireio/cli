package transport

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/replicas"
)

func TestDoSendsRepoRequest(t *testing.T) {
	t.Parallel()
	var got struct {
		method, path, auth, ua, contentType string
		body                                []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.ua, got.contentType = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body) //nolint:errcheck // test
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test
	}))
	defer srv.Close()
	var authed []string
	p := New(Config{
		Nodes:     replicas.NodeConfig{InitialNodes: []string{srv.URL}, EntryURL: srv.URL, ClusterHost: mustHost(t, srv.URL)},
		Path:      "/et/alice/repo",
		UserAgent: "git-remote-entire/test",
		SetAuth: func(r *http.Request) error {
			authed = append(authed, r.Method+" "+r.URL.Path)
			r.Header.Set("Authorization", "Bearer tok")
			return nil
		},
	})

	resp, err := p.Do(t.Context(), http.MethodPut, "/bb/xorbs/abc", bytes.NewReader([]byte("xorb")), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/octet-stream")
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.JSONEq(t, `{"ok":true}`, string(body))
	require.Equal(t, http.MethodPut, got.method)
	require.Equal(t, "/et/alice/repo/bb/xorbs/abc", got.path)
	require.Equal(t, "Bearer tok", got.auth)
	require.Equal(t, "git-remote-entire/test", got.ua)
	require.Equal(t, "application/octet-stream", got.contentType)
	require.Equal(t, []byte("xorb"), got.body)
	require.Equal(t, []string{"PUT /et/alice/repo/bb/xorbs/abc"}, authed)
}

func TestDoRefusesSuffixWithoutSlash(t *testing.T) {
	t.Parallel()
	p := New(Config{Nodes: replicas.NodeConfig{InitialNodes: []string{"http://127.0.0.1:1"}, EntryURL: "http://127.0.0.1:1"}, Path: "/et/alice/repo"})
	resp, err := p.Do(t.Context(), http.MethodPost, "bb/dedup", nil, nil)
	if resp != nil {
		resp.Body.Close()
	}
	require.ErrorContains(t, err, "does not start with /")
}

// A client error is the endpoint's answer: Do returns it, and the node
// stays in the replica set.
func TestDoReturnsClientErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "xorb hash mismatch", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	p, failed := recordingProxy(t, []string{srv.URL})

	resp, err := p.Do(t.Context(), http.MethodPut, "/bb/xorbs/abc", bytes.NewReader(nil), nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Empty(t, *failed)
	require.Equal(t, []string{srv.URL}, p.replicaSet())
}

// A 5xx answer comes back to the caller, which retries it. Marking the node
// failed would purge the replica set that the push still needs, and a
// send-pack fallback after the uploads would then find no node.
func TestDoReturnsServerErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusInsufficientStorage} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var hits atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Retry-After", "2")
				http.Error(w, "server error", status)
			})
			a := httptest.NewServer(handler)
			defer a.Close()
			b := httptest.NewServer(handler)
			defer b.Close()
			p, failed := recordingProxy(t, []string{a.URL, b.URL})

			resp, err := p.Do(t.Context(), http.MethodPut, "/bb/xorbs/abc", bytes.NewReader(nil), nil)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, status, resp.StatusCode)
			require.Equal(t, "2", resp.Header.Get("Retry-After"))
			require.Equal(t, int32(1), hits.Load(), "no failover to the second node")
			require.Empty(t, *failed)
			require.Equal(t, []string{a.URL, b.URL}, p.replicaSet())
		})
	}
}

// The caller retries a 5xx answer. The retry must leave the sticky node that
// answered it, or every retry reaches the same broken replica.
func TestDoRetryStartsPastServerError(t *testing.T) {
	t.Parallel()
	var badHits, goodHits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	p, failed := recordingProxy(t, []string{bad.URL, good.URL})
	p.stickyNode = bad.URL

	for _, want := range []int{http.StatusBadGateway, http.StatusOK} {
		resp, err := p.Do(t.Context(), http.MethodPut, "/bb/xorbs/abc", bytes.NewReader(nil), nil)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, want, resp.StatusCode)
	}
	require.Equal(t, int32(1), badHits.Load())
	require.Equal(t, int32(1), goodHits.Load())
	require.Empty(t, *failed)
	require.Equal(t, []string{bad.URL, good.URL}, p.replicaSet())
	require.Equal(t, good.URL, p.stickyNode)
}

// A node that does not answer is down: Do fails over to the next one.
func TestDoFailsOverOnConnectionError(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	var goodHits atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	p, failed := recordingProxy(t, []string{down.URL, good.URL})
	p.stickyNode = down.URL

	resp, err := p.Do(t.Context(), http.MethodPost, "/bb/dedup", bytes.NewReader([]byte("{}")), nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(1), goodHits.Load())
	require.Equal(t, []string{down.URL}, *failed)
}

// Do runs concurrently with itself. Run with -race.
func TestDoConcurrent(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		_, _ = w.Write(body)          //nolint:errcheck // test
	}))
	defer srv.Close()
	p, _ := recordingProxy(t, []string{srv.URL})

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			payload := []byte(strconv.Itoa(i))
			resp, err := p.Do(t.Context(), http.MethodPut, "/bb/xorbs/"+strconv.Itoa(i), bytes.NewReader(payload), nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err == nil && !bytes.Equal(got, payload) {
				err = io.ErrUnexpectedEOF
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "request %d", i)
	}
	require.Equal(t, int32(n), hits.Load())
	require.Equal(t, srv.URL, p.stickyNode)
}

// The entry domain redirects to a replica without a replica set, so the
// helper probes it, then follows the redirect. Every hop carries the BB
// header, and so does a warm request to the cached replica. Plain InfoRefs
// does not send it.
func TestInfoRefsForPushSendsBBHeader(t *testing.T) {
	t.Parallel()
	var replicaURL string
	var mu sync.Mutex
	var seen []string
	record := func(who string, r *http.Request) {
		mu.Lock()
		seen = append(seen, who+" "+r.URL.Query().Get("service")+" "+r.Header.Get(HeaderBB))
		mu.Unlock()
	}
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("replica", r)
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		_, _ = w.Write([]byte("refs")) //nolint:errcheck // test
	}))
	defer replica.Close()
	replicaURL = replica.URL
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("entry", r)
		http.Redirect(w, r, replicaURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()
	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", nil)
	p.client = &http.Client{CheckRedirect: p.checkRedirect}

	for range 2 {
		body, err := p.InfoRefsForPush(t.Context())
		require.NoError(t, err)
		require.NoError(t, body.Close())
	}
	body, err := p.InfoRefs(t.Context(), "git-receive-pack")
	require.NoError(t, err)
	require.NoError(t, body.Close())

	require.Equal(t, []string{
		"entry git-receive-pack 1",
		"entry git-receive-pack 1",
		"replica git-receive-pack 1",
		"replica git-receive-pack 1",
		"replica git-receive-pack ",
	}, seen)
}
