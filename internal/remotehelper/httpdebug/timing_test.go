package httpdebug

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// Not parallel: enables ENTIRE_DEBUG and swaps the process-wide debuglog sink.
func TestTimingRoundTripperBreaksDownConnectionPhases(t *testing.T) {
	t.Setenv("ENTIRE_DEBUG", "1")
	var buf bytes.Buffer
	prev := debuglog.SetOutput(&buf)
	t.Cleanup(func() { debuglog.SetOutput(prev) })

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &TimingRoundTripper{Next: srv.Client().Transport, Label: "git"}}

	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/info/refs", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)
	fresh := regexp.MustCompile(`timing: git GET \S+/info/refs status=204 conn=new connect_ms=\d+ tls_ms=\d+ ttfb_ms=\d+ dur_ms=\d+$`)
	require.Regexp(t, fresh, lines[0])
	reused := regexp.MustCompile(`timing: git GET \S+/info/refs status=204 conn=reused ttfb_ms=\d+ dur_ms=\d+$`)
	require.Regexp(t, reused, lines[1])
}

// Not parallel: enables ENTIRE_DEBUG and swaps the process-wide debuglog sink.
func TestTimingRoundTripperLogsPhasesOnError(t *testing.T) {
	t.Setenv("ENTIRE_DEBUG", "1")
	var buf bytes.Buffer
	prev := debuglog.SetOutput(&buf)
	t.Cleanup(func() { debuglog.SetOutput(prev) })

	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	client := &http.Client{Transport: &TimingRoundTripper{Next: http.DefaultTransport, Label: "auth"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, addr+"/.well-known/entire-cluster.json", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Error(t, err)

	require.Regexp(t, `timing: auth GET \S+ error=.* conn=new connect_ms=\d+ dur_ms=\d+\n$`, buf.String())
}
