package httputil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RFC 8693 grant + token-type URNs. Re-export the literals so callers
// composing /oauth/token forms don't keep parallel copies. Lifted out
// of core/repoadmin and core/api during COR-337 cleanup.
const (
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an OAuth grant_type URN, not a credential
	TokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // G101: an RFC 8693 token-type URN, not a credential
)

// BufferRequestBody reads the request body once so a fallback retry
// can replay it. http.NoBody (and nil) short-circuits — both signal
// "no body" but only the latter is a runtime nil, so the explicit
// identity check keeps the cloned request's Content-Length correct on
// the wire. Returns (nil, nil) for no-body requests; the caller can
// safely forward without replay state.
func BufferRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("buffer request body: %w", err)
	}
	return b, nil
}

// BodyReader wraps a buffered request body so http.Request.Body /
// GetBody can replay it across a retry. Pair with BufferRequestBody.
func BodyReader(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}

// cloneValuesWithoutClient returns a shallow copy of v with the
// client_id and client_secret keys removed. Used so we can lift them
// into Basic auth without mutating the caller's form.
func cloneValuesWithoutClient(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		if k == "client_id" || k == "client_secret" {
			continue
		}
		out[k] = vs
	}
	return out
}

// OAuthError is returned by PostOAuthToken when the OAuth endpoint
// responds with a non-2xx status. Callers can errors.As it to surface
// status-specific UX (e.g. a friendly 403 message).
type OAuthError struct {
	Status int
	Body   string
}

func (e *OAuthError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// PostOAuthToken posts a form-encoded request to coreURL+"/oauth/token"
// and parses the standard {access_token, expires_in} response. Callers
// build the form (grant_type, subject_token, audience, etc.) so the
// helper stays neutral about which OAuth grant is being exercised.
//
// If the form carries client_id (and optionally client_secret), the
// helper lifts both into an HTTP Basic Authorization header and drops
// them from the form body. zitadel/oidc's token endpoint reads client
// credentials only from Basic auth, so form-only client_id produces
// invalid_client even when the form is otherwise well-formed. Both values
// are url.QueryEscaped per RFC 6749 §2.3.1 because pkg/op QueryUnescapes
// them on the other side — a raw '+'/'%xx' would round-trip to a different
// value and fail invalid_client (matches core/api/token_endpoint.go).
//
// coreURL must already be trimmed of any trailing slash. A non-2xx
// response is surfaced as *OAuthError; transport and decode failures
// are wrapped plain errors.
func PostOAuthToken(ctx context.Context, httpClient *http.Client, coreURL string, form url.Values) (accessToken string, expiresIn int, err error) {
	clientID := form.Get("client_id")
	clientSecret := form.Get("client_secret")
	if clientID != "" {
		form = cloneValuesWithoutClient(form)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		coreURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientID != "" {
		// RFC 6749 §2.3.1: percent-encode before Basic so pkg/op's
		// QueryUnescape recovers the original (matches token_endpoint.go).
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort body read for error message
		return "", 0, &OAuthError{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, errors.New("token response missing access_token")
	}
	return out.AccessToken, out.ExpiresIn, nil
}
