package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/remotehelper/replicas"
)

// testProxy creates a Proxy pointed at a single test server for
// /et/owner/repo. token is left empty so SetAuth no-ops — these tests
// exercise the plumbing (HTTP method, path, body), not the auth flow.
func testProxy(server *httptest.Server) *Proxy {
	return New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{server.URL},
			EntryURL:     server.URL,
			ClusterHost:  mustHost(nil, server.URL),
			RepoPath:     "owner/repo",
		},
		Path: "/et/owner/repo",
	})
}

func proxyWithClient(nodes []string, path, entryURL, clusterHost string, client *http.Client) *Proxy {
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: nodes,
			EntryURL:     entryURL,
			ClusterHost:  clusterHost,
			RepoPath:     "owner/repo",
		},
		Path: path,
	})
	if client != nil {
		// Swap in the test-provided client (e.g. one we want to
		// inspect or one with a custom CheckRedirect).
		p.client = client
	}
	return p
}

// mustHost returns the hostname portion of a URL, stripping the port.
func mustHost(t *testing.T, raw string) string {
	if t != nil {
		t.Helper()
	}
	u, err := url.Parse(raw)
	if err != nil {
		if t != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return ""
	}
	return u.Hostname()
}

func TestNewProxy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		nodes    []string
		path     string
		wantPath string
	}{
		{
			name:     "basic",
			nodes:    []string{"https://host.com"},
			path:     "/et/owner/repo",
			wantPath: "/et/owner/repo",
		},
		{
			name:     "multiple nodes",
			nodes:    []string{"https://n1.host.com", "https://n2.host.com"},
			path:     "/et/owner/repo",
			wantPath: "/et/owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := New(Config{
				Nodes: replicas.NodeConfig{
					InitialNodes: tt.nodes,
					EntryURL:     tt.nodes[0],
					ClusterHost:  "host.com",
					RepoPath:     "owner/repo",
				},
				Path: tt.path,
			})
			if p.path != tt.wantPath {
				t.Errorf("path = %q, want %q", p.path, tt.wantPath)
			}
			if len(p.nodes) != len(tt.nodes) {
				t.Errorf("nodes = %d, want %d", len(p.nodes), len(tt.nodes))
			}
		})
	}
}

const serviceParam = "git-upload-pack"

func TestInfoRefs(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/et/owner/repo/info/refs") {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("service") != serviceParam {
			t.Errorf("service = %s", r.URL.Query().Get("service"))
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "mock refs response")
	}))
	defer server.Close()

	resp, err := testProxy(server).InfoRefs(context.Background(), serviceParam)
	if err != nil {
		t.Fatalf("InfoRefs failed: %v", err)
	}
	defer resp.Close()

	body, err := io.ReadAll(resp)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(body) != "mock refs response" {
		t.Errorf("body = %q", body)
	}
}

func TestServiceRPC(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/et/owner/repo/git-upload-pack" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" {
			t.Errorf("content-type = %s", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) //nolint:errcheck // test
	}))
	defer server.Close()

	resp, err := testProxy(server).ServiceRPC(context.Background(), serviceParam, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("ServiceRPC: %v", err)
	}
	defer resp.Close()
	got, _ := io.ReadAll(resp) //nolint:errcheck // test
	if string(got) != "body" {
		t.Errorf("body = %q", got)
	}
}

func TestInfoRefs_ErrorIncludesStatusCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantSubstr string
	}{
		{"5xx with body", http.StatusBadGateway, "upstream connect error", "HTTP 502"},
		{"HTML body shows status", http.StatusBadGateway, "<html>Bad Gateway</html>", "HTTP 502"},
		{"empty body shows status", http.StatusInternalServerError, "", "HTTP 500"},
		{"4xx", http.StatusBadRequest, "bad request body", "server error (HTTP 400): bad request body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			_, err := testProxy(server).InfoRefs(context.Background(), "git-upload-pack")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestServiceRPC_ErrorIncludesStatusCode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream timeout")
	}))
	defer server.Close()

	_, err := testProxy(server).ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("body"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestUnauthorized(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := testProxy(server).InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error for unauthorized request")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error = %v", err)
	}
}

