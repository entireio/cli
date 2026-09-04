package auth

import "github.com/entireio/cli/internal/entireclient/httputil"

// OAuth wiring for the entire-cli public client against an entire-core
// login server. Matches an OIDC-standard auth server's discovery doc —
// confirmed against a regional core's (us.auth.entire.io)
// /.well-known/openid-configuration. Device authorization, the loopback
// authorization-code flow, token poll/refresh, and RFC 8693 exchange all
// hit the standard endpoints; grant_type differentiates token vs exchange
// at the shared /oauth/token endpoint.
//
// The paths are fixed rather than discovered, which is what lets login
// start against the apex (auth.entire.io) even though the apex publishes no
// discovery document: only /authorize and /device_authorization are ever
// dialled there, and both are redirected to a region. The token endpoint is
// retargeted at that region mid-login — see UseTokenIssuer in client.go.
const (
	oauthClientID       = httputil.OAuthClientID
	oauthDeviceCodePath = "/device_authorization"
	oauthAuthorizePath  = "/authorize"
	oauthTokenPath      = "/oauth/token" //nolint:gosec // G101: an endpoint path, not a credential
)
