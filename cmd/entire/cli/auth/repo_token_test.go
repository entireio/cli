package auth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// captureTransport records the last request's parsed form body and
// returns a canned RFC 8693 token-exchange success response.
type captureTransport struct {
	form url.Values
	url  string
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	c.form = form
	c.url = req.URL.String()
	resp := `{"access_token":"repo-scoped.jwt","token_type":"Bearer",` +
		`"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":300}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(resp)),
		Request:    req,
	}, nil
}

// fakeBackend returns a fixed login token for any keyring lookup so the
// exchange has a subject token without touching the OS keyring.
type fakeBackend struct{ token string }

func (f fakeBackend) save(string, string, string) error  { return nil }
func (f fakeBackend) get(string, string) (string, error) { return f.token, nil }
func (f fakeBackend) delete(string, string) error        { return nil }

func TestRepoScopedToken_WireForm(t *testing.T) {
	t.Setenv(api.AuthBaseURLEnvVar, "https://us.auth.entire.io")

	prevBackend := chooseBackend
	chooseBackend = func() tokenBackend { return fakeBackend{token: "login.jwt.value"} }
	t.Cleanup(func() { chooseBackend = prevBackend })

	capture := &captureTransport{}
	t.Cleanup(SetRepoExchangeTransportForTest(capture))

	tok, err := RepoScopedToken(context.Background(),
		"https://aws-us-east-2.entire.io/", "/gh/octocat/hello", "pull")
	if err != nil {
		t.Fatalf("RepoScopedToken: %v", err)
	}
	if tok != "repo-scoped.jwt" {
		t.Errorf("token = %q, want %q", tok, "repo-scoped.jwt")
	}

	// Endpoint: the configured core's STS path.
	if !strings.HasPrefix(capture.url, "https://us.auth.entire.io/") {
		t.Errorf("exchange URL = %q, want core host prefix", capture.url)
	}

	// Wire form must match what the data plane's git gate accepts.
	want := map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token":        "login.jwt.value",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:access_token",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"audience":             "https://aws-us-east-2.entire.io/gh/octocat/hello",
		"scope":                "repo:pull",
		"client_id":            "entire-cli",
	}
	for k, v := range want {
		if got := capture.form.Get(k); got != v {
			t.Errorf("form[%q] = %q, want %q", k, got, v)
		}
	}

	// resource must NOT be sent — the gate keys on audience alone, and a
	// divergent resource param risks the server validating the wrong value.
	if capture.form.Has("resource") {
		t.Errorf("form unexpectedly includes resource=%q", capture.form.Get("resource"))
	}
}
