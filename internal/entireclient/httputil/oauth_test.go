package httputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostOAuthToken_LiftsAndPercentEncodesClientCreds pins that a form
// client_id/secret is lifted into Basic, dropped from the body, and
// url.QueryEscaped per RFC 6749 §2.3.1 — so pkg/op's QueryUnescape on the
// other side recovers the original. A raw '+'/'%xx' would otherwise round-trip
// to a different value and fail invalid_client. Matches token_endpoint.go.
func TestPostOAuthToken_LiftsAndPercentEncodesClientCreds(t *testing.T) {
	var gotUser, gotPass string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_ = r.ParseForm() //nolint:errcheck // test stub
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":900}`)) //nolint:errcheck // test stub
	}))
	defer srv.Close()

	form := url.Values{}
	form.Set("grant_type", GrantTypeTokenExchange)
	form.Set("client_id", "cli+id")
	form.Set("client_secret", "se+cr%et")

	tok, exp, err := PostOAuthToken(context.Background(), srv.Client(), srv.URL, form)
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	assert.Equal(t, 900, exp)

	// r.BasicAuth base64-decodes but does not unescape; QueryUnescape mirrors
	// what pkg/op does next and must recover the originals.
	gotID, err := url.QueryUnescape(gotUser)
	require.NoError(t, err)
	gotSecret, err := url.QueryUnescape(gotPass)
	require.NoError(t, err)
	assert.Equal(t, "cli+id", gotID, "client_id must round-trip through QueryEscape→base64→QueryUnescape")
	assert.Equal(t, "se+cr%et", gotSecret, "client_secret must round-trip too")

	assert.Empty(t, gotForm.Get("client_id"), "client_id must be dropped from the body once lifted into Basic")
	assert.Empty(t, gotForm.Get("client_secret"), "client_secret must be dropped from the body")
}

// TestPostOAuthToken_RefusesCrossHostRedirect proves the subject_token (the
// caller's login JWT) never reaches a different host, even when the origin
// server issues a 307/308 redirect. Go's default client only strips
// sensitive *headers* on a cross-host redirect (shouldCopyHeaderOnRedirect in
// net/http) — the POST body, where subject_token actually lives, is copied
// unconditionally. This is a genuine two-server reproduction: origin
// redirects to attacker, and the test fails if attacker ever sees the token.
func TestPostOAuthToken_RefusesCrossHostRedirect(t *testing.T) {
	t.Parallel()

	const secretSubjectToken = "super-secret-login-jwt"

	// atomic.Bool because the write happens on the attacker server's
	// handler goroutine and the read below happens on the test goroutine.
	// The handler never runs while the guard works, so this does not race
	// in practice — it is atomic so that a future regression is reported
	// as a failed assertion rather than as a data race.
	var attackerSawToken atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm() //nolint:errcheck // test stub
		if r.PostForm.Get("subject_token") == secretSubjectToken {
			attackerSawToken.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stolen","expires_in":900}`)) //nolint:errcheck // test stub
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/oauth/token", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	form := url.Values{}
	form.Set("grant_type", GrantTypeTokenExchange)
	form.Set("subject_token", secretSubjectToken)

	_, _, err := PostOAuthToken(context.Background(), origin.Client(), origin.URL, form)

	// The leak check comes first, deliberately. It is the assertion that
	// pins this test's headline claim, and require.Error below aborts the
	// test — so with the two in the other order any regression failed on
	// the error assertion alone and never evaluated whether the token had
	// actually reached the attacker.
	assert.False(t, attackerSawToken.Load(), "subject_token must never reach a host other than the one the caller targeted")
	require.Error(t, err, "a cross-host redirect must be refused, not silently followed")
}

// TestPostOAuthToken_ErrorCode pins that a non-200 response surfaces as
// *OAuthError with the RFC 6749 `error` code and `error_description` parsed
// from the body (both empty for non-JSON bodies), so callers can branch on
// e.g. invalid_target and render the description (the git-remote-entire
// wrong-cluster UX relies on Description being extracted here).
func TestPostOAuthToken_ErrorCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, wantCode, wantDesc string
	}{
		{"json error code", `{"error":"invalid_target","error_description":"no mirror"}`, "invalid_target", "no mirror"},
		{"non-json body", `gateway exploded`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body)) //nolint:errcheck // test stub
			}))
			defer srv.Close()

			_, _, err := PostOAuthToken(context.Background(), srv.Client(), srv.URL, url.Values{})
			var oe *OAuthError
			require.ErrorAs(t, err, &oe)
			assert.Equal(t, http.StatusBadRequest, oe.Status)
			assert.Equal(t, tc.wantCode, oe.Code)
			assert.Equal(t, tc.wantDesc, oe.Description)
			assert.Equal(t, tc.body, oe.Body)
		})
	}
}