func TestFailover(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok from up node")
	}))
	defer up.Close()

	p := proxyWithClient([]string{down.URL, up.URL}, "/et/alice/repo", "", "", &http.Client{})

	resp, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("expected failover to succeed, got: %v", err)
	}
	defer resp.Close()
	body, _ := io.ReadAll(resp) //nolint:errcheck // test
	if string(body) != "ok from up node" {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestFailover5xx(t *testing.T) {
	t.Parallel()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "bad gateway")
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer good.Close()

	p := proxyWithClient([]string{bad.URL, good.URL}, "/et/alice/repo", "", "", &http.Client{})

	resp, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("expected failover to succeed, got: %v", err)
	}
	defer resp.Close()
	body, _ := io.ReadAll(resp) //nolint:errcheck // test
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
}

func TestOnNodeFailedCalledOnConnectionError(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()

	var failed []string
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{down.URL},
			ClusterHost:  mustHost(t, down.URL),
		},
		Path:         "/et/alice/repo",
		OnNodeFailed: func(node string) { failed = append(failed, node) },
	})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error with single unreachable node")
	}
	if len(failed) != 1 || failed[0] != down.URL {
		t.Errorf("onNodeFailed = %v, want [%s]", failed, down.URL)
	}
}

func TestOnNodeFailedCalledOn5xx(t *testing.T) {
	t.Parallel()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()

	var failed []string
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{bad.URL},
			ClusterHost:  mustHost(t, bad.URL),
		},
		Path:         "/et/alice/repo",
		OnNodeFailed: func(node string) { failed = append(failed, node) },
	})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error with single 502 node")
	}
	if len(failed) != 1 || failed[0] != bad.URL {
		t.Errorf("onNodeFailed = %v, want [%s]", failed, bad.URL)
	}
}

func TestNodeRemovedAfterFailure(t *testing.T) {
	t.Parallel()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	p := proxyWithClient([]string{bad.URL}, "/et/alice/repo", "", "", &http.Client{})

	resp, _ := p.doWithFailover(context.Background(), "/info/refs?service=git-upload-pack", http.MethodGet, nil, nil) //nolint:errcheck // we expect failure
	if resp != nil {
		resp.Body.Close()
	}
	if len(p.nodes) != 0 {
		t.Errorf("expected 0 nodes after failure, got %d: %v", len(p.nodes), p.nodes)
	}
}

func TestFailoverAll5xx(t *testing.T) {
	t.Parallel()
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s2.Close()

	p := proxyWithClient([]string{s1.URL, s2.URL}, "/et/alice/repo", "", "", &http.Client{})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error when all nodes return 5xx")
	}
	if !strings.Contains(err.Error(), "all 2 nodes failed") {
		t.Errorf("error = %v", err)
	}
}

func TestNoFailoverOn401(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "unauthorized")
	}))
	defer server.Close()

	var failedCalled bool
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{server.URL},
			ClusterHost:  mustHost(t, server.URL),
		},
		Path:         "/et/alice/repo",
		OnNodeFailed: func(string) { failedCalled = true },
	})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error = %v", err)
	}
	if failedCalled {
		t.Error("onNodeFailed should not be called on 401")
	}
}

func TestNoFailoverOn404(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "not found")
	}))
	defer server.Close()

	var failedCalled bool
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{server.URL},
			ClusterHost:  mustHost(t, server.URL),
		},
		Path:         "/et/alice/repo",
		OnNodeFailed: func(string) { failedCalled = true },
	})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if failedCalled {
		t.Error("onNodeFailed should not be called on 404")
	}
}

