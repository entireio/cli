package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// AuthSession is a single active login session — an OAuth refresh-token family —
// returned by entire-core's session endpoint. One is created per
// `entire login`, across all of a user's devices. Plaintext token values are
// never returned by the server, only metadata. (The list envelope's wire key
// is "tokens"; the rows are sessions.)
type AuthSession struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	ExpiresAt  string  `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	CreatedAt  string  `json:"created_at"`
}

// AuthSessionsResponse is the envelope returned by the list endpoint.
type AuthSessionsResponse struct {
	Sessions []AuthSession `json:"tokens"`
}

// errAuthSessionsPathUnset surfaces when a session method is called on a Client
// that wasn't given a base path. Construct via
// NewClientWithBaseURL(...).WithAuthSessionsPath(...).
var errAuthSessionsPathUnset = errors.New("api: auth sessions path is unset (call (*Client).WithAuthSessionsPath before list/revoke)")

func (c *Client) authSessionsBasePath() (string, error) {
	if c.authSessionsPath == "" {
		return "", errAuthSessionsPathUnset
	}
	return c.authSessionsPath, nil
}

// ListAuthSessions returns the authenticated user's active login sessions.
func (c *Client) ListAuthSessions(ctx context.Context) ([]AuthSession, error) {
	base, err := c.authSessionsBasePath()
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

	var out AuthSessionsResponse
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out.Sessions, nil
}

// RevokeCurrentAuthSession revokes the login session this client is authenticating
// with (the family the current bearer belongs to).
func (c *Client) RevokeCurrentAuthSession(ctx context.Context) error {
	base, err := c.authSessionsBasePath()
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

// RevokeAuthSession revokes the login session with the given id.
func (c *Client) RevokeAuthSession(ctx context.Context, id string) error {
	base, err := c.authSessionsBasePath()
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
