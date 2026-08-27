package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// Plugin asset downloads go out through one shared client, so the User-Agent
// belongs on its transport rather than at the two call sites (httpGetSmall and
// the asset download) that use it.
func TestPluginHTTPClient_SendsVersionedUserAgent(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Write([]byte("ok")) //nolint:errcheck // test fixture response
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/checksums.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pluginHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	want := versioninfo.UserAgent()
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("User-Agent seen = %v, want [%s]", seen, want)
	}
}