// TestSticky_AcrossRounds verifies that multiple POSTs from the same
// fetch session land on the same replica so Go's transport pool can
// reuse its TCP+TLS connection.
func TestSticky_AcrossRounds(t *testing.T) {
	t.Parallel()
	var s1Hits, s2Hits, s3Hits int
	mkServer := func(counter *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			*counter++
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1 := mkServer(&s1Hits)
	s2 := mkServer(&s2Hits)
	s3 := mkServer(&s3Hits)
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()

	p := proxyWithClient([]string{s1.URL, s2.URL, s3.URL}, "/et/owner/repo", "", "", s1.Client())

	ctx := context.Background()
	const rounds = 10
	for range rounds {
		resp, err := p.ServiceRPC(ctx, "git-upload-pack", strings.NewReader("body"))
		if err != nil {
			t.Fatalf("ServiceRPC: %v", err)
		}
		_ = resp.Close()
	}

	hits := []int{s1Hits, s2Hits, s3Hits}
	var nonZero, total int
	for _, h := range hits {
		total += h
		if h > 0 {
			nonZero++
		}
	}
	if total != rounds {
		t.Fatalf("total hits = %d, want %d", total, rounds)
	}
	if nonZero != 1 {
		t.Errorf("expected sticky to one node, got hits distributed across %d nodes: %v", nonZero, hits)
	}
}

func TestSticky_FollowsRedirect(t *testing.T) {
	t.Parallel()
	var bouncerHits, targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	bouncer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bouncerHits++
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer bouncer.Close()

	p := proxyWithClient([]string{bouncer.URL, target.URL}, "/et/owner/repo", "", "", target.Client())
	p.stickyNode = bouncer.URL

	resp, err := p.ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("ServiceRPC: %v", err)
	}
	_ = resp.Close()

	if bouncerHits != 1 {
		t.Errorf("bouncer hits = %d, want 1", bouncerHits)
	}
	if targetHits != 1 {
		t.Errorf("target hits = %d, want 1", targetHits)
	}
	if p.stickyNode != target.URL {
		t.Errorf("StickyNode = %q after redirect, want %q", p.stickyNode, target.URL)
	}

	bouncerHits, targetHits = 0, 0
	resp, err = p.ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("ServiceRPC 2: %v", err)
	}
	_ = resp.Close()
	if bouncerHits != 0 {
		t.Errorf("after sticky moved to target, bouncer should not be hit; got %d", bouncerHits)
	}
	if targetHits != 1 {
		t.Errorf("target hits on round 2 = %d, want 1", targetHits)
	}
}

func TestSticky_ResetsOnReplicaRefresh(t *testing.T) {
	t.Parallel()
	var hits int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()

	p := proxyWithClient([]string{s.URL}, "/et/owner/repo", "", "", s.Client())
	p.stickyNode = "https://stale-replica.example.com:443"

	resp, err := p.ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("ServiceRPC: %v", err)
	}
	_ = resp.Close()

	if hits != 1 {
		t.Errorf("hits = %d, want 1 (stale stickyNode shouldn't poison selection)", hits)
	}
	if p.stickyNode != s.URL {
		t.Errorf("StickyNode = %q after request, want %q", p.stickyNode, s.URL)
	}
}

// buildReplicasHeader joins server URLs the way the server middleware does.
func buildReplicasHeader(urls ...string) string { return strings.Join(urls, ",") }

func TestInfoRefsColdPathUsesEntryURL(t *testing.T) {
	t.Parallel()
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("cold-path test should not hit the replica directly")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer replica.Close()

	var entryHits atomic.Int32
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entryHits.Add(1)
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replica.URL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refs")
	}))
	defer entry.Close()

	entryHost := mustHost(t, entry.URL)
	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, entryHost, entry.Client())

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("InfoRefs: %v", err)
	}
	_ = body.Close()

	if got := entryHits.Load(); got != 1 {
		t.Errorf("entry hits = %d, want 1", got)
	}
	if !slices.Equal(p.nodes, []string{replica.URL}) {
		t.Errorf("nodes after cold path = %v, want %v", p.nodes, []string{replica.URL})
	}
}

