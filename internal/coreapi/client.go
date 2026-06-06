package coreapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// apiBasePath is appended to the control-plane origin to reach the v1
// surface. The generated spec's single server entry is "/api/v1", so the
// origin we dial is <core-host> and the client base is <core-host>/api/v1.
const apiBasePath = "/api/v1"

// New returns a *Client wired to talk to the Entire control plane (Core
// API) as the currently logged-in user.
//
// The host and bearer come from auth.ResolveControlPlaneTarget. Control-plane
// commands target a login server directly — unlike `git clone` or the data
// API, there's no resource host to match a context against — so the active
// contexts.json login is used as-is, and `entire auth use <ctx>` retargets the
// control plane onto that login server. ENTIRE_AUTH_BASE_URL / the default is
// only the fallback when no context is active, not an override. The Core API
// is served at <host>/api/v1. The bearer is resolved lazily per request; for
// an active context it re-mints silently from the stored refresh token, and
// for the fallback path an RFC 8693 exchange happens transparently when the
// stored token's audience doesn't cover the host.
func New() (*Client, error) {
	target, err := auth.ResolveControlPlaneTarget()
	if err != nil {
		return nil, fmt.Errorf("resolve control-plane target: %w", err)
	}
	src := &providerSource{provide: target.TokenSource}
	client, err := NewClient(strings.TrimRight(target.CoreURL, "/")+apiBasePath, src)
	if err != nil {
		return nil, fmt.Errorf("build Entire API client: %w", err)
	}
	return client, nil
}

// NewWithBearer returns a *Client targeting an explicit core origin with a
// fixed bearer token — no per-request resolution or STS exchange. Used when a
// command must hit a specific login server rather than the configured
// AuthBaseURL: e.g. `entire auth status` querying /me on the active context's
// core with that context's session token.
func NewWithBearer(coreBaseURL, token string) (*Client, error) {
	base := strings.TrimRight(coreBaseURL, "/")
	client, err := NewClient(base+apiBasePath, staticBearer{token: token})
	if err != nil {
		return nil, fmt.Errorf("build Entire API client: %w", err)
	}
	return client, nil
}

// staticBearer is a SecuritySource that returns a fixed bearer token. Same
// sessionAuth-skipping rationale as providerSource.
type staticBearer struct{ token string }

func (s staticBearer) BearerAuth(context.Context, OperationName) (BearerAuth, error) {
	return BearerAuth{Token: s.token}, nil
}

func (s staticBearer) SessionAuth(context.Context, OperationName) (SessionAuth, error) {
	return SessionAuth{}, ogenerrors.ErrSkipClientSecurity
}

// providerSource implements the generated SecuritySource, supplying the
// logged-in user's bearer token for every request from a token-provider
// func (auth.ControlPlaneTarget.TokenSource). The control plane only uses
// bearerAuth from the CLI; the sessionAuth (browser cookie) scheme is
// reported as ErrSkipClientSecurity so ogen's middleware satisfies the
// "bearerAuth OR sessionAuth" requirement via the bearer alone — without
// adding a stray `Cookie: entire_session=` header. (Returning an empty
// SessionAuth would not skip the cookie: the generated securitySessionAuth
// unconditionally calls req.AddCookie.)
type providerSource struct {
	provide func(context.Context) (string, error)
}

func (p *providerSource) BearerAuth(ctx context.Context, _ OperationName) (BearerAuth, error) {
	token, err := p.provide(ctx)
	if err != nil {
		// The static fallback path returns a bare ErrNotLoggedIn sentinel with
		// no helpful text, so add the standard login hint. The active-context
		// path (NewRefreshingLoginProvider) instead returns a tailored message
		// that already names the context, its login server, and the exact
		// re-login command — surface that verbatim rather than burying it under
		// a generic prefix. Other failures (STS rejection, network) are
		// likewise self-descriptive.
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return BearerAuth{}, fmt.Errorf("not logged in — run 'entire login': %w", err)
		}
		return BearerAuth{}, err
	}
	return BearerAuth{Token: token}, nil
}

func (p *providerSource) SessionAuth(context.Context, OperationName) (SessionAuth, error) {
	// The CLI authenticates with a bearer token, never the browser
	// session cookie. ErrSkipClientSecurity tells ogen to drop this
	// scheme entirely for the request (no Cookie header added); the
	// bearerAuth path alone satisfies the OR-requirement.
	return SessionAuth{}, ogenerrors.ErrSkipClientSecurity
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
