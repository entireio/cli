package transport

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
	"github.com/entireio/cli/internal/remotehelper/replicas"
)

func TestProxy_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		p := New(Config{Nodes: replicas.NodeConfig{EntryURL: origin, ClusterHost: mustHost(t, origin)}, SetAuth: func(r *http.Request) error {
			r.Header.Set("Authorization", "Bearer test-token")
			return nil
		}})
		p.client.Transport = rt
		p.discoveryTransport = rt
		body, err := p.InfoRefs(t.Context(), "git-upload-pack")
		if body != nil {
			_ = body.Close()
		}
		return err
	})
}

func TestProxy_HTTPReplicaIngress(t *testing.T) {
	t.Parallel()
	const origin = "https://cluster.example"
	const node = "https://n.cluster.example"
	const insecure = "http://n.cluster.example"
	for _, flow := range []string{"cache", "cold header", "Location salvage", "warm header"} {
		t.Run(flow, func(t *testing.T) {
			t.Parallel()
			nodes := []string{insecure}
			if flow == "warm header" {
				nodes = []string{node}
			}
			plaintext := 0
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				code := http.StatusOK
				h := make(http.Header)
				if r.URL.Scheme == "http" {
					plaintext++
				}
				switch flow {
				case "cold header":
					code = http.StatusTemporaryRedirect
					h.Set("Location", insecure+"/repo")
					h.Set("X-Entire-Replicas", insecure)
				case "Location salvage":
					if r.URL.Host == "cluster.example" {
						code = http.StatusTemporaryRedirect
						h.Set("Location", insecure+"/repo")
						h.Set("X-Entire-Replicas", node)
					} else {
						code = http.StatusServiceUnavailable
					}
				case "warm header":
					h.Set("X-Entire-Replicas", insecure)
				}
				return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader("refs")), Request: r}, nil
			})
			p := New(Config{Nodes: replicas.NodeConfig{EntryURL: origin, ClusterHost: "cluster.example", InitialNodes: nodes}, SkipTLS: true, SetAuth: func(r *http.Request) error {
				r.Header.Set("Authorization", "Bearer test-token")
				return nil
			}})
			p.client.Transport = rt
			p.discoveryTransport = rt
			body, err := p.InfoRefs(t.Context(), "git-upload-pack")
			if body != nil {
				require.NoError(t, body.Close())
			}
			if flow == "cold header" || flow == "Location salvage" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, plaintext, "no HTTP replica may be dialed, even with SkipTLS")
			require.NotContains(t, p.nodes, insecure)
		})
	}
}

func TestProxy_LocalHTTPDoesNotResumeAfterHTTPS(t *testing.T) {
	t.Parallel()
	const local = "http://127.0.0.1"
	const secure = "https://127.0.0.1"
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: local, InitialNodes: []string{local, secure}}})
	p.stickyNode = secure
	calls := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "https", r.URL.Scheme, "manual failover must not downgrade either")
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
	})
	_, err := p.ServiceRPC(t.Context(), "git-upload-pack", strings.NewReader("body"))
	require.Error(t, err)
	require.Equal(t, 1, calls)
}

func TestProxy_RejectsHTTPReplicas(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"https://cluster.example", "https://127.0.0.1"} {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			host := mustHost(t, origin)
			insecure := "http://" + host
			p := New(Config{Nodes: replicas.NodeConfig{EntryURL: origin, ClusterHost: host, InitialNodes: []string{insecure, origin}}, SetAuth: func(*http.Request) error {
				t.Error("unsafe request reached auth provider")
				return nil
			}})
			require.Equal(t, []string{origin}, p.nodes)
			require.False(t, p.replicaInCluster(insecure), "header and Location ingress must reject HTTP too")
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, insecure+"/repo", nil)
			require.NoError(t, err)
			require.Error(t, p.setAuthOrError(req), "credential stamping must independently reject HTTP")
		})
	}
}