func TestInfoRefsWarmPathRefreshesCache(t *testing.T) {
	t.Parallel()
	var newReplica string
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(newReplica))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refs")
	}))
	defer old.Close()

	brandNew := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer brandNew.Close()
	newReplica = brandNew.URL

	entryHost := mustHost(t, old.URL)
	p := proxyWithClient([]string{old.URL}, "/et/alice/repo", "http://unused.example", entryHost, old.Client())

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("InfoRefs: %v", err)
	}
	_ = body.Close()

	if !slices.Equal(p.nodes, []string{newReplica}) {
		t.Errorf("nodes after warm path = %v, want %v", p.nodes, []string{newReplica})
	}
}

func TestInfoRefsFollowsRedirectFromEntryDomain(t *testing.T) {
	t.Parallel()
	var replicaURL string
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/info/refs") {
			t.Errorf("replica got unexpected path %q", r.URL.Path)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refs")
	}))
	defer replica.Close()
	replicaURL = replica.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		http.Redirect(w, r, replicaURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", nil)
	p.client = &http.Client{CheckRedirect: p.checkRedirect}

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("InfoRefs: %v", err)
	}
	_ = body.Close()

	if !slices.Equal(p.nodes, []string{replicaURL}) {
		t.Errorf("nodes after 307 = %v, want %v", p.nodes, []string{replicaURL})
	}
}

func TestInfoRefsV2FollowsRedirectFromEntryDomain(t *testing.T) {
	t.Parallel()
	var replicaURL string
	var sawV2OnEntry, sawV2OnReplica atomic.Bool
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Git-Protocol") == "version=2" {
			sawV2OnReplica.Store(true)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "v2 refs")
	}))
	defer replica.Close()
	replicaURL = replica.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Git-Protocol") == "version=2" {
			sawV2OnEntry.Store(true)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		http.Redirect(w, r, replicaURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	body, err := p.InfoRefsV2(context.Background())
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "v2 refs", string(got))
	assert.True(t, sawV2OnEntry.Load(), "entry-domain v2 probe must carry Git-Protocol")
	assert.True(t, sawV2OnReplica.Load(), "replica v2 fetch must carry Git-Protocol")
}

func TestCheckRedirectPreservesAuthSameCluster(t *testing.T) {
	t.Parallel()
	p := &Proxy{clusterHost: "cluster.example"}

	via, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://entry.cluster.example/et/alice/repo", nil)
	require.NoError(t, err)
	via.Header.Set("Authorization", "Bearer scoped-token")

	next, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://n1.cluster.example/et/alice/repo", nil)
	require.NoError(t, err)

	require.NoError(t, p.checkRedirect(next, []*http.Request{via}))
	assert.Equal(t, "Bearer scoped-token", next.Header.Get("Authorization"))
}

func TestCheckRedirectDropsAuthOutOfCluster(t *testing.T) {
	t.Parallel()
	p := &Proxy{clusterHost: "cluster.example"}

	via, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://entry.cluster.example/et/alice/repo", nil)
	require.NoError(t, err)
	via.Header.Set("Authorization", "Bearer scoped-token")

	next, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://foreign.example/some/path", nil)
	require.NoError(t, err)
	next.Header.Set("Authorization", "Bearer scoped-token") // simulate same-host carry

	require.NoError(t, p.checkRedirect(next, []*http.Request{via}))
	assert.Empty(t, next.Header.Get("Authorization"))
}

func TestRefreshReplicasFallsBackToResponseURL(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://node7.cluster.example:8443/et/owner/repo/info/refs?service=git-upload-pack") //nolint:errcheck // static
	resp := &http.Response{Request: &http.Request{URL: u}, Header: http.Header{}}
	p := &Proxy{}
	p.refreshReplicas(resp)
	want := []string{"https://node7.cluster.example:8443"}
	if !slices.Equal(p.nodes, want) {
		t.Errorf("nodes = %v, want %v", p.nodes, want)
	}
}

