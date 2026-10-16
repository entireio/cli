package auth

import (
	"net/http"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestCellResolution_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		// Exercise the resolver with an otherwise unguarded client: protection
		// belongs at the request boundary, not only in one client constructor.
		_, err := resolveCellAPIBaseURL(t.Context(), origin, "test-token", "us", &http.Client{Transport: rt})
		return err
	})
}
