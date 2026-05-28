package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// Token is a single API token row returned by the auth-tokens endpoint.
// Plaintext token values are never returned by the server — only metadata.
type Token struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	ExpiresAt  string  `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	CreatedAt  string  `json:"created_at"`
}

// TokensResponse is the envelope returned by the list endpoint.
type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// errAuthTokensPathUnset surfaces when an auth-tokens method is called
// on a Client that wasn't given a base path. Construct via
// NewClientWithBaseURL(...).WithAuthTokensPath(...) — the active path
// lives in cmd/entire/cli/auth.CurrentProvider().AuthTokensPath, the
// single source of truth for provider-version routing.
var errAuthTokensPathUnset = errors.New("api: auth-tokens path is unset (call (*Client).WithAuthTokensPath before list/revoke)")

func (c *Client) authTokensBasePath() (string, error) {
	if c.authTokensPath == "" {
		return "", errAuthTokensPathUnset
	}
	return c.authTokensPath, nil
}

// ListTokens returns the authenticated user's non-expired API tokens.
func (c *Client) ListTokens(ctx context.Context) ([]Token, error) {
	base, err := c.authTokensBasePath()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	resp, err := c.Get(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}

	var out TokensResponse
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out.Tokens, nil
}

// RevokeCurrentToken revokes the bearer token used to authenticate this client.
func (c *Client) RevokeCurrentToken(ctx context.Context) error {
	base, err := c.authTokensBasePath()
	if err != nil {
		return fmt.Errorf("revoke current token: %w", err)
	}
	resp, err := c.Delete(ctx, base+"/current")
	if err != nil {
		return fmt.Errorf("revoke current token: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return fmt.Errorf("revoke current token: %w", err)
	}
	return nil
}

// RevokeToken revokes the API token with the given id.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	base, err := c.authTokensBasePath()
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	resp, err := c.Delete(ctx, base+"/"+url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	return nil
}