func TestRefreshReplicasMissingHeaderPreservesCachedSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"
	cached := []string{"https://n1", "https://n2", "https://n3"}
	replicas.Persist(cluster, repo, cached)

	u, _ := url.Parse("https://n1/et/alice/repo/info/refs?service=git-upload-pack") //nolint:errcheck // static
	resp := &http.Response{Request: &http.Request{URL: u}, Header: http.Header{}}
	p := &Proxy{nodes: slices.Clone(cached), cacheHost: cluster, repoPath: repo}
	p.refreshReplicas(resp)

	if !slices.Equal(p.nodes, cached) {
		t.Errorf("p.nodes = %v, want %v", p.nodes, cached)
	}
	onDisk := replicas.LoadFresh(cluster, repo)
	if !slices.Equal(onDisk, cached) {
		t.Errorf("on-disk cache = %v, want %v", onDisk, cached)
	}
}

func TestRefreshReplicasMissingHeaderDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"

	u, _ := url.Parse("https://n1.eu.cluster.example/et/alice/repo/info/refs?service=git-upload-pack") //nolint:errcheck // static
	resp := &http.Response{Request: &http.Request{URL: u}, Header: http.Header{}}
	p := &Proxy{cacheHost: cluster, repoPath: repo}
	p.refreshReplicas(resp)

	if len(p.nodes) != 1 {
		t.Fatalf("p.nodes = %v, want one in-memory fallback entry", p.nodes)
	}
	if got := replicas.LoadFresh(cluster, repo); len(got) != 0 {
		t.Errorf("on-disk cache = %v, want empty (cold-start fallback must not persist)", got)
	}
}

func TestProxyConsumesServerHeaderFormat(t *testing.T) {
	t.Parallel()
	header := "https://n1.eu.cluster.example,https://n2.eu.cluster.example:8443"
	got := discovery.ParseReplicas(header)
	want := []string{"https://n1.eu.cluster.example", "https://n2.eu.cluster.example:8443"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseReplicas = %v, want %v", got, want)
	}
	if got := discovery.ParseReplicas(""); got != nil {
		t.Errorf("ParseReplicas(\"\") = %v, want nil", got)
	}
}

// closedServerURL returns the URL of an httptest.Server that has been
// closed — a guaranteed-dead 127.0.0.1:port a request will fail to dial.
func closedServerURL() string {
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := s.URL
	s.Close()
	return url
}

func TestColdPathFailoverWhenRedirectTargetUnreachable(t *testing.T) {
	dead := closedServerURL()

	var aliveURL string
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/info/refs") {
			t.Errorf("alive replica got unexpected path %q", r.URL.Path)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refs from alive")
	}))
	defer alive.Close()
	aliveURL = alive.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(dead, aliveURL))
		http.Redirect(w, r, dead+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	const iterations = 8
	deadFailoverObserved := false

	for i := range iterations {
		var failed []string
		p := New(Config{
			Nodes: replicas.NodeConfig{
				EntryURL:    entry.URL,
				ClusterHost: "127.0.0.1",
			},
			Path:         "/et/alice/repo",
			OnNodeFailed: func(node string) { failed = append(failed, node) },
		})

		body, err := p.InfoRefs(context.Background(), "git-upload-pack")
		require.NoErrorf(t, err, "iteration %d: failover must succeed", i)

		got, err := io.ReadAll(body)
		require.NoError(t, err)
		_ = body.Close()
		assert.Equal(t, "refs from alive", string(got), "iteration %d: response must come from alive replica", i)

		if !slices.Equal(p.nodes, []string{aliveURL}) {
			t.Errorf("iteration %d: nodes after failover = %v, want [%s]", i, p.nodes, aliveURL)
		}
		if slices.Contains(failed, dead) {
			deadFailoverObserved = true
		}
	}

	if !deadFailoverObserved {
		t.Errorf("dead replica never marked failed across %d iterations — failover path may not be exercised", iterations)
	}
}

func TestColdPathDoesNotFollowRedirectAuto(t *testing.T) {
	t.Parallel()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("redirect target was dialled — auto-follow regression")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer forbidden.Close()

	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", "")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "from replica")
	}))
	defer replica.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replica.URL))
		http.Redirect(w, r, forbidden.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "from replica", string(got))
}

