package httputil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// rejectCrossHostRedirect stops a redirect chain from leaving the host the
// request was sent to. Mirrors cmd/entire/cli/api.rejectCrossHostRedirect —
// duplicated rather than imported to avoid a dependency from this
// low-level package onto the CLI's api package. Same-host redirects (e.g.
// a trailing-slash normalize) still follow, up to Go's usual 10-hop cap.
func rejectCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("refusing redirect to a different host (%s -> %s): the OAuth token request body must not leave its origin", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// RFC 8693 grant + token-type URNs. Re-export the literals so callers
// composing /oauth/token forms don't keep parallel copies. Lifted out
// of core/repoadmin and core/api during COR-337 cleanup.
const (
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an OAuth grant_type URN, not a credential
	TokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // G101: an RFC 8693 token-type URN, not a credential
)

// OAuthClientID is the public OAuth client_id the CLI identifies as on
// /oauth/token. Lifted into Basic auth by PostOAuthToken.
const OAuthClientID = "entire-cli"

// TokenExchangeForm builds the RFC 8693 token-exchange form every CLI
// exchange POSTs to /oauth/token: subjectToken (the login JWT) is traded
// for an access token pinned to audience with the given scope. The one
// form shape serves repo-scoped and jurisdiction tokens alike — only
// audience and scope differ — so keep call sites on this builder rather
// than open-coding the fields.
func TokenExchangeForm(subjectToken, audience, scope string) url.Values {
	form := url.Values{}
	form.Set("grant_type", GrantTypeTokenExchange)
	form.Set("subject_token_type", TokenTypeAccessToken)
	form.Set("subject_token", subjectToken)
	form.Set("requested_token_type", TokenTypeAccessToken)
	form.Set("audience", audience)
	form.Set("scope", scope)
	form.Set("client_id", OAuthClientID)
	return form
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
// responds with a non-200 status. Callers can errors.As it to surface
// status-specific UX (e.g. a friendly 403 message) or branch on the
// RFC 6749 error code.
type OAuthError struct {
	Status      int
	Code        string // RFC 6749 `error` code from the response body; "" when not present
	Description string // RFC 6749 `error_description` from the response body; "" when not present
	Body        string
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
// coreURL must already be trimmed of any trailing slash. A non-200
// response is surfaced as *OAuthError (RFC 6749 defines token-endpoint
// success as 200 only); transport and decode failures
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

	// The form body (subject_token, and for password-style exchanges the
	// caller's credentials) must never follow a redirect to a different
	// host: Go's default client only strips sensitive *headers* on a
	// cross-host redirect (net/http's shouldCopyHeaderOnRedirect), and
	// copies the request body unconditionally — a POST body is not
	// protected the way a bearer header is. Guard here, once, for every
	// caller, rather than requiring each constructed *http.Client to set
	// its own CheckRedirect. The guard is applied to a shallow copy so a
	// client instance callers may share elsewhere is not mutated; note
	// that it does take precedence over any CheckRedirect the caller had
	// set, for the duration of this request. No caller sets one today,
	// and a caller that needs a different policy here should be given a
	// same-host-preserving hook rather than having this guard removed.
	guarded := *httpClient
	guarded.CheckRedirect = rejectCrossHostRedirect
	resp, err := guarded.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort body read for error message
		var oauthBody struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(msg, &oauthBody) //nolint:errcheck // best-effort code extraction; non-JSON bodies leave Code empty
		return "", 0, &OAuthError{Status: resp.StatusCode, Code: oauthBody.Code, Description: oauthBody.Description, Body: strings.TrimSpace(string(msg))}
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
