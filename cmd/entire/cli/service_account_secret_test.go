package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	saTestSecretID  = "sas_01HZX7QABCDEFGHJKMNPQRSTSC"
	saTestSecretID2 = "sas_01HZX7QABCDEFGHJKMNPQRSTS2"
	// Not a real secret: the layout only.
	saTestSecret = "ent_svc_us_01HZX7QABCDEFGHJKMNPQRSTSA_01HZX7QABCDEFGHJKMNPQRSTSC_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrsq0trkC6"
)

var saTestSecretsRoute = saBase + "/" + saTestID + "/secrets"

func saTestCreatedSecret() *coreapi.CreateServiceAccountSecretOutputBody {
	return &coreapi.CreateServiceAccountSecretOutputBody{
		ID: saTestSecretID, AccountID: saTestID, Secret: saTestSecret,
		CreatedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2027, 2, 4, 12, 0, 0, 0, time.UTC),
	}
}

// saTestSecretPage serves two pages: an active, used secret, then one that
// expired.
func saTestSecretPage(cursor string) *coreapi.ListServiceAccountSecretsOutputBody {
	if cursor == "" {
		return &coreapi.ListServiceAccountSecretsOutputBody{
			NextCursor: coreapi.NewOptString("page2"),
			Secrets: []coreapi.ServiceAccountSecret{{
				ID: saTestSecretID, AccountID: saTestID,
				CreatedAt:         time.Now().Add(-48 * time.Hour),
				ExpiresAt:         time.Now().Add(30 * 24 * time.Hour),
				LastUsedAt:        coreapi.NewOptDateTime(time.Now().Add(-time.Hour)),
				LastUsedUserAgent: coreapi.NewOptString("entire-sdk-go/0.3.1\x1b]0;pwned\x07\u202e"),
			}},
		}
	}
	return &coreapi.ListServiceAccountSecretsOutputBody{Secrets: []coreapi.ServiceAccountSecret{{
		ID: saTestSecretID2, AccountID: saTestID,
		CreatedAt: time.Now().Add(-100 * 24 * time.Hour),
		ExpiresAt: time.Now().Add(-10 * 24 * time.Hour),
	}}}
}

func runSAFull(t *testing.T, srvURL string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newServiceAccountCmd, srvURL, args...)
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSASecretCreate(t *testing.T) {
	route := "POST " + saTestSecretsRoute

	t.Run("prints only the secret on stdout", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		stdout, stderr, err := runSAFull(t, srv.URL, "secret", "create", "acme/ci")
		require.NoError(t, err)
		require.Equal(t, saTestSecret+"\n", stdout)
		require.Contains(t, stderr, "✓ Created secret "+saTestSecretID+" for acme/ci")
		require.Contains(t, stderr, "not shown again")
		require.NotContains(t, stderr, saTestSecret)
		require.Empty(t, f.body(route), "no flag sends no body, so core picks its default")
	})

	t.Run("sends the lifetime", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, _, err := runSAFull(t, srv.URL, "secret", "create", saTestID, "--expires-in-days", "30")
		require.NoError(t, err)
		require.JSONEq(t, `{"expires_in_days":30}`, f.body(route))
	})

	t.Run("json prints the wire record", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		stdout, _, err := runSAFull(t, srv.URL, "secret", "create", "acme/ci", "--json")
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(stdout), &got))
		require.Equal(t, saTestSecret, got["secret"])
		require.Equal(t, saTestSecretID, got["id"])
	})

	for _, days := range []string{"0", "-1", "366"} {
		t.Run("refuses lifetime "+days, func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			_, _, err := runSAFull(t, srv.URL, "secret", "create", "acme/ci", "--expires-in-days", days)
			require.EqualError(t, err, "--expires-in-days must be between 1 and 365")
			require.NotContains(t, f.callList(), route)
		})
	}

	refusals := []struct {
		name   string
		status int
		detail string
		typ    string
		want   string
	}{
		{"typed limit", http.StatusConflict, "service account already holds the maximum number of active secrets; delete one first", saProblemSecretLimit,
			"acme/ci (" + saTestID + ") already holds two unexpired secrets; delete one first (entire service-account secret list acme/ci)"},
		{"403 keeps core's reason", http.StatusForbidden, "creating a service account secret needs a person's own session", "",
			"cannot create a secret for acme/ci (" + saTestID + "): creating a service account secret needs a person's own session"},
		{"other errors pass through", http.StatusServiceUnavailable, "service account secrets not configured", "", "service account secrets not configured"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			f.fail(route, tc.status, tc.detail, tc.typ)
			stdout, _, err := runSAFull(t, srv.URL, "secret", "create", "acme/ci")
			require.EqualError(t, err, tc.want)
			require.Empty(t, stdout)
		})
	}
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSASecretList(t *testing.T) {
	t.Run("walks every page and marks expired secrets", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "secret", "list", "acme/ci")
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 3, out)
		require.Contains(t, lines[1], saTestSecretID)
		require.Contains(t, lines[1], "active")
		require.Contains(t, lines[1], "entire-sdk-go/0.3.1]0;pwned")
		require.NotContains(t, out, "\x1b", "an escape sequence from a client's User-Agent reaches the terminal")
		require.NotContains(t, out, "\u202e")
		require.Contains(t, lines[2], saTestSecretID2)
		require.Contains(t, lines[2], "expired")
		require.Contains(t, lines[2], "never")
		calls := 0
		for _, c := range f.callList() {
			if c == "GET "+saTestSecretsRoute {
				calls++
			}
		}
		require.Equal(t, 2, calls)
	})

	t.Run("json lists the wire records", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "secret", "list", "acme/ci", "--json")
		require.NoError(t, err)
		var got []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.Len(t, got, 2)
		require.NotContains(t, out, "ent_svc_")
	})
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSASecretDelete(t *testing.T) {
	route := "DELETE " + saTestSecretsRoute + "/" + saTestSecretID

	t.Run("deletes", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "secret", "delete", "acme/ci", saTestSecretID)
		require.NoError(t, err)
		require.Contains(t, out, "✓ Deleted secret "+saTestSecretID+" from acme/ci")
		require.Contains(t, f.callList(), route)
	})

	t.Run("upper-cases a lower-case ID", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		lower := saSecretIDPrefix + strings.ToLower(strings.TrimPrefix(saTestSecretID, saSecretIDPrefix))
		_, err := runSA(t, srv.URL, "secret", "delete", "acme/ci", lower)
		require.NoError(t, err)
		require.Contains(t, f.callList(), route)
	})

	for _, id := range []string{"01HZX7QABCDEFGHJKMNPQRSTSC", "sas_nope", saTestSecret} {
		t.Run("refuses "+id[:min(len(id), 12)], func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			_, err := runSA(t, srv.URL, "secret", "delete", "acme/ci", id)
			require.ErrorContains(t, err, "secret ID must look like sas_<id>")
			require.Empty(t, f.callList())
		})
	}
}

// A missing typed account is an error before any secret call.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestSASecretMissingAccount(t *testing.T) {
	for _, args := range [][]string{
		{"secret", "create", saTestID},
		{"secret", "list", saTestID},
		{"secret", "delete", saTestID, saTestSecretID},
	} {
		t.Run(args[1], func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			f.fail("GET "+saBase+"/"+saTestID, http.StatusNotFound, "service account not found", "")
			_, err := runSA(t, srv.URL, args...)
			require.EqualError(t, err, "no service account "+saTestID)
			for _, c := range f.callList() {
				require.NotContains(t, c, "/secrets")
			}
		})
	}
}
