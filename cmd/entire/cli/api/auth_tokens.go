package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// Session is a single active login session — an OAuth refresh-token family —
// returned by the auth-tokens endpoint. One is created per `entire login`,
// across all of a user's devices. Plaintext token values are never returned by
// the server, only metadata. (The wire endpoint is historically named
// "tokens"; these rows are sessions, not personal access tokens.)
type Session struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	ExpiresAt  string  `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	CreatedAt  string  `json:"created_at"`
}

// SessionsResponse is the envelope returned by the list endpoint. The wire key
// stays "tokens" — that is the server's contract.
type SessionsResponse struct {
	Sessions []Session `json:"tokens"`
}

// errAuthTokensPathUnset surfaces when a session method is called on a
// Client that wasn't given a base path. Construct via
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

// ListSessions returns the authenticated user's active login sessions.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	base, err := c.authTokensBasePath()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	resp, err := c.Get(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var out SessionsResponse
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out.Sessions, nil
}

// RevokeCurrentSession revokes the login session this client is authenticating
// with (the family the current bearer belongs to).
func (c *Client) RevokeCurrentSession(ctx context.Context) error {
	base, err := c.authTokensBasePath()
	if err != nil {
		return fmt.Errorf("revoke current session: %w", err)
	}
	resp, err := c.Delete(ctx, base+"/current")
	if err != nil {
		return fmt.Errorf("revoke current session: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return fmt.Errorf("revoke current session: %w", err)
	}
	return nil
}

// RevokeSession revokes the login session with the given id.
func (c *Client) RevokeSession(ctx context.Context, id string) error {
	base, err := c.authTokensBasePath()
	if err != nil {
		return fmt.Errorf("revoke session %s: %w", id, err)
	}
	resp, err := c.Delete(ctx, base+"/"+url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("revoke session %s: %w", id, err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return fmt.Errorf("revoke session %s: %w", id, err)
	}
	return nil
}