func TestColdPathRedirectAllReplicasFail(t *testing.T) {
	t.Parallel()
	dead1 := closedServerURL()
	dead2 := closedServerURL()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(dead1, dead2))
		http.Redirect(w, r, dead1+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 2 nodes failed")
	assert.Contains(t, err.Error(), "after entry-domain redirect")
}

func TestColdPathRedirectMissingHeaderFollows(t *testing.T) {
	t.Parallel()
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "from final")
	}))
	defer final.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", nil)
	p.client = &http.Client{CheckRedirect: p.checkRedirect}

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()
	got, _ := io.ReadAll(body) //nolint:errcheck // test
	assert.Equal(t, "from final", string(got))
}

func TestColdPathRedirectFailureDoesNotPoisonCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dead1 := closedServerURL()
	dead2 := closedServerURL()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(dead1, dead2))
		http.Redirect(w, r, dead1+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"
	p := New(Config{
		Nodes: replicas.NodeConfig{
			EntryURL:    entry.URL,
			ClusterHost: "127.0.0.1",
			RepoPath:    repo,
		},
		Path: "/et/alice/repo",
	})
	p.cacheHost, p.repoPath = cluster, repo

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.Error(t, err)
	if got := replicas.LoadFresh(cluster, repo); len(got) != 0 {
		t.Errorf("on-disk cache = %v, want empty", got)
	}
}

func TestColdPathRedirectStaleHeaderFallsBackToLocation(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dead1 := closedServerURL()
	dead2 := closedServerURL()

	var aliveURL string
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "served by alive")
	}))
	defer alive.Close()
	aliveURL = alive.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(dead1, dead2))
		http.Redirect(w, r, aliveURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"
	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", &http.Client{})
	p.cacheHost, p.repoPath = cluster, repo

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()
	got, _ := io.ReadAll(body) //nolint:errcheck // test
	assert.Equal(t, "served by alive", string(got))

	if cached := replicas.LoadFresh(cluster, repo); !slices.Equal(cached, []string{aliveURL}) {
		t.Errorf("on-disk cache = %v, want [%s]", cached, aliveURL)
	}
}

func TestColdPathRedirectSuccessPersistsReplicas(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var aliveURL string
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer alive.Close()
	aliveURL = alive.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL))
		http.Redirect(w, r, aliveURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	const cluster = "eu.cluster.example"
	const repo = "alice/repo"
	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())
	p.cacheHost, p.repoPath = cluster, repo

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	_ = body.Close()

	if cached := replicas.LoadFresh(cluster, repo); !slices.Equal(cached, []string{aliveURL}) {
		t.Errorf("on-disk cache = %v, want [%s]", cached, aliveURL)
	}
}

// TestColdPathRedirectForwardsBearer: the Bearer header set by SetAuth
// must reach both the entry probe AND the replica fetch across a
// cold-path 307. The SetAuth callback stamps Authorization per
// request, and checkRedirect re-carries it for same-cluster hops.
func TestColdPathRedirectForwardsBearer(t *testing.T) {
	t.Parallel()
	const bearer = "Bearer scoped"

	var replicaURL string
	var sawBearerOnReplica atomic.Bool
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == bearer {
			sawBearerOnReplica.Store(true)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer replica.Close()
	replicaURL = replica.URL

	var sawBearerOnEntry atomic.Bool
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == bearer {
			sawBearerOnEntry.Store(true)
		}
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		http.Redirect(w, r, replicaURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := New(Config{
		Nodes: replicas.NodeConfig{
			EntryURL:    entry.URL,
			ClusterHost: "127.0.0.1",
		},
		Path: "/et/alice/repo",
		SetAuth: func(req *http.Request) error {
			req.Header.Set("Authorization", bearer)
			return nil
		},
	})

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	_ = body.Close()

	assert.True(t, sawBearerOnEntry.Load(), "entry-domain probe must carry Authorization: Bearer")
	assert.True(t, sawBearerOnReplica.Load(), "warm-path replica fetch must carry Authorization: Bearer")
}

func TestColdPathDirect2xxIsHandledInPlace(t *testing.T) {
	t.Parallel()
	var replicaURL string
	replica := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("replica must not be dialled when entry serves 200 directly")
	}))
	defer replica.Close()
	replicaURL = replica.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(replicaURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "from entry")
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()
	got, _ := io.ReadAll(body) //nolint:errcheck // test
	assert.Equal(t, "from entry", string(got))
	if !slices.Equal(p.nodes, []string{replicaURL}) {
		t.Errorf("nodes = %v, want [%s]", p.nodes, replicaURL)
	}
}

