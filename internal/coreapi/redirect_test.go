package coreapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestNewCrossJurisHTTPClient_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, base http.RoundTripper) error {
		t.Helper()
		client, err := newCrossJurisHTTPClient(origin)
		require.NoError(t, err)
		// Keep the constructor's HTTP redirect policy, rebuilding only its real
		// cross-jurisdiction transport chain over a recording, network-free base.
		client.Transport, err = newCrossJurisRoundTripper(base, isLoopbackHTTP(origin))
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/api/v1/clusters", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := client.Do(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		return err
	})
}
