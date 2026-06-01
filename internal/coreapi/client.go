package coreapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// apiBasePath is appended to the control-plane origin to reach the v1
// surface. The generated spec's single server entry is "/api/v1", so the
// origin we dial is <core-host> and the client base is <core-host>/api/v1.
const apiBasePath = "/api/v1"

// New returns a *Client wired to talk to the Entire control plane (Core
// API) as the currently logged-in user.
//
// The base URL is the auth/login host — the Core API is served at
// <auth-host>/api/v1, and that host is exactly what `entire login`
// authenticated against, so no extra configuration is needed. Override
// with ENTIRE_AUTH_BASE_URL for non-default deployments (the same knob
// `entire login` honours).
//
// Auth is the logged-in bearer, resolved lazily per request through
// auth.TokenForResource so an RFC 8693 token exchange happens
// transparently when the stored token's audience doesn't already cover
// the control-plane host.
func New() (*Client, error) {
	base := strings.TrimRight(api.AuthBaseURL(), "/")
	// The token exchange's resource must be the bare origin; api.OriginOnly
	// strips any path/query so the audience the manager keys on matches
	// what the server expects.
	src := &bearerSource{resourceBaseURL: api.OriginOnly(base)}
	client, err := NewClient(base+apiBasePath, src)
	if err != nil {
		return nil, fmt.Errorf("build core API client: %w", err)
	}
	return client, nil
}

// bearerSource implements the generated SecuritySource, supplying the
// logged-in user's bearer token for every request. The control plane
// only uses bearerAuth from the CLI; the sessionAuth (browser cookie)
// scheme is satisfied with an empty value so ogen's two-scheme security
// requirement is met without sending a cookie.
type bearerSource struct {
	resourceBaseURL string
}

func (b *bearerSource) BearerAuth(ctx context.Context, _ OperationName) (BearerAuth, error) {
	token, err := auth.TokenForResource(ctx, b.resourceBaseURL)
	if err != nil {
		// Only suggest login when the user genuinely isn't logged in.
		// Other failures (STS rejection, network, malformed config) must
		// surface verbatim rather than be masked by a login hint.
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return BearerAuth{}, fmt.Errorf("not logged in — run 'entire login': %w", err)
		}
		return BearerAuth{}, fmt.Errorf("resolve control-plane token: %w", err)
	}
	return BearerAuth{Token: token}, nil
}

func (b *bearerSource) SessionAuth(context.Context, OperationName) (SessionAuth, error) {
	// The CLI authenticates with a bearer token, never the browser
	// session cookie. Returning an empty value lets ogen satisfy the
	// "bearerAuth OR sessionAuth" requirement via the bearer alone.
	return SessionAuth{}, nil
}

// APIError reports the title/detail/status of a control-plane RFC 7807
// problem response, or "" if err isn't a control-plane API error. Use it
// to render a clean message instead of ogen's wrapped decode string:
//
//	if _, err := client.CreateOrg(ctx, body); err != nil {
//	    if msg := coreapi.APIError(err); msg != "" {
//	        return cli.NewSilentError(errors.New(msg))
//	    }
//	    return err
//	}
func APIError(err error) string {
	var statusErr *ErrorModelStatusCode
	if !errors.As(err, &statusErr) {
		return ""
	}
	m := statusErr.Response
	switch {
	case m.Detail.Set && m.Detail.Value != "":
		return m.Detail.Value
	case m.Title.Set && m.Title.Value != "":
		return m.Title.Value
	default:
		return fmt.Sprintf("control-plane request failed with status %d", statusErr.StatusCode)
	}
}
