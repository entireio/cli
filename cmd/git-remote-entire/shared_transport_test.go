package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// A cold clone fetches /.well-known/entire-cluster.json and then probes
// info/refs on the same entry host. Both must ride one pooled connection
// instead of paying a second TCP and TLS handshake.
func TestAuthClientAndColdProbeShareEntryConnection(t *testing.T) {
	t.Parallel()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		}
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Error(err)
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	base := httpclient.NewDiscoveryTransport(true)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/.well-known/entire-cluster.json", nil)
	require.NoError(t, err)
	resp, err := newAuthHTTPClient(base, "test").Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	p := transport.New(transport.Config{
		Nodes:              replicas.NodeConfig{EntryURL: srv.URL, ClusterHost: u.Host},
		Path:               "/gh/owner/repo",
		SkipTLS:            true,
		DiscoveryTransport: base,
	})
	body, err := p.InfoRefs(t.Context(), "git-upload-pack")
	require.NoError(t, err)
	require.NoError(t, body.Close())

	require.Equal(t, int32(1), conns.Load())
}
