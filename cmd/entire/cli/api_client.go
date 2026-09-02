package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
)

// NewAuthenticatedAPIClient creates an API client targeting api.BaseURL()
// (the data API origin) carrying the matching login context's account access
// token (see auth.ResolveDataAPIToken).
//
// Pass insecureHTTP=true to allow plain HTTP base URLs for local
// development. Only the data origin is checked here — the bearer travels
// there on resource requests; the refresh leg is guarded by the per-context
// token manager (https required outside loopback/opt-in).
func NewAuthenticatedAPIClient(ctx context.Context, insecureHTTP bool) (*api.Client, error) {
	dataURL := api.BaseURL()
	if insecureHTTP {
		auth.EnableInsecureHTTP()
	} else if err := api.RequireSecureURL(dataURL); err != nil {
		return nil, fmt.Errorf("base URL check: %w", err)
	}

	// ResolveDataAPIToken discovers which login context the data host trusts
	// (via its /.well-known/entire-api.json) and returns that context's
	// refreshed login JWT. It normalises dataURL to an origin internally.
	token, err := auth.ResolveDataAPIToken(ctx, dataURL)
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

// NewAuthenticatedEntireAPICellClient creates an API client for repo-scoped
// entire-api routes (e.g. trails, experts). It exchanges the login JWT for a
// jurisdictional identity token and dials the entire-api cell directly, because
// the BFF does not proxy these routes for bearer callers (COR-666).
//
// fullName (owner/repo) or ulid identifies the repo whose cell to reach; ulid
// wins when both are set, and both being empty is an error, not a fallback to
// the caller's home cell. The repo's PROCESSING cell + jurisdiction are
// resolved from the control plane
// (mirroring the BFF's per-repo cell selection) so the call lands in the
// region that actually holds the repo's data. This is NOT best-effort: a
// resolution failure fails the command instead of falling back to the
// caller's home cell, because for repo-scoped data a silent wrong-region
// "success" is worse than an error — that fallback is exactly what used to
// make `entire trail`/`entire experts` read the wrong region for a
// multi-homed repo like entirehq/entire.io.
func NewAuthenticatedEntireAPICellClient(ctx context.Context, insecureHTTP bool, fullName, ulid string) (*api.Client, error) {
	target, err := resolveRepoCellTarget(ctx, fullName, ulid)
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
