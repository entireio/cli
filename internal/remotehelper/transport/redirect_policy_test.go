package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/remotehelper/replicas"
)

func TestProxy_ColdProbeUsesHTTPSReplicasDespiteHTTPLocation(t *testing.T) {
	t.Parallel()
	var plaintextHits atomic.Int32
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plaintextHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plaintext.Close)
	replica := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "refs"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(replica.Close)
	entry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", replica.URL)
		http.Redirect(w, r, plaintext.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(entry.Close)
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: entry.URL, ClusterHost: mustHost(t, entry.URL)}, SetAuth: func(req *http.Request) error {
		req.Header.Set("Authorization", "Bearer test-token")
		return nil
	}})
	p.discoveryTransport = entry.Client().Transport
	p.client.Transport = replica.Client().Transport
	body, err := p.InfoRefs(t.Context(), "git-upload-pack")
	require.NoError(t, err)
	defer body.Close()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "refs", string(data))
	require.Zero(t, plaintextHits.Load())
	require.Equal(t, []string{replica.URL}, p.nodes)
}

func TestProxy_PolicyRejectionTriesRemainingReplicas(t *testing.T) {
	t.Parallel()
	const bad = "https://bad.cluster.example"
	const good = "https://good.cluster.example"
	var failed, dialed []string
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: "https://cluster.example", ClusterHost: "cluster.example", InitialNodes: []string{bad, good}}, OnNodeFailed: func(node string) { failed = append(failed, node) }})
	p.stickyNode = bad
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		node := req.URL.Scheme + "://" + req.URL.Host
		dialed = append(dialed, node)
		code := http.StatusOK
		h := make(http.Header)
		if node == bad {
			code = http.StatusTemporaryRedirect
			h.Set("Location", "http://bad.cluster.example/repo")
		}
		return &http.Response{StatusCode: code, Header: h, Body: http.NoBody, Request: req}, nil
	})
	body, err := p.InfoRefs(t.Context(), "git-upload-pack")
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Equal(t, []string{bad, good}, dialed)
	require.Equal(t, []string{bad}, failed, "a policy-rejected node is dropped like an unreachable one")
	require.Equal(t, []string{good}, p.nodes)
}

func TestProxy_RedirectRejectionFailsNodeWithoutLocationFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		cold       bool
		wantCalls  int
		wantFailed int
	}{
		// Warm: the cached node is refused, then the entry URL re-advertises it
		// and it is refused again. Neither path follows the server's Location.
		{"warm downgrade", false, 3, 2},
		{"cold downgrade", true, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const entry = "https://cluster.example"
			const node = "https://n.cluster.example"
			nodes := []string{node}
			if tc.cold {
				nodes = nil
			}
			var failed []string
			p := New(Config{Nodes: replicas.NodeConfig{EntryURL: entry, ClusterHost: "cluster.example", InitialNodes: nodes}, OnNodeFailed: func(node string) { failed = append(failed, node) }})
			calls := 0
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				h := make(http.Header)
				h.Set("Location", "http://n.cluster.example/repo")
				if req.URL.Host == "cluster.example" {
					h.Set("X-Entire-Replicas", node)
					h.Set("Location", node+"/repo")
				}
				return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h, Body: http.NoBody, Request: req}, nil
			})
			p.client.Transport = rt
			p.discoveryTransport = rt
			_, err := p.InfoRefs(t.Context(), "git-upload-pack")
			require.ErrorIs(t, err, httpclient.ErrInsecureRedirect)
			require.Len(t, failed, tc.wantFailed, "every rejection invalidates the node")
			require.Empty(t, p.nodes)
			require.Equal(t, tc.wantCalls, calls, "a policy failure must not trigger the Location fallback")
		})
	}
}

