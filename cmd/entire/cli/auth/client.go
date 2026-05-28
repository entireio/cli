package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/auth-go/deviceflow"
	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
)

// nowFunc is the package's clock. Override in tests.
var nowFunc = time.Now

// DeviceAuthStart preserves the historical type name; the shape now
// matches deviceflow.DeviceCode field-for-field.
type DeviceAuthStart = deviceflow.DeviceCode

// DeviceAuthPoll is the historical token-poll response shape. The shim
// flattens deviceflow's typed errors back into the Error field so
// existing login.go logic that switches on result.Error keeps working.
//
// ErrorDescription carries the optional `error_description` from the
// server's RFC 8628 §3.5 error response, when present. Used to give
// callers a more actionable message than the bare error code.
type DeviceAuthPoll struct {
	AccessToken      string
	TokenType        string
	ExpiresIn        int
	Scope            string
	Error            string
	ErrorDescription string
}

// Client wraps a deviceflow.Client preconfigured for whichever provider
// version is selected via ENTIRE_AUTH_PROVIDER_VERSION (defaulting to
// v1).
type Client struct {
	inner *deviceflow.Client
}

// NewClient constructs a Client targeting the active provider version.
// httpClient.Transport is reused when non-nil (its TLS / proxy config
// flows through); a nil httpClient or nil Transport falls back to the
// deviceflow default (http.DefaultTransport).
//
// HTTPS is required by default. Loopback http:// (localhost, 127.0.0.1,
// ::1) is always permitted — see isLoopbackHTTP. allowInsecureHTTP=true
// additionally permits non-loopback http:// for cases like local-dev
// auth hosts on a private network (e.g. http://devbox.internal); the
// CLI plumbs this from the --insecure-http-auth flag.
func NewClient(httpClient *http.Client, allowInsecureHTTP bool) *Client {
	p := CurrentProvider()
	issuer := api.AuthBaseURL()
	var transport http.RoundTripper
	if httpClient != nil {
		transport = httpClient.Transport
	}
	return &Client{inner: &deviceflow.Client{
		Transport:         transport,
		BaseURL:           issuer,
		ClientID:          p.ClientID,
		Scope:             "cli",
		UserAgent:         p.ClientID,
		DeviceCodePath:    p.DeviceCodePath,
		TokenPath:         p.TokenPath,
		AllowInsecureHTTP: allowInsecureHTTP || isLoopbackHTTP(issuer),
	}}
}

// BaseURL returns the issuer base URL this client talks to.
func (c *Client) BaseURL() string { return c.inner.BaseURL }

// StartDeviceAuth requests a fresh device code.
func (c *Client) StartDeviceAuth(ctx context.Context) (*DeviceAuthStart, error) {
	return c.inner.StartDeviceAuth(ctx) //nolint:wrapcheck // shim preserves the lib's wrapped errors verbatim
}

// PollDeviceAuth polls the token endpoint. On any OAuth-protocol error
// (recognised RFC 8628 §3.5 sentinel or unknown but spec-shaped code
// like invalid_request / invalid_client / server_error), the wire-side
// code is returned in DeviceAuthPoll.Error so the existing polling
// loop in login.go can branch on it — known codes hit the dedicated
// switch arms, unknown codes fall through to the default arm and fail
// fast. Non-protocol errors (network, decode) are returned as a real
// error and treated as transient by the polling loop.
func (c *Client) PollDeviceAuth(ctx context.Context, deviceCode string) (*DeviceAuthPoll, error) {
	t, err := c.inner.PollDeviceAuth(ctx, deviceCode)
	if err != nil {
		if code, description, ok := oauthErrorParts(err); ok {
			return &DeviceAuthPoll{
				Error:            code,
				ErrorDescription: description,
			}, nil
		}
		return nil, err //nolint:wrapcheck // shim returns deviceflow errors verbatim so callers can errors.Is on sentinels
	}

	return &DeviceAuthPoll{
		AccessToken: t.AccessToken,
		TokenType:   t.TokenType,
		ExpiresIn:   secondsUntil(t),
		Scope:       t.Scope,
	}, nil
}

// oauthErrorParts inspects err for either a recognised RFC 8628 §3.5
// sentinel or the generic "oauth error: <code>" wrapper deviceflow uses
// for unrecognised but spec-shaped codes (RFC 6749 §5.2: invalid_request,
// invalid_client, server_error, …).
//
// On a match, returns the wire-side code, any error_description the
// server included, and ok=true. Otherwise returns "", "", false — the
// caller should treat the error as a transport/decode failure.
//
// Surfacing unknown codes as ok=true is what keeps login.go's polling
// loop fast-failing on terminal OAuth rejections instead of treating
// them as transient and retrying ~5 times.
func oauthErrorParts(err error) (code, description string, ok bool) {
	switch {
	case errors.Is(err, deviceflow.ErrAuthorizationPending):
		code = "authorization_pending"
	case errors.Is(err, deviceflow.ErrSlowDown):
		code = "slow_down"
	case errors.Is(err, deviceflow.ErrAccessDenied):
		code = "access_denied"
	case errors.Is(err, deviceflow.ErrExpiredToken):
		code = "expired_token"
	case errors.Is(err, deviceflow.ErrInvalidGrant):
		code = "invalid_grant"
	default:
		// Unknown but legitimate OAuth codes come back from
		// deviceflow.errCodeToSentinel as fmt.Errorf("oauth error: %s",
		// code), optionally wrapped a second time with ": <description>"
		// when the server supplied error_description.
		const oauthPrefix = "oauth error: "
		rest, hadPrefix := strings.CutPrefix(err.Error(), oauthPrefix)
		if !hadPrefix {
			return "", "", false
		}
		if c, d, hasDesc := strings.Cut(rest, ": "); hasDesc {
			return c, d, true
		}
		return rest, "", true
	}
	description = descriptionFromSentinelError(err, code)
	return code, description, true
}

// descriptionFromSentinelError pulls the description suffix out of a
// wrapped sentinel error. The deviceflow lib uses
// fmt.Errorf("%w: %s", sentinel, description) when the server included
// an error_description, so the formatted error reads
// "<code>: <description>". Stripping the "<code>: " prefix yields the
// description; absent prefix means the server didn't supply one.
func descriptionFromSentinelError(err error, code string) string {
	msg := err.Error()
	prefix := code + ": "
	if rest, ok := strings.CutPrefix(msg, prefix); ok {
		return rest
	}
	return ""
}

// secondsUntil computes seconds-until-expiry for a TokenSet with an
// absolute ExpiresAt. Returns 0 when no expiry is set or when ExpiresAt
// is already in the past (clock skew, scheduling delays) — historically
// ExpiresIn was non-negative and downstream loggers / display code don't
// expect to see a negative value.
func secondsUntil(t *tokens.TokenSet) int {
	if t.ExpiresAt.IsZero() {
		return 0
	}
	return max(0, int(t.ExpiresAt.Unix()-nowFunc().Unix()))
}
