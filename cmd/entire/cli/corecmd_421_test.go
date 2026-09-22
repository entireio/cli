package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
// refused, not followed. The general follow (COR-1743) is already pinned at
// the command layer by TestCreateAndAwaitMirror_AsyncCrossJurisdiction in
// repo_mirror_request_test.go, and the manifest check itself by
// TestRoundTripper_Rejects421OffFederation in
// internal/coreapi/cross_juris_client_test.go; neither drives this refusal
// through activeCoreClient and cobra the way a real command does.
//
// Not parallel: runCoreCmd swaps the package-global activeCoreClient seam.
func TestControlPlaneMutation_RejectsOffManifestRedirect(t *testing.T) {
	var homeHits int
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		homeHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(home.Close)

	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/entire-federation" {
			writeFederationManifest(w) // empty peer list: home is not trusted
			return
		}
		write421(w, home.URL)
	}))
	t.Cleanup(wrong.Close)

	_, _, err := runCoreCmd(t, newOrgDeleteCmd, wrong.URL, testDeleteULID, "--force")
	require.Error(t, err)
	require.Equal(t, 0, homeHits, "off-manifest home must receive nothing")
}
