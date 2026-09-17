package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
)

// NewAuthenticatedAPIClient creates a data API client for the selected login
// (see auth.ResolveDataAPI).
//
// Pass insecureHTTP=true to allow plain HTTP base URLs for local
// development. Only the data origin is checked here — the bearer travels
// there on resource requests; the refresh leg is guarded by the per-context
// token manager (https required outside loopback/opt-in).
func NewAuthenticatedAPIClient(ctx context.Context, insecureHTTP bool) (*api.Client, error) {
	if insecureHTTP {
		auth.EnableInsecureHTTP()
	} else if err := requireSecureDataOverride(); err != nil {
		return nil, err
	}
	target, err := auth.ResolveDataAPI(ctx)
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			// Keep err in the chain: it carries the keyring or expiry detail.
			return nil, fmt.Errorf("not logged in (run 'entire login' first): %w", err)
		}
		return nil, fmt.Errorf("resolve API token: %w", err)
	}
	if !insecureHTTP {
		if err := api.RequireSecureURL(target.BaseURL); err != nil {
			return nil, fmt.Errorf("base URL check: %w", err)
		}
	}
	return api.NewClientWithBaseURL(target.Token, target.BaseURL), nil
}

// requireSecureDataOverride rejects an http ENTIRE_API_BASE_URL before any
// credential resolution runs against it.
func requireSecureDataOverride() error {
	if dataURL, ok := api.BaseURLOverride(); ok {
		if err := api.RequireSecureURL(dataURL); err != nil {
			return fmt.Errorf("base URL check: %w", err)
		}
	}
	return nil
}

// NewAuthenticatedEntireAPICellClient creates an API client for repo-scoped
// entire-api routes (e.g. trails). It exchanges the login JWT for a
// jurisdictional identity token and dials the entire-api cell directly, because
// the BFF does not proxy these routes for bearer callers.
//
// fullName (owner/repo) identifies the repo whose cell to reach. The repo's
// PROCESSING cell + jurisdiction are resolved from the control plane
// (mirroring the BFF's per-repo cell selection) so the call lands in the
// region that actually holds the repo's data. This is NOT best-effort: a
// resolution failure fails the command instead of falling back to the
// caller's home cell, because for repo-scoped data a silent wrong-region
// "success" is worse than an error — that fallback is exactly what used to
// make `entire trail` read the wrong region for a multi-homed repo like
// entirehq/entire.io.
func NewAuthenticatedEntireAPICellClient(ctx context.Context, insecureHTTP bool, fullName string) (*api.Client, error) {
	target, err := resolveRepoCellTarget(ctx, fullName, "")
	if err != nil {
		return nil, err
	}
	// NewEntireAPICellClient already returns user-facing, context-rich errors
	// (login hint, discovery-unavailable, region guidance); re-wrapping here
	// would bury them, so surface them verbatim.
	return auth.NewEntireAPICellClient(ctx, insecureHTTP, target) //nolint:wrapcheck // pass through contextual auth errors
}

// newTrailAPIClient dials the entire-api cell that owns the forge-qualified
// repository and returns its repo_id for repo-addressed trail reads. It is a
// package seam so tests can substitute a client pointed at a stub server.
var newTrailAPIClient = func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (*api.Client, string, error) {
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil {
		return nil, "", err
	}
	client, err := auth.NewEntireAPICellClient(ctx, insecureHTTP, placement.Target)
	if errors.Is(err, clusterdiscovery.ErrNoAuthContext) {
		// Preserve cluster discovery's detailed host/context hint while restoring
		// the sentinel trail commands use for the standard login UX.
		return nil, "", fmt.Errorf("%w: %w", auth.ErrNotLoggedIn, err)
	}
	if err != nil {
		return nil, "", err //nolint:wrapcheck // auth client returns contextual, user-facing errors
	}
	return client, placement.RepoID, nil
}
