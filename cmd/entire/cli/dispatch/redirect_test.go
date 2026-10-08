package dispatch

import (
	"net/http"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestCloudClient_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"default client", "injected client"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
				t.Helper()
				cfg := CloudConfig{BaseURL: origin, Token: "test-token"}
				if mode == "injected client" {
					cfg.HTTP = &http.Client{Transport: rt}
				}
				c := NewCloudClient(cfg)
				c.http.Transport = rt
				_, err := c.CreateDispatch(t.Context(), CreateDispatchRequest{}, "")
				return err
			})
		})
	}
}
