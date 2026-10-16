package httpclient_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestNewRejectsDowngradesByDefault(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		c := httpclient.New(httpclient.Options{Transport: rt})
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

func TestWithSecureRedirectsNilUsesDefaultClient(t *testing.T) {
	t.Parallel()
	c := httpclient.WithSecureRedirects(nil)
	require.NotSame(t, http.DefaultClient, c)
	require.Equal(t, http.DefaultClient.Transport, c.Transport)
	require.Equal(t, http.DefaultClient.Timeout, c.Timeout)
	require.Equal(t, http.DefaultClient.Jar, c.Jar)
	require.NotNil(t, c.CheckRedirect)
}
