package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entiredb/tokenstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AuthRefreshConfig contains configuration for token refresh.
type AuthRefreshConfig struct {
	// KeyringService is the full tokenstore service name to look the
	// access token up under, e.g. "entire-core:https://core.example".
	// Refresh tokens live at KeyringService + ":refresh".
	KeyringService string
	BaseURL        string // Node URL — informational; refresh always goes through CoreBaseURL.
	Username       string
	// CoreBaseURL is entire-core's base URL — used to refresh tokens via
	// /oauth/token. Required for refresh; callers populate it from
	// the resolved context's CoreURL (contexts.json) or an explicit
	// override.
	CoreBaseURL string
	HTTPClient  *http.Client
	// ClientID identifies the OAuth client refresh exchanges run under.
	// pkg/op enforces issuer-client binding on /oauth/token; the value
	// must match the client_id the original auth-code or device-code
	// grant was issued to.
	ClientID string
}

// CoreRefreshTokenPrefix identifies opaque entire refresh tokens. Used
// as a sanity check before POSTing to /oauth/token — stale non-entire
// tokens in the keyring (e.g. from retired login flows) error out
// cleanly instead of generating spurious 4xx server traffic.
const CoreRefreshTokenPrefix = "entr_"

// ErrSessionExpired signals the stored refresh token has been rejected by
// entire-core (invalid_grant — expired, revoked, or replayed) and the user
// must re-authenticate. Callers should surface it as-is rather than retrying
// with the cached access token, which is also dead. The wrapped error
// message includes the --base-url hint pointing at the same core the failed
// refresh hit, so the user can copy/paste it.
var ErrSessionExpired = errors.New("your session has expired")

// TokenResponse represents the response from a token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// AuthInterceptor creates a simple auth interceptor without refresh capability.
// Use AuthInterceptorWithRefresh for automatic token refresh.
func AuthInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// Skip auth for login endpoint
		if method == "/proto.user.v1.User/Login" {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		// Add authorization header
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// AuthInterceptorWithRefresh creates an auth interceptor that automatically
// refreshes the access token when it expires or is about to expire.
func AuthInterceptorWithRefresh(cfg AuthRefreshConfig, token *string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// Skip auth for login endpoint
		if method == "/proto.user.v1.User/Login" {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		currentToken := *token

		// Proactively refresh if token is expiring soon
		if currentToken != "" {
			currentToken = proactiveRefresh(ctx, cfg, token, currentToken)
		}

		// Make request with current token
		if currentToken != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+currentToken)
		}

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err == nil {
			return nil
		}

		// Check if error is "token expired" (fallback for clock skew or race conditions)
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.Unauthenticated || st.Message() != "token expired" {
			return err // Not a token expiry error, return as-is
		}

		// Try to refresh the token
		newToken, refreshErr := RefreshAccessToken(ctx, cfg)
		if refreshErr != nil {
			return fmt.Errorf("token expired and refresh failed: %w", refreshErr)
		}

		// Update the token pointer so subsequent calls use the new token
		*token = newToken

		// Retry with new token - create fresh context without old auth header
		retryCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+newToken)
		return invoker(retryCtx, method, req, reply, cc, opts...)
	}
}

// AuthTransport injects the current bearer token into HTTP requests. The repo
// client owns refresh/retry so it can safely replay buffered request bodies.
func AuthTransport(token *string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &authTransport{base: base, token: token}
}

type authTransport struct {
	base  http.RoundTripper
	token *string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != nil && *t.token != "" && req.Header.Get("Authorization") == "" {
		clone := req.Clone(req.Context())
		clone.Header.Set("Authorization", "Bearer "+*t.token)
		req = clone
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("auth transport round trip: %w", err)
	}
	return resp, nil
}

// proactiveRefresh checks if the token is expiring soon and refreshes it if needed.
// Returns the current token (possibly refreshed).
func proactiveRefresh(ctx context.Context, cfg AuthRefreshConfig, tokenPtr *string, currentToken string) string {
	// Read encoded token from keyring to check expiration
	encodedToken, err := tokenstore.Get(cfg.KeyringService, cfg.Username)
	if err != nil {
		return currentToken // Can't check expiration, use current token
	}

	_, expiresAt := tokenstore.DecodeTokenWithExpiration(encodedToken)
	if !tokenstore.IsTokenExpiredOrExpiring(expiresAt) {
		return currentToken // Token still valid, no refresh needed
	}

	// Token is expiring soon, try to refresh
	newToken, err := RefreshAccessToken(ctx, cfg)
	if err != nil {
		// Best effort: if refresh fails, continue with current token
		// The reactive refresh will catch it if the server rejects
		return currentToken
	}

	*tokenPtr = newToken
	return newToken
}

