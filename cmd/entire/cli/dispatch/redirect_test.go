package dispatch

import (
	"net/http"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestCloudClient_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		c := NewCloudClient(api.NewClientWithTransport("test-token", origin, rt))
		_, err := c.CreateDispatch(t.Context(), CreateDispatchRequest{}, "")
		return err
	})
}
