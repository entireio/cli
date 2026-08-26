package coreapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/userdirs"
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
// control plane onto that login server; with no active context this errors
// with the `entire login` hint. The Core API is served at <host>/api/v1. The
// bearer is resolved lazily per request, re-minting silently from the stored
// refresh token.
//
// For a resource whose home jurisdiction is another region, the client's
// transport follows the home core's 421 redirect and exchanges the login
// token for one that core accepts (see newCrossJurisHTTPClient).
func New() (*Client, error) {
	if client, ok, err := clientFromEnvToken(); ok {
		return client, err
	}
	target, err := auth.ResolveControlPlaneTarget()
	if err != nil {
		return nil, fmt.Errorf("resolve control-plane target: %w", err)
	}
	return clientForTarget(target)
}

// NewForCluster returns a *Client for a resource-provider control-plane command
// whose subject is a mirror on clusterHost (mirror create/remove, mirror
// collaborators list).
//
// The client is the same one New returns — the acting identity is the active
// context (or ENTIRE_TOKEN), and its core is the core we dial. What
// NewForCluster adds is a gate: clusterHost must name a cluster that core
// actually fronts, per its own cluster registry (VerifyClusterRegistered).
//
// The core is deliberately NOT discovered from the cluster's
// /.well-known/entire-cluster.json any more. That document is self-reported by
// the host under scrutiny, and let a host nominate the login servers a client
// should trust it with; the control plane's registry is the authoritative
// record of which clusters exist, and is already what cell routing resolves
// against. The cost of the change is that acting on a cluster fronted by a
// federation other than your active login now fails instead of silently
// switching cores — with an error naming the core consulted and pointing at
// `entire auth use`.
func NewForCluster(ctx context.Context, clusterHost string) (*Client, error) {
	client, err := newActiveClient()
	if err != nil {
		return nil, err
	}
	if err := VerifyClusterRegistered(ctx, client, userdirs.Cache(), client.CoreOrigin(), clusterHost); err != nil {
		return nil, err
	}
	return client, nil
}

// newActiveClient is New, as a seam: NewForCluster's own test needs a client
// pointed at a fake control plane, which no amount of env/context fixture can
// produce (New builds an https client from real credentials).
var newActiveClient = New

// clientFromEnvToken handles the ENTIRE_TOKEN bypass shared by New and
// NewForCluster. ok=true commits the caller to this mode (the var is present);
// ok=false means no env token, so fall through to context resolution.
//
// CI / workload-identity runners inject a short-lived login or sa-session JWT
// and want control-plane commands to use it verbatim, with no contexts.json
// (the runner never ran `entire login`) and no keyring (the runner has none).
// Presence of the var (LookupEnv, including blank) commits the CLI to this mode.
//
// Fail-closed: a blank or malformed value is fatal rather than a silent
// fallback to contexts.json, which would mask a misconfigured runner. The
// token's own aud claim becomes the control-plane origin we dial —
// CoreURLFromEnvToken validates aud is a https bare-origin URL, and makes that
// the resource the static bearer is sent to.
//
// NO CLUSTER GATE — the target-host check lives one level up, in
// NewForCluster / VerifyClusterRegistered, because it needs a cluster host to
// check and New has none: a control-plane command addressed at no particular
// cluster has nothing to anchor against, so the token's aud would only ever be
// gated against itself.
//
// That is safe here because aud-redirection carries no escalation for a
// verbatim bearer: the token IS the credential, so re-pointing aud at an
// attacker host requires already holding a valid token and yields nothing the
// holder didn't already have. (The git remote helper's env-token path uses the
// token as an STS subject_token instead, and does gate it — against the
// registry of the core aud names, anchored by the cluster host the user typed.)
func clientFromEnvToken() (*Client, bool, error) {
	raw, ok := os.LookupEnv(auth.EnvTokenVar)
	if !ok {
		return nil, false, nil
	}
	coreURL, envToken, err := auth.ParseEnvToken(raw)
	if err != nil {
		return nil, true, err //nolint:wrapcheck // auth.ParseEnvToken already prefixes with EnvTokenVar
	}
	client, err := NewWithBearer(coreURL, envToken)
	return client, true, err
}

// clientForTarget builds the *Client for a resolved control-plane target: the
// per-request bearer source plus the cross-juris transport. Shared by New and
// NewForCluster.
func clientForTarget(target auth.ControlPlaneTarget) (*Client, error) {
	src := &providerSource{provide: target.TokenSource}
	client, err := NewClient(strings.TrimRight(target.CoreURL, "/")+apiBasePath, src, WithClient(newCrossJurisHTTPClient()))
	if err != nil {
		return nil, fmt.Errorf("build Entire API client: %w", err)
	}
	return client, nil
}

// CoreOrigin reports the control-plane core origin this client dials —
// scheme://host, the apiBasePath stripped. It is the single source of truth
// for "which core am I talking to": whether the client came from New (active
// context), NewForCluster (the cluster's core), or the ENTIRE_TOKEN bypass
// (the token's aud), the origin is whatever was actually wired in. Use it for
// user-facing "talking to <core>" output so the named core can never diverge
// from where requests go — re-deriving it from ResolveControlPlaneTarget would
// silently miss the ENTIRE_TOKEN and cluster cases.
func (c *Client) CoreOrigin() string {
	return c.serverURL.Scheme + "://" + c.serverURL.Host
}

// NewWithBearer returns a *Client targeting an explicit core origin with a
// fixed bearer token: the token is sent verbatim, not re-resolved or
// re-minted per request. Used when a command must hit a specific login
// server with a token already in hand: e.g. `entire auth status` querying
// /me on the active context's core with that context's session token. A
// cross-jurisdiction call still follows the home core's 421 and exchanges
// this token for that core's audience (see newCrossJurisHTTPClient).
func NewWithBearer(coreBaseURL, token string) (*Client, error) {
	return NewWithBearerSkipTLS(coreBaseURL, token, false)
}

// NewWithBearerSkipTLS is NewWithBearer with the ENTIRE_TLS_SKIP_VERIFY
// local-dev escape hatch plumbed through. Only git-remote-entire passes true:
// it honours that variable for the whole clone, and its cluster-registry check
// must not be the one call that hard-fails against a self-signed dev core.
func NewWithBearerSkipTLS(coreBaseURL, token string, skipTLS bool) (*Client, error) {
	base := strings.TrimRight(coreBaseURL, "/")
	client, err := NewClient(base+apiBasePath, staticBearer{token: token}, WithClient(newCrossJurisHTTPClientSkipTLS(skipTLS)))
	if err != nil {
		return nil, fmt.Errorf("build Entire API client: %w", err)
	}
	return client, nil
}

// NewWithTokenSource returns a *Client that resolves its bearer per request
// from provide — the shape a caller holding a refreshing login credential
// wants, so a registry lookup shares that credential (and its silent re-mint)
// instead of pinning a token snapshot.
func NewWithTokenSource(coreBaseURL string, provide func(context.Context) (string, error), skipTLS bool) (*Client, error) {
	src := &providerSource{provide: provide}
	client, err := NewClient(strings.TrimRight(coreBaseURL, "/")+apiBasePath, src, WithClient(newCrossJurisHTTPClientSkipTLS(skipTLS)))
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
		// The per-context provider returns a tailored message that already
		// names the context, its login server, and the exact re-login command
		// — surface it verbatim rather than burying it under a generic prefix;
		// other failures (STS rejection, network) are likewise
		// self-descriptive. A bare ErrNotLoggedIn (no tailored text) gets the
		// standard login hint as a backstop.
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
