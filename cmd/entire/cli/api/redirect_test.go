package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/httpclient/httpclienttest"
)

func TestRequireSameOrigin_RejectsInsecureAbsoluteEndpoint(t *testing.T) {
	t.Parallel()
	if err := requireSameOrigin("https://api.example", "http://api.example/test"); !errors.Is(err, httpclient.ErrRequestPolicy) {
		t.Fatalf("absolute endpoint downgrading an HTTPS base must be a policy rejection, got %v", err)
	}
	if err := requireSameOrigin("https://api.example", "https://other.example/test"); !errors.Is(err, httpclient.ErrRequestPolicy) {
		t.Fatalf("off-host endpoint must be a policy rejection, got %v", err)
	}
	if err := requireSameOrigin("http://127.0.0.1", "http://127.0.0.1/test"); err != nil {
		t.Fatalf("explicit local HTTP base must still work: %v", err)
	}
}

func TestClient_RejectsTLSDowngrade(t *testing.T) {
	t.Parallel()
	httpclienttest.CheckRejectsDowngrade(t, func(t *testing.T, origin string, rt http.RoundTripper) error {
		t.Helper()
		c := NewClientWithBaseURL("test-token", origin)
		transport, ok := c.httpClient.Transport.(*bearerTransport)
		if !ok {
			t.Fatalf("expected bearer transport, got %T", c.httpClient.Transport)
		}
		transport.base = rt
		resp, err := c.Post(t.Context(), "/test", map[string]string{"test": "body"})
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	})
}
