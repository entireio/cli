package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// write421 writes entire-core's cross-jurisdiction redirect envelope: 421
// Misdirected Request naming the home core.
func write421(w http.ResponseWriter, homeCoreURL string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusMisdirectedRequest)
	fmt.Fprintf(w, `{"error":"wrong jurisdiction","home_core_url":"%s","jurisdiction":"eu"}`, homeCoreURL)
}

// writeFederationManifest answers GET /.well-known/entire-federation, naming
// peers as trusted 421 redirect targets. The cross-juris transport refuses to
// follow a redirect to a host absent from this list.
func writeFederationManifest(w http.ResponseWriter, peers ...string) {
	w.Header().Set("Content-Type", "application/json")
	quoted := make([]string, len(peers))
	for i, p := range peers {
		quoted[i] = `"` + p + `"`
	}
	fmt.Fprintf(w, `{"peer_auth_hosts":[%s]}`, strings.Join(quoted, ","))
}

// TestControlPlaneMutation_RejectsOffManifestRedirect pins the one property
// at the command layer that was not already covered elsewhere: a 421 naming
// a home core absent from the responding core's federation manifest is
// refused, not followed.
//
// Two tests already cover neighbouring layers, and neither reaches this one.
// TestCreateAndAwaitMirror_AsyncCrossJurisdiction in repo_mirror_request_test.go
// pins the general follow at the HELPER level: it builds a client with
// coreapi.NewWithBearer and calls addAndAwaitMirror directly, so it runs no
// command and never touches activeCoreClient. TestRoundTripper_Rejects421OffFederation
// in internal/coreapi/cross_juris_client_test.go pins the manifest check at the
// TRANSPORT level. This test is the only one that drives the refusal through
// activeCoreClient and cobra, the way a real command does.
//
// Not parallel: runCoreCmd swaps the package-global activeCoreClient seam.
func TestControlPlaneMutation_RejectsOffManifestRedirect(t *testing.T) {
	// httptest serves each request on its own goroutine, so the counters the
	// assertions read need a lock.
	var mu sync.Mutex
	var homeHits, manifestHits, deleteHits int

	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		homeHits++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(home.Close)

	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/entire-federation" {
			mu.Lock()
			manifestHits++
			mu.Unlock()
			writeFederationManifest(w) // empty peer list: home is not trusted
			return
		}
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleteHits++
			mu.Unlock()
		}
		write421(w, home.URL)
	}))
	t.Cleanup(wrong.Close)

	_, _, err := runCoreCmd(t, newOrgDeleteCmd, wrong.URL, testDeleteULID, "--force")
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	// Assert the refusal happened where it should. An error alone would also be
	// satisfied by a failure raised before the first request, which would leave
	// the manifest check unexercised and this test green for the wrong reason.
	require.Positive(t, deleteHits, "the command must reach the responding core")
	require.Positive(t, manifestHits, "the transport must read the federation manifest")
	require.Equal(t, 0, homeHits, "off-manifest home must receive nothing")
}
