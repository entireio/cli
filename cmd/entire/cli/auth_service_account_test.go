package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

const testCmdServiceAccount = "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X3"

func runServiceAccountCmd(ctx context.Context, args ...string) (string, string, error) {
	cmd := newAuthServiceAccountCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// fakeServiceAccountServer answers the STS and the proposed key endpoints
// without verifying proofs (the auth package tests do that). It records
// revoke calls and how enrollment polls are answered.
type fakeServiceAccountServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	revokeStatus int
	revoked      []string
	pollStatus   string
	stsStatus    int
	cancelBody   string // non-empty: answer cancel with 409 and this body
}

func newFakeServiceAccountServer(t *testing.T) *fakeServiceAccountServer {
	t.Helper()
	f := &fakeServiceAccountServer{revokeStatus: http.StatusNoContent, pollStatus: "approved", stsStatus: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServiceAccountServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/oauth/token":
		if f.stsStatus != http.StatusOK {
			w.WriteHeader(f.stsStatus)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`) //nolint:errcheck // test server
			return
		}
		// sa_key_id is the minting key: the kid of the submitted assertion.
		_ = r.ParseForm() //nolint:errcheck // a malformed form fails the kid decode below
		var header struct {
			Kid string `json:"kid"`
		}
		if raw, err := base64.RawURLEncoding.DecodeString(strings.Split(r.PostForm.Get("subject_token"), ".")[0]); err == nil {
			_ = json.Unmarshal(raw, &header) //nolint:errcheck // an empty kid is refused by the client
		}
		enc := base64.RawURLEncoding.EncodeToString
		claims := fmt.Sprintf(`{"sub":%q,"aud":%q,"sa_key_id":%q,"exp":%d}`, testCmdServiceAccount, f.srv.URL, header.Kid, time.Now().Add(time.Hour).Unix())
		token := enc([]byte(`{"alg":"EdDSA"}`)) + "." + enc([]byte(claims)) + ".c2ln"
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, token)
	case r.URL.Path == "/api/v1/service-account-enrollments":
		_, _ = io.WriteString(w, `{"enrollment_id":"enr_1","user_code":"WXYZ-1234","verification_uri":"https://entire.example/approve","expires_in":600,"interval":1}`) //nolint:errcheck // test server
	case r.URL.Path == "/api/v1/service-account-enrollments/enr_1/poll":
		fmt.Fprintf(w, `{"status":%q,"service_account":%q}`, f.pollStatus, testCmdServiceAccount)
	case r.URL.Path == "/api/v1/service-account-enrollments/enr_1/cancel":
		if f.cancelBody != "" {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, f.cancelBody) //nolint:errcheck // test server
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/revoke"):
		f.revoked = append(f.revoked, r.URL.Path)
		w.WriteHeader(f.revokeStatus)
	default:
		http.NotFound(w, r)
	}
}

func listKeys(t *testing.T) map[string]auth.ServiceAccountKeyStatus {
	t.Helper()
	keys, err := auth.ListServiceAccountKeys("")
	require.NoError(t, err)
	out := map[string]auth.ServiceAccountKeyStatus{}
	for _, k := range keys {
		out[k.Key.KeyID()] = k.Status
	}
	return out
}

// TestAuthServiceAccountKeyRotation covers `key create|show|list|activate`: a
// second key never displaces the working one until `activate` has proved it
// with a real exchange, and the old key then revokes itself.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR and XDG_CACHE_HOME.
func TestAuthServiceAccountKeyRotation(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := t.Context()
	srv := newFakeServiceAccountServer(t)

	_, _, err := runServiceAccountCmd(ctx, "key", "create", "--service-account", testCmdServiceAccount)
	require.ErrorContains(t, err, "--server is required")

	out, _, err := runServiceAccountCmd(ctx, "key", "create", "--service-account", testCmdServiceAccount, "--server", srv.srv.URL, "--json")
	require.NoError(t, err)
	var first serviceAccountKeyOutput
	require.NoError(t, json.Unmarshal([]byte(out), &first))
	require.Equal(t, testCmdServiceAccount, first.ServiceAccount)
	require.Equal(t, "active", first.Status)
	require.Contains(t, first.PublicKey, "BEGIN PUBLIC KEY")
	require.NotContains(t, out, "PRIVATE KEY", "create must never print the private key")

	out, _, err = runServiceAccountCmd(ctx, "key", "create", "--service-account", testCmdServiceAccount, "--server", srv.srv.URL)
	require.NoError(t, err)
	require.Contains(t, out, "key activate")
	require.NotContains(t, out, "PRIVATE KEY")
	keys := listKeys(t)
	require.Len(t, keys, 2)
	require.Equal(t, auth.KeyActive, keys[first.KeyID], "a second key must not displace the active one")
	var second string
	for id, status := range keys {
		if status == auth.KeyInactive {
			second = id
		}
	}
	require.NotEmpty(t, second)

	out, _, err = runServiceAccountCmd(ctx, "key", "show", "--service-account", testCmdServiceAccount, "--json")
	require.NoError(t, err)
	var shown serviceAccountKeyOutput
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, first, shown)

	// A key the server rejects is never activated.
	srv.stsStatus = http.StatusBadRequest
	_, _, err = runServiceAccountCmd(ctx, "key", "activate", "--service-account", testCmdServiceAccount)
	require.ErrorContains(t, err, "nothing was changed")
	require.Equal(t, auth.KeyActive, listKeys(t)[first.KeyID])

	// Accepted: switch, then the old key revokes itself and is deleted.
	srv.stsStatus = http.StatusOK
	out, _, err = runServiceAccountCmd(ctx, "key", "activate", "--service-account", testCmdServiceAccount)
	require.NoError(t, err)
	require.Contains(t, out, "now uses key "+second)
	require.Equal(t, map[string]auth.ServiceAccountKeyStatus{second: auth.KeyActive}, listKeys(t))
	require.Equal(t, []string{"/api/v1/service-accounts/" + testCmdServiceAccount + "/keys/" + first.KeyID + "/revoke"}, srv.revoked)
}

// A failed self-revoke after activation keeps the retired key so it can retry.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR and XDG_CACHE_HOME.
func TestAuthServiceAccountActivateKeepsKeyWhenRevokeFails(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := t.Context()
	srv := newFakeServiceAccountServer(t)
	srv.revokeStatus = http.StatusInternalServerError

	for range 2 {
		_, _, err := runServiceAccountCmd(ctx, "key", "create", "--service-account", testCmdServiceAccount, "--server", srv.srv.URL)
		require.NoError(t, err)
	}
	_, _, err := runServiceAccountCmd(ctx, "key", "activate", "--service-account", testCmdServiceAccount)
	require.ErrorContains(t, err, "was kept")
	statuses := map[auth.ServiceAccountKeyStatus]int{}
	for _, s := range listKeys(t) {
		statuses[s]++
	}
	require.Equal(t, map[auth.ServiceAccountKeyStatus]int{auth.KeyActive: 1, auth.KeyRetired: 1}, statuses)
}

// TestAuthServiceAccountEnrollInterruptResumeAndCancel: an interrupted wait
// keeps the key for --resume; --cancel withdraws and deletes it; a second
// enrollment while one is pending is refused.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR, XDG_CACHE_HOME and the enrollment
// sleep seam.
func TestAuthServiceAccountEnrollInterruptResumeAndCancel(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := newFakeServiceAccountServer(t)
	srv.pollStatus = "pending"

	// The sleep seam stands in for Ctrl-C: the first wait is interrupted.
	interrupted, cancel := context.WithCancel(t.Context())
	t.Cleanup(auth.SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}))
	_, errOut, err := runServiceAccountCmd(interrupted, "enroll", "--server", srv.srv.URL)
	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, errOut, "WXYZ-1234")
	require.Contains(t, errOut, "--resume")
	keys := listKeys(t)
	require.Len(t, keys, 1)
	for _, s := range keys {
		require.Equal(t, auth.KeyEnrolling, s, "an interrupted enrollment must keep its key")
	}

	_, _, err = runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL)
	require.ErrorIs(t, err, auth.ErrEnrollmentInProgress)

	t.Cleanup(auth.SkipEnrollmentWaitForTest(t))
	srv.pollStatus = "approved"
	out, _, err := runServiceAccountCmd(t.Context(), "enroll", "--resume")
	require.NoError(t, err)
	require.Contains(t, out, "export ENTIRE_SERVICE_ACCOUNT="+testCmdServiceAccount)
	_, err = auth.LoadActiveServiceAccountKey(testCmdServiceAccount)
	require.NoError(t, err, "a resumed, approved enrollment must activate the key")

	// A second enrollment for an account with an active key stays inactive.
	out, _, err = runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL, "--service-account", testCmdServiceAccount)
	require.NoError(t, err)
	require.Contains(t, out, "key activate")

	// Cancel: withdraw a pending enrollment and delete its key.
	srv.pollStatus = "pending"
	t.Cleanup(auth.SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error { return context.Canceled }))
	_, _, err = runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL)
	require.ErrorIs(t, err, context.Canceled)
	before := len(listKeys(t))
	out, _, err = runServiceAccountCmd(t.Context(), "enroll", "--cancel")
	require.NoError(t, err)
	require.Contains(t, out, "Withdrew")
	require.Len(t, listKeys(t), before-1)
}

// TestAuthServiceAccountRevoke: a failed server revoke keeps the key;
// --local-only deletes it without a server call.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR and XDG_CACHE_HOME.
func TestAuthServiceAccountRevoke(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := t.Context()
	srv := newFakeServiceAccountServer(t)
	srv.revokeStatus = http.StatusInternalServerError

	_, _, err := runServiceAccountCmd(ctx, "key", "create", "--service-account", testCmdServiceAccount, "--server", srv.srv.URL)
	require.NoError(t, err)

	_, _, err = runServiceAccountCmd(ctx, "key", "revoke", "--service-account", testCmdServiceAccount)
	require.ErrorContains(t, err, "was kept")
	_, err = auth.LoadActiveServiceAccountKey(testCmdServiceAccount)
	require.NoError(t, err, "a failed server revoke must keep the local key")

	srv.revoked = nil
	_, _, err = runServiceAccountCmd(ctx, "key", "revoke", "--service-account", testCmdServiceAccount, "--local-only")
	require.NoError(t, err)
	_, err = auth.LoadActiveServiceAccountKey(testCmdServiceAccount)
	require.Error(t, err, "revoke must delete the local key")
	require.Empty(t, srv.revoked, "--local-only must not call the server")
	require.Empty(t, listKeys(t))
}

// TestAuthServiceAccountCancelRaceAndEnrollingGuards: an approval that beats
// the cancel keeps (and binds) the key, and an enrolling key cannot be
// revoked or deleted by `key revoke`, even with --key-id or --local-only.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR, XDG_CACHE_HOME and the enrollment
// sleep seam.
func TestAuthServiceAccountCancelRaceAndEnrollingGuards(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := newFakeServiceAccountServer(t)
	srv.pollStatus = "pending"
	t.Cleanup(auth.SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error { return context.Canceled }))

	_, _, err := runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL, "--service-account", testCmdServiceAccount)
	require.ErrorIs(t, err, context.Canceled)
	var keyID string
	for id := range listKeys(t) {
		keyID = id
	}

	for _, args := range [][]string{
		{"key", "revoke", "--service-account", testCmdServiceAccount, "--key-id", keyID},
		{"key", "revoke", "--service-account", testCmdServiceAccount, "--key-id", keyID, "--local-only"},
	} {
		_, _, err = runServiceAccountCmd(t.Context(), args...)
		require.ErrorIs(t, err, auth.ErrKeyEnrolling, args)
	}
	require.Empty(t, srv.revoked)
	require.Equal(t, map[string]auth.ServiceAccountKeyStatus{keyID: auth.KeyEnrolling}, listKeys(t))

	srv.cancelBody = fmt.Sprintf(`{"status":"approved","service_account":%q}`, testCmdServiceAccount)
	_, errOut, err := runServiceAccountCmd(t.Context(), "enroll", "--cancel")
	require.NoError(t, err)
	require.Contains(t, errOut, "approved before it could be cancelled")
	require.Equal(t, map[string]auth.ServiceAccountKeyStatus{keyID: auth.KeyActive}, listKeys(t), "an approved key must be kept and bound")
}

// With several inactive keys, `key activate` without --key-id must ask which
// one, not fall back to finishing retirements.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR and XDG_CACHE_HOME.
func TestAuthServiceAccountActivateAmbiguousKeyNeedsKeyID(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := newFakeServiceAccountServer(t)
	for range 3 {
		_, _, err := runServiceAccountCmd(t.Context(), "key", "create", "--service-account", testCmdServiceAccount, "--server", srv.srv.URL)
		require.NoError(t, err)
	}
	_, _, err := runServiceAccountCmd(t.Context(), "key", "activate", "--service-account", testCmdServiceAccount)
	require.ErrorContains(t, err, "pick one with --key-id")
	require.Empty(t, srv.revoked)
}

// A crash after the pointer write but before the enrollment is cleared leaves
// an active key still in the approved phase; `enroll --resume` must find it
// through the real CLI selection and finish the bind.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR, XDG_CACHE_HOME and package seams.
func TestAuthServiceAccountResumeFinishesInterruptedBind(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Cleanup(auth.SkipEnrollmentWaitForTest(t))
	srv := newFakeServiceAccountServer(t)

	restore := auth.InjectStoreFaultForTest("bind:finish")
	_, _, err := runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL)
	restore()
	require.ErrorContains(t, err, "--resume")
	keys, err := auth.ListServiceAccountKeys("")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, auth.KeyActive, keys[0].Status)
	require.True(t, auth.HasEnrollmentInProgress(keys[0].Key), "the bind was not finished")

	out, _, err := runServiceAccountCmd(t.Context(), "enroll", "--resume")
	require.NoError(t, err)
	require.Contains(t, out, "export ENTIRE_SERVICE_ACCOUNT="+testCmdServiceAccount)
	keys, err = auth.ListServiceAccountKeys("")
	require.NoError(t, err)
	require.False(t, auth.HasEnrollmentInProgress(keys[0].Key), "resume must clear the finished enrollment")

	// The next enrollment is allowed again.
	srv.pollStatus = "pending"
	t.Cleanup(auth.SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error { return context.Canceled }))
	_, _, err = runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL, "--service-account", testCmdServiceAccount)
	require.ErrorIs(t, err, context.Canceled)
}

// --resume and --cancel continue a stored enrollment; flags for a new one are
// refused rather than silently ignored. --local-only needs --cancel and
// abandons a key whose cancellation the server cannot confirm.
//
// Not parallel: it sets ENTIRE_CONFIG_DIR, XDG_CACHE_HOME and package seams.
func TestAuthServiceAccountEnrollFlagsAndLocalAbandon(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := newFakeServiceAccountServer(t)
	srv.pollStatus = "pending"
	t.Cleanup(auth.SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error { return context.Canceled }))
	_, _, err := runServiceAccountCmd(t.Context(), "enroll", "--server", srv.srv.URL)
	require.ErrorIs(t, err, context.Canceled)

	for _, args := range [][]string{
		{"enroll", "--resume", "--server", srv.srv.URL},
		{"enroll", "--cancel", "--service-account", testCmdServiceAccount},
		{"enroll", "--resume", "--name", "x"},
		{"enroll", "--local-only"},
	} {
		_, _, err := runServiceAccountCmd(t.Context(), args...)
		require.Error(t, err, args)
	}

	srv.srv.Close() // the server is gone: cancellation cannot be confirmed
	_, _, err = runServiceAccountCmd(t.Context(), "enroll", "--cancel")
	require.ErrorContains(t, err, "was kept")
	require.Len(t, listKeys(t), 1)

	_, errOut, err := runServiceAccountCmd(t.Context(), "enroll", "--cancel", "--local-only")
	require.NoError(t, err)
	require.Contains(t, errOut, "stays trusted until an admin revokes it")
	require.Empty(t, listKeys(t))
}
