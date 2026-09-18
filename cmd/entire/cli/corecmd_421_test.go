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
// Misdirected Request naming the home core. An empty homeCoreURL omits the
// field, mirroring a home core that can't be determined server-side.
func write421(w http.ResponseWriter, homeCoreURL string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusMisdirectedRequest)
	if homeCoreURL == "" {
		fmt.Fprint(w, `{"error":"wrong jurisdiction"}`)
		return
	}
	fmt.Fprintf(w, `{"error":"wrong jurisdiction","home_core_url":"%s","jurisdiction":"eu"}`, homeCoreURL)
}

// writeFederationManifest answers GET /.well-known/entire-federation,
// naming peers as trusted 421 redirect targets. The cross-juris transport
// refuses to follow a redirect to a host absent from this list, so any test
// whose wrong core is expected to be followed must serve it.
func writeFederationManifest(w http.ResponseWriter, peers ...string) {
	w.Header().Set("Content-Type", "application/json")
	quoted := make([]string, len(peers))
	for i, p := range peers {
		quoted[i] = `"` + p + `"`
	}
	fmt.Fprintf(w, `{"peer_auth_hosts":[%s]}`, strings.Join(quoted, ","))
}

// TestControlPlaneMutation_Follows421 covers the cross-jurisdiction redirect
// a control-plane mutation gets against a resource homed in another region
// (COR-1743): entire-core answers 421 Misdirected Request naming the owning
// core's home_core_url (see docs/adrs and internal/coreapi/cross_juris_client.go).
// The CLI never retries this itself — every coreapi.Client's HTTP transport
// (New, NewForCluster, NewWithBearer all route through
// newCrossJurisHTTPClient) already follows exactly one such redirect via
// auth-go's crossjuris.Transport, replaying the request at the home core. This
// test pins that behavior at the command layer using `entire org delete` as
// the representative mutation, plus the redirect's failure modes.
//
// Not parallel: runCoreCmd swaps the package-global activeCoreClient seam.
func TestControlPlaneMutation_Follows421(t *testing.T) {
	t.Run("421 with home_core_url retries at the redirect target and succeeds", func(t *testing.T) {
		var homeHits, wrongHits int
		home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			homeHits++
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(home.Close)

		wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/entire-federation" {
				writeFederationManifest(w, home.URL)
				return
			}
			wrongHits++
			write421(w, home.URL)
		}))
		t.Cleanup(wrong.Close)

		out, _, err := runCoreCmd(t, newOrgDeleteCmd, wrong.URL, testDeleteULID, "--force")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Deleted org "+testDeleteULID)
		require.Equal(t, 1, wrongHits, "exactly one request to the misdirected core")
		require.Equal(t, 1, homeHits, "the retry landed on the home core")
	})

	t.Run("a second 421 in a row fails without a second retry", func(t *testing.T) {
		var firstHits, secondHits int
		var second *httptest.Server
		second = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			secondHits++
			// Still misdirected; a well-behaved home core wouldn't do this,
			// but a chain must stop after the CLI's one retry regardless.
			write421(w, second.URL)
		}))
		t.Cleanup(second.Close)

		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/entire-federation" {
				writeFederationManifest(w, second.URL)
				return
			}
			firstHits++
			write421(w, second.URL)
		}))
		t.Cleanup(first.Close)

		_, _, err := runCoreCmd(t, newOrgDeleteCmd, first.URL, testDeleteULID, "--force")
		require.Error(t, err)
		require.Equal(t, 1, firstHits)
		require.Equal(t, 1, secondHits, "the retried request lands once at the home core, no further hop")
	})

	t.Run("421 with an empty home_core_url fails without a retry", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			write421(w, "")
		}))
		t.Cleanup(srv.Close)

		_, _, err := runCoreCmd(t, newOrgDeleteCmd, srv.URL, testDeleteULID, "--force")
		require.Error(t, err)
		require.Equal(t, 1, hits, "no retry when the server names no home core")
	})

	t.Run("a non-421 error is unchanged: no retry", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			writeNotFoundProblem(t, w)
		}))
		t.Cleanup(srv.Close)

		out, _, err := runCoreCmd(t, newOrgDeleteCmd, srv.URL, testDeleteULID, "--force")
		require.NoError(t, err, "a 404 on delete is idempotent, not a failure")
		require.Contains(t, out, "not found; nothing to delete")
		require.Equal(t, 1, hits)
	})
}
