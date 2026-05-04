package api

import (
	"context"
	"fmt"
	"net/url"
)

// Token is a single API token row returned by GET /api/v1/auth/tokens.
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

// TokensResponse is the envelope returned by GET /api/v1/auth/tokens.
type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// ListTokens returns the authenticated user's non-expired API tokens.
// Backed by GET /api/v1/auth/tokens.
func (c *Client) ListTokens(ctx context.Context) ([]Token, error) {
	resp, err := c.Get(ctx, "/api/v1/auth/tokens")
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
// Backed by DELETE /api/v1/auth/tokens/current.
func (c *Client) RevokeCurrentToken(ctx context.Context) error {
	resp, err := c.Delete(ctx, "/api/v1/auth/tokens/current")
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
// Backed by DELETE /api/v1/auth/tokens/{id}.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	resp, err := c.Delete(ctx, "/api/v1/auth/tokens/"+url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	return nil
}