// A misbehaving cached replica must not block fetches until the replica cache
// expires: the warm path falls back to the entry URL, including after a policy
// rejection, and a redirect loop is an ordinary node failure.
func TestProxy_WarmPathFailureFallsBackToEntry(t *testing.T) {
	t.Parallel()
	const entry = "https://cluster.example"
	const looping = "https://loop.cluster.example"
	const downgrading = "https://down.cluster.example"
	const unavailable = "https://five.cluster.example"
	for _, tc := range []struct {
		name       string
		nodes      []string
		wantFailed []string
	}{
		{"redirect loop", []string{looping}, []string{looping}},
		{"downgrade and unavailable replica", []string{downgrading, unavailable}, []string{downgrading, unavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var failed []string
			p := New(Config{Nodes: replicas.NodeConfig{EntryURL: entry, ClusterHost: "cluster.example", InitialNodes: tc.nodes}, OnNodeFailed: func(node string) { failed = append(failed, node) }})
			entryHits := 0
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				h := make(http.Header)
				status := http.StatusTemporaryRedirect
				switch req.URL.Scheme + "://" + req.URL.Host {
				case entry:
					entryHits++
					status = http.StatusOK
				case looping:
					h.Set("Location", looping+"/repo")
				case downgrading:
					h.Set("Location", "http://down.cluster.example/repo")
				default:
					status = http.StatusServiceUnavailable
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("refs")), Request: req}, nil
			})
			p.client.Transport = rt
			p.discoveryTransport = rt
			body, err := p.InfoRefs(t.Context(), "git-upload-pack")
			require.NoError(t, err)
			require.NoError(t, body.Close())
			require.Equal(t, 1, entryHits)
			require.ElementsMatch(t, tc.wantFailed, failed)
		})
	}
}

// When the entry answers info/refs itself (no X-Entire-Replicas), the POST must
// go to the entry host, not back to the cached replica that was just refused.
func TestProxy_RejectedReplicaIsNotThePostTargetAfterEntryFallback(t *testing.T) {
	t.Parallel()
	const entry = "https://cluster.example"
	const downgrading = "https://down.cluster.example"
	var failed, posted []string
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: entry, ClusterHost: "cluster.example", InitialNodes: []string{downgrading}}, OnNodeFailed: func(node string) { failed = append(failed, node) }})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		host := req.URL.Scheme + "://" + req.URL.Host
		if req.Method == http.MethodPost {
			posted = append(posted, host)
		}
		if host == downgrading {
			h := http.Header{"Location": {"http://down.cluster.example/repo"}}
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h, Body: http.NoBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	p.client.Transport = rt
	p.discoveryTransport = rt
	body, err := p.InfoRefs(t.Context(), "git-upload-pack")
	require.NoError(t, err)
	require.NoError(t, body.Close())
	rpc, err := p.ServiceRPC(t.Context(), "git-upload-pack", strings.NewReader("want"))
	require.NoError(t, err)
	require.NoError(t, rpc.Close())
	require.Equal(t, []string{entry}, posted)
	require.Equal(t, []string{downgrading}, failed)
}

func TestProxy_SkippedReplicaPreservesStorageError(t *testing.T) {
	t.Parallel()
	const secure = "https://127.0.0.1"
	const plain = "http://127.0.0.1"
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: plain, InitialNodes: []string{secure, plain}}})
	p.stickyNode = secure
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https", req.URL.Scheme)
		return &http.Response{StatusCode: http.StatusInsufficientStorage, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})
	_, err := p.ServiceRPC(t.Context(), "git-receive-pack", strings.NewReader("push"))
	var storage *InsufficientStorageError
	require.ErrorAs(t, err, &storage)
}

func TestProxy_InsecureReplicaErrorDoesNotClaimRedirect(t *testing.T) {
	t.Parallel()
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: "http://cluster.example"}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://cluster.example/repo", nil)
	require.NoError(t, err)
	err = p.setAuthOrError(req)
	require.ErrorIs(t, err, ErrInsecureReplica)
	require.ErrorIs(t, err, httpclient.ErrRequestPolicy)
	require.NotContains(t, err.Error(), "redirect")
}

func TestProxy_ResponseURLFallbackRequiresSafeScheme(t *testing.T) {
	t.Parallel()
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: "https://cluster.example", ClusterHost: "cluster.example"}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://cluster.example/repo", nil)
	require.NoError(t, err)
	p.refreshReplicas(&http.Response{Header: make(http.Header), Request: req})
	require.Empty(t, p.nodes)
}
