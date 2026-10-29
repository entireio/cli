package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// TestAuthServiceAccountKeyCmds covers `entire auth service-account key
// create|show`.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR.
func TestAuthServiceAccountKeyCmds(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	const sa = "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3"

	run := func(args ...string) (string, error) {
		cmd := newAuthServiceAccountCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}

	_, err := run("key", "create", "--service-account", sa)
	require.ErrorContains(t, err, "--server is required")

	out, err := run("key", "create", "--service-account", sa, "--server", "https://us.auth.example.com", "--json")
	require.NoError(t, err)
	var created serviceAccountKeyOutput
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	require.Equal(t, sa, created.ServiceAccount)
	require.Equal(t, "https://us.auth.example.com", created.Server)
	require.Contains(t, created.PublicKey, "BEGIN PUBLIC KEY")
	require.NotContains(t, out, "PRIVATE KEY", "create must never print the private key")

	_, err = run("key", "create", "--service-account", sa, "--server", "https://us.auth.example.com")
	require.ErrorContains(t, err, "--force")

	out, err = run("key", "show", "--service-account", sa, "--json")
	require.NoError(t, err)
	var shown serviceAccountKeyOutput
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, created, shown)

	out, err = run("key", "create", "--service-account", sa, "--server", "https://us.auth.example.com", "--force")
	require.NoError(t, err)
	require.Contains(t, out, "export ENTIRE_SERVICE_ACCOUNT="+sa)
	require.NotContains(t, out, created.KeyID, "--force must generate a new key")
}

// fakeServiceAccountKeyServer answers the proposed enrollment and revoke
// endpoints without verifying proofs (the auth package tests do that), and
// records which revoke calls arrived.
func fakeServiceAccountKeyServer(t *testing.T, revokeStatus int, revoked *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/service-account-enrollments":
			_, _ = io.WriteString(w, `{"enrollment_id":"enr_1","user_code":"WXYZ-1234","verification_uri":"https://entire.example/approve","expires_in":600,"interval":1}`) //nolint:errcheck // test server
		case r.URL.Path == "/api/v1/service-account-enrollments/enr_1/poll":
			_, _ = io.WriteString(w, `{"status":"approved","service_account":"svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3"}`) //nolint:errcheck // test server
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			*revoked = append(*revoked, r.URL.Path)
			w.WriteHeader(revokeStatus)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAuthServiceAccountRevokeAndEnroll covers the two paths where a key could
// be lost or orphaned: revoke keeps the local key when the server call fails,
// and enroll revokes an approved key it cannot store.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR and XDG_CACHE_HOME.
func TestAuthServiceAccountRevokeAndEnroll(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Cleanup(auth.SkipEnrollmentWaitForTest(t))
	const sa = "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3"

	run := func(args ...string) (string, string, error) {
		cmd := newAuthServiceAccountCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), errOut.String(), err
	}

	var revoked []string
	srv := fakeServiceAccountKeyServer(t, http.StatusInternalServerError, &revoked)

	out, errOut, err := run("enroll", "--server", srv.URL)
	require.NoError(t, err)
	require.Contains(t, errOut, "WXYZ-1234")
	require.Contains(t, out, "export ENTIRE_SERVICE_ACCOUNT="+sa)
	_, err = auth.LoadServiceAccountKey(sa)
	require.NoError(t, err, "an approved enrollment must store the key")

	_, _, err = run("key", "revoke", "--service-account", sa)
	require.ErrorContains(t, err, "the key was kept")
	_, err = auth.LoadServiceAccountKey(sa)
	require.NoError(t, err, "a failed server revoke must keep the local key")

	// A second enrollment that lands on the same account without --force is
	// approved server-side but cannot be stored: it must be revoked again.
	revoked = nil
	srvOK := fakeServiceAccountKeyServer(t, http.StatusNoContent, &revoked)
	_, _, err = run("enroll", "--server", srvOK.URL)
	require.ErrorContains(t, err, "was revoked again")
	require.Len(t, revoked, 1)

	revoked = nil
	_, _, err = run("key", "revoke", "--service-account", sa, "--local-only")
	require.NoError(t, err)
	_, err = auth.LoadServiceAccountKey(sa)
	require.Error(t, err, "revoke must delete the local key")
	require.Empty(t, revoked, "--local-only must not call the server")
}