func TestColdPathEntryDomain4xxNotTreatedAsRedirect(t *testing.T) {
	t.Parallel()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication failed")
	assert.NotContains(t, err.Error(), "after entry-domain redirect")
}

func TestColdPathRedirectEmptyReplicaHeaderFollows(t *testing.T) {
	t.Parallel()
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "fallback served")
	}))
	defer final.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", "  , ,")
		http.Redirect(w, r, final.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", nil)
	p.client = &http.Client{CheckRedirect: p.checkRedirect}

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()
	got, _ := io.ReadAll(body) //nolint:errcheck // test
	assert.Equal(t, "fallback served", string(got))
}

func TestIsRedirect(t *testing.T) {
	t.Parallel()
	for _, c := range []int{300, 301, 302, 303, 307, 308, 399} {
		if !isRedirect(c) {
			t.Errorf("isRedirect(%d) = false, want true", c)
		}
	}
	for _, c := range []int{200, 299, 400, 401, 500, 502} {
		if isRedirect(c) {
			t.Errorf("isRedirect(%d) = true, want false", c)
		}
	}
}

// fakeTransport is a no-op http.RoundTripper used to verify pointer
// identity in TestNoRedirectClientShareTransport without leaning on
// http.DefaultTransport.
type fakeTransport struct{}

func (fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("fakeTransport must not be exercised")
}

func TestNoRedirectClientShareTransport(t *testing.T) {
	t.Parallel()
	transport := fakeTransport{}
	p := &Proxy{client: &http.Client{Transport: transport}}
	got := p.noRedirectClient()
	if got.Transport != transport {
		t.Errorf("NoRedirectClient transport = %v, want %v", got.Transport, transport)
	}
	if got.CheckRedirect == nil {
		t.Fatal("NoRedirectClient must set CheckRedirect")
	}
	if err := got.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}

func TestInfoRefsWarmAndColdBothFailSurfacesBoth(t *testing.T) {
	t.Parallel()
	deadReplica1 := closedServerURL()
	deadReplica2 := closedServerURL()
	deadEntry := closedServerURL()

	p := proxyWithClient([]string{deadReplica1, deadReplica2}, "/et/alice/repo", deadEntry, "127.0.0.1", &http.Client{})

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "cached replicas")
	assert.Contains(t, msg, "all 2 nodes failed")
	assert.Contains(t, msg, "entry domain")
}

// --- Off-cluster credential-leak protection -------------------------------
//
// These tests cover the trust boundary that keeps the repo-scoped bearer
// token from reaching hosts outside the cluster the user addressed. The
// attack: a malicious entry domain (or a poisoned nodes.json) advertises
// replica/Location URLs on attacker-controlled hosts; without scoping, the
// helper would POST git-upload-pack to them with the core-minted JWT
// attached. httptest servers all bind 127.0.0.1, so off-cluster hosts are
// fabricated URL strings — the point is precisely that they are never
// dialed.

const offClusterReplica = "https://attacker-log-collector.example.com"

func TestSetAuthOrErrorRefusesOutOfClusterHost(t *testing.T) {
	t.Parallel()
	var attached int
	p := &Proxy{
		clusterHost: "cluster.example",
		setAuth: func(req *http.Request) error {
			attached++
			req.Header.Set("Authorization", "Bearer scoped-token")
			return nil
		},
	}

	off, err := http.NewRequestWithContext(t.Context(), http.MethodGet, offClusterReplica+"/et/alice/repo/info/refs", nil)
	require.NoError(t, err)
	require.Error(t, p.setAuthOrError(off), "must refuse to stamp a token onto an off-cluster request")
	assert.Empty(t, off.Header.Get("Authorization"))
	assert.Zero(t, attached, "SetAuth must not even be invoked for an off-cluster host")

	in, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://n1.cluster.example/et/alice/repo/info/refs", nil)
	require.NoError(t, err)
	require.NoError(t, p.setAuthOrError(in))
	assert.Equal(t, "Bearer scoped-token", in.Header.Get("Authorization"))
}

