package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// NewAuthenticatedAPIClient creates an API client targeting api.BaseURL()
// (the data API origin) carrying a token valid for that audience.
//
// Resolution: looks up the core token from the keyring, then either uses
// it directly (single-host setup, or when the core token's `aud` already
// covers api.BaseURL()) or performs an RFC 8693 token exchange against
// the auth host to obtain a token scoped to the data API. Exchanged
// tokens are cached in-memory keyed off the wire-affecting fields of
// the request — see tokenmanager.cacheKey for the precise key shape.
//
// Pass insecureHTTP=true to allow plain HTTP base URLs for local
// development. Both api.BaseURL() and api.AuthBaseURL() are validated:
// the bearer travels to the data host on resource requests, and the
// core token travels to the auth host during the exchange step.
func NewAuthenticatedAPIClient(ctx context.Context, insecureHTTP bool) (*api.Client, error) {
	dataURL, authURL := api.BaseURL(), api.AuthBaseURL()
	if insecureHTTP {
		auth.EnableInsecureHTTP()
	} else {
		if err := api.RequireSecureURL(dataURL); err != nil {
			return nil, fmt.Errorf("base URL check: %w", err)
		}
		if authURL != dataURL {
			if err := api.RequireSecureURL(authURL); err != nil {
				return nil, fmt.Errorf("auth base URL check: %w", err)
			}
		}
	}

	// tokenmanager validates Resource as a strict origin URL; strip any path
	// the operator may have included in ENTIRE_API_BASE_URL before handing
	// it across the package boundary.
	token, err := auth.TokenForResource(ctx, api.OriginOnly(dataURL))
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			// Wrap the original err (not the sentinel) so any context
			// the tokenmanager attached — keyring backend message,
			// expired-token reason — survives to the caller. The
			// errors.Is(err, auth.ErrNotLoggedIn) chain is preserved
			// because err already wraps the sentinel; replacing it
			// with the bare sentinel would drop that context for
			// zero behavioural gain.
			return nil, fmt.Errorf("not logged in (run 'entire login' first): %w", err)
		}
		return nil, fmt.Errorf("resolve API token: %w", err)
	}

	return api.NewClient(token), nil
}
