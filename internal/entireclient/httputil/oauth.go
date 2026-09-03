// Package httputil holds the OAuth identity the CLI presents to
// entire-core.
//
// It used to carry a second RFC 8693 token-exchange implementation
// (PostOAuthToken and its form builder, error type, and cross-host
// redirect guard). That is gone: every exchange the CLI performs now runs
// through auth-go's sts client — the cross-jurisdiction transport in
// internal/coreapi and the jurisdictional identity token in
// cmd/entire/cli/auth alike — so the wire format, the error taxonomy, the
// terminal-escape sanitisation of server-supplied error text, and the
// redirect policy have exactly one implementation to keep correct.
package httputil

// OAuthClientID is the public OAuth client_id the CLI identifies as on
// /oauth/token. auth-go lifts it into HTTP Basic per RFC 6749 §2.3.1
// (zitadel/oidc's token endpoint reads client credentials only from
// Basic auth, so a form-only client_id produces invalid_client).
const OAuthClientID = "entire-cli"