func TestNewFiltersOutOfClusterCachedNodes(t *testing.T) {
	t.Parallel()
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{"https://n1.cluster.example", offClusterReplica, "https://n2.cluster.example"},
			EntryURL:     "https://cluster.example",
			ClusterHost:  "cluster.example",
			RepoPath:     "alice/repo",
		},
		Path: "/et/alice/repo",
	})
	assert.Equal(t, []string{"https://n1.cluster.example", "https://n2.cluster.example"}, p.nodes,
		"poisoned cache entry pointing off-cluster must be dropped on load")
}

func TestRefreshReplicasDropsOutOfClusterReplicas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	const cluster = "cluster.example"
	const repo = "alice/repo"

	u, err := url.Parse("https://n1.cluster.example/et/alice/repo/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	resp := &http.Response{
		Request: &http.Request{URL: u},
		Header: http.Header{
			"X-Entire-Replicas": {buildReplicasHeader("https://n1.cluster.example", offClusterReplica, "https://canarytokens.com/foo")},
		},
	}
	p := &Proxy{clusterHost: cluster, cacheHost: cluster, repoPath: repo}
	p.refreshReplicas(resp)

	assert.Equal(t, []string{"https://n1.cluster.example"}, p.nodes,
		"off-cluster entries from X-Entire-Replicas must be dropped")
	assert.Equal(t, []string{"https://n1.cluster.example"}, replicas.LoadFresh(cluster, repo),
		"only in-cluster replicas may be persisted to disk")
}

func TestRefreshReplicasResponseURLFallbackIgnoresOutOfClusterHost(t *testing.T) {
	t.Parallel()
	// Cold start, no header, but the request landed on an off-cluster host
	// (e.g. an auth-stripped redirect). It must not become the node set.
	u, err := url.Parse(offClusterReplica + "/et/alice/repo/info/refs")
	require.NoError(t, err)
	resp := &http.Response{Request: &http.Request{URL: u}, Header: http.Header{}}
	p := &Proxy{clusterHost: "cluster.example"}
	p.refreshReplicas(resp)
	assert.Empty(t, p.nodes, "off-cluster responder must not be adopted as the node set")
}

func TestColdPathDropsOutOfClusterReplicasFromHeader(t *testing.T) {
	t.Parallel()
	var aliveURL string
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refs from in-cluster replica")
	}))
	defer alive.Close()
	aliveURL = alive.URL

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise a legit in-cluster replica next to an attacker host.
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(aliveURL, offClusterReplica))
		http.Redirect(w, r, aliveURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	// clusterHost = 127.0.0.1 makes the httptest hosts in-cluster while the
	// fabricated attacker host stays out.
	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	body, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.NoError(t, err)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	_ = body.Close()

	assert.Equal(t, "refs from in-cluster replica", string(got))
	assert.NotContains(t, p.nodes, offClusterReplica, "attacker replica must never enter the node set")
}

func TestColdPathRefusesOutOfClusterLocationSalvage(t *testing.T) {
	t.Parallel()
	deadReplica := closedServerURL() // in-cluster (127.0.0.1) but unreachable

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The only replica is dead, forcing the salvage path; Location points
		// off-cluster, which the salvage path must refuse rather than dial
		// with the token attached.
		w.Header().Set("X-Entire-Replicas", buildReplicasHeader(deadReplica))
		http.Redirect(w, r, offClusterReplica+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	p := proxyWithClient(nil, "/et/alice/repo", entry.URL, "127.0.0.1", entry.Client())

	_, err := p.InfoRefs(context.Background(), "git-upload-pack")
	require.Error(t, err, "must not salvage to an off-cluster Location")
}