// RefreshAccessToken refreshes the access token using the refresh token stored
// in the keyring. The refresh hits entire-core's /api/auth/refresh endpoint;
// non-core refresh tokens (e.g. residue from retired flows) error rather than
// being POSTed. Returns the new access token after persisting both tokens.
func RefreshAccessToken(ctx context.Context, cfg AuthRefreshConfig) (string, error) {
	refreshService := cfg.KeyringService + ":refresh"
	debugf("tokenstore.Get(service=%s user=%s) for refresh token", refreshService, cfg.Username)
	refreshToken, err := tokenstore.Get(refreshService, cfg.Username)
	if err != nil {
		return "", fmt.Errorf("no refresh token available: %w", err)
	}

	if !strings.HasPrefix(refreshToken, CoreRefreshTokenPrefix) {
		return "", errors.New("refresh token is not a core-issued token; please log in again")
	}
	if cfg.CoreBaseURL == "" {
		return "", errors.New("no core URL configured for refresh — caller must set AuthRefreshConfig.CoreBaseURL from contexts.json or an explicit override")
	}
	tokenResp, err := refreshViaCore(ctx, cfg, refreshToken)
	if err != nil {
		return "", err
	}
	debugf("refresh response: expires_in=%ds, refresh_token rotated=%v", tokenResp.ExpiresIn, tokenResp.RefreshToken != "")

	encodedToken := tokenstore.EncodeTokenWithExpiration(tokenResp.AccessToken, tokenResp.ExpiresIn)
	if err := tokenstore.Set(cfg.KeyringService, cfg.Username, encodedToken); err != nil {
		return "", fmt.Errorf("storing new access token: %w", err)
	}

	if tokenResp.RefreshToken != "" {
		//nolint:errcheck // Refresh token storage is best-effort
		_ = tokenstore.Set(refreshService, cfg.Username, tokenResp.RefreshToken)
	}

	return tokenResp.AccessToken, nil
}

// refreshViaCore exchanges a core refresh token at entire-core's canonical
// /oauth/token endpoint (RFC 6749 §6 refresh-token grant). Single-use: if
// pkg/op detects a replayed token it returns invalid_grant and revokes the
// whole family — the caller should surface that as "please log in again".
func refreshViaCore(ctx context.Context, cfg AuthRefreshConfig, refreshToken string) (TokenResponse, error) {
	if cfg.ClientID == "" {
		return TokenResponse{}, errors.New("refresh: missing OAuth client_id — caller must set AuthRefreshConfig.ClientID")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.ClientID)

	endpoint := strings.TrimRight(cfg.CoreBaseURL, "/") + "/oauth/token"
	debugf("POST %s (refresh access token)", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("creating refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("sending refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) //nolint:errcheck // best-effort body read for diagnostics
	if resp.StatusCode != http.StatusOK {
		debugf("refresh endpoint returned HTTP %d", resp.StatusCode)
		var oerr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oerr) //nolint:errcheck // best-effort decode
		if oerr.Error == "invalid_grant" {
			hint := fmt.Sprintf("please run 'entire-core auth login --base-url %s'", strings.TrimRight(cfg.CoreBaseURL, "/"))
			if oerr.ErrorDescription != "" {
				return TokenResponse{}, fmt.Errorf("%w — %s (refresh token rejected: %s)", ErrSessionExpired, hint, oerr.ErrorDescription)
			}
			return TokenResponse{}, fmt.Errorf("%w — %s", ErrSessionExpired, hint)
		}
		if oerr.Error != "" {
			return TokenResponse{}, fmt.Errorf("core refresh failed (HTTP %d, %s): %s", resp.StatusCode, oerr.Error, oerr.ErrorDescription)
		}
		return TokenResponse{}, fmt.Errorf("core refresh failed with status %d", resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return TokenResponse{}, fmt.Errorf("decoding refresh response: %w", err)
	}
	return tokenResp, nil
}

// GetTokenWithRefresh retrieves a valid OAuth token, refreshing if expired.
// cfg.KeyringService is the tokenstore service name to look the access
// token up under. Returns the token or an error. On refresh failure,
// returns the existing token with a warning.
func GetTokenWithRefresh(ctx context.Context, cfg AuthRefreshConfig) (string, error) {
	debugf("tokenstore.Get(service=%s user=%s) for access token", cfg.KeyringService, cfg.Username)
	encodedToken, err := tokenstore.Get(cfg.KeyringService, cfg.Username)
	if err != nil {
		if errors.Is(err, tokenstore.ErrNotFound) {
			return "", fmt.Errorf("no token found for %s in %s - please run 'entire-core auth login'", cfg.Username, cfg.KeyringService)
		}
		return "", fmt.Errorf("failed to get token from keyring: %w", err)
	}

	// Decode token and check expiration
	token, expiresAt := tokenstore.DecodeTokenWithExpiration(encodedToken)

	// If token is expired or expiring soon, try to refresh
	if tokenstore.IsTokenExpiredOrExpiring(expiresAt) {
		debugf("access token expired or expiring (expiresAt=%s); refreshing via %s/oauth/token", expiresAt.Format(time.RFC3339), cfg.CoreBaseURL)
		newToken, refreshErr := RefreshAccessToken(ctx, cfg)
		if refreshErr != nil {
			// invalid_grant means the refresh family is dead — the cached
			// access token will also fail (typically with subject_token is
			// invalid on the next STS exchange), so fail fast with an
			// actionable message instead of letting the user chase a
			// downstream 400.
			if errors.Is(refreshErr, ErrSessionExpired) {
				return "", refreshErr
			}
			// Transient refresh failures (network, 5xx) fall back to the
			// existing token in case it's still accepted by the resource
			// server — better than failing outright on a flaky network.
			fmt.Fprintf(os.Stderr, "Warning: token refresh failed: %v\n", refreshErr)
		} else {
			debugf("access token refresh succeeded; using new token")
			token = newToken
		}
	} else {
		debugf("access token still valid (expiresAt=%s); skipping refresh", expiresAt.Format(time.RFC3339))
	}

	return token, nil
}

// debugf writes to stderr when ENTIRE_DEBUG is set. Mirrors the gate used in
// core/authn/device_flow.go so all CLI debug output toggles from one env var.
func debugf(format string, args ...any) {
	if os.Getenv("ENTIRE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[client] "+format+"\n", args...)
	}
}
