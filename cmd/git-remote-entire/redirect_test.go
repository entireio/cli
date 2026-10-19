package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestAuthHTTPClientRejectsDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		c := newAuthHTTPClient(httpclient.NewDiscoveryTransport(false), "test")
		c.Transport = rt
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := c.Do(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorIs(t, err, httpclient.ErrInsecureRedirect)
		return err
	})
}
