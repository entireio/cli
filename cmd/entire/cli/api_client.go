package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// NewAuthenticatedAPIClient creates an API client targeting api.BaseURL()
// (the data API origin) carrying a token valid for that audience, minted by
// exchanging the matching login context's JWT at its own core (see
// auth.ResolveDataAPIToken).
//
// Pass insecureHTTP=true to allow plain HTTP base URLs for local
// development. Only the data origin is checked here — the bearer travels
// there on resource requests; the exchange leg is guarded by the
// per-context token manager (https required outside loopback/opt-in).
func NewAuthenticatedAPIClient(ctx context.Context, insecureHTTP bool) (*api.Client, error) {
	dataURL := api.BaseURL()
	if insecureHTTP {
		auth.EnableInsecureHTTP()
	} else if err := api.RequireSecureURL(dataURL); err != nil {
		return nil, fmt.Errorf("base URL check: %w", err)
	}

	// ResolveDataAPIToken discovers which login context the data host trusts
	// (via its /.well-known/entire-api.json) and exchanges that context's
	// token for the advertised audience. It normalises dataURL to an origin
	// internally.
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
// entire-api routes (e.g. experts). It exchanges the login JWT for a
// jurisdictional identity token and dials the entire-api cell directly, because
// the BFF does not proxy these routes for bearer callers (COR-666).
//
// fullName (owner/repo) and/or ulid identify the repo whose cell to reach. When
// either is supplied, the repo's OWNING cell + jurisdiction are resolved from
// the control plane (mirroring the BFF's per-repo cell selection) so the call
// lands in the region that hosts the repo. Resolution is best-effort: any
// failure yields a nil target and NewEntireAPICellClient falls back to
// home-jurisdiction routing, so the common same-region case never regresses.
func NewAuthenticatedEntireAPICellClient(ctx context.Context, insecureHTTP bool, fullName, ulid string) (*api.Client, error) {
	target := resolveExpertsCellTarget(ctx, fullName, ulid)
	// NewEntireAPICellClient already returns user-facing, context-rich errors
	// (login hint, discovery-unavailable, region guidance); re-wrapping here
	// would bury them, so surface them verbatim.
	return auth.NewEntireAPICellClient(ctx, insecureHTTP, target) //nolint:wrapcheck // pass through contextual auth errors
}
