package httputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
