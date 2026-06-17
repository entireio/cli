package auth

// OAuth wiring for the entire-cli public client against an entire-core
// login server. Matches an OIDC-standard auth server's discovery doc —
// confirmed against us.auth.entire.io's /.well-known/openid-configuration.
// Device authorization, the loopback authorization-code flow, token
// poll/refresh, and RFC 8693 exchange all hit the standard endpoints;
// grant_type differentiates token vs exchange at the shared /oauth/token
// endpoint.
const (
	oauthClientID       = "entire-cli"
	oauthDeviceCodePath = "/device_authorization"
	oauthAuthorizePath  = "/authorize"
	oauthTokenPath      = "/oauth/token" //nolint:gosec // G101: an endpoint path, not a credential
	oauthSTSPath        = "/oauth/token" //nolint:gosec // G101: an endpoint path, not a credential
)
