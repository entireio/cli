package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

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
	// No second scheme check: requireSecureDataOverride above already rejected an
	// http ENTIRE_API_BASE_URL, and every other value target.BaseURL can hold is
	// built by auth.dataBaseURLForCore as "https://" + site. Checking again after
	// the credential is in hand would be too late to matter anyway — the point of
	// the gate is to run before discovery and refresh dial the host.
	// TestNewAuthenticatedAPIClient_RejectsInsecureOverrideBeforeResolving pins it.
	return api.NewClientWithBaseURL(target.Token, target.BaseURL), nil
}

// insecureDataOverrideNote describes a rejected http ENTIRE_API_BASE_URL for a
// user-facing message, naming the host only when it can be shown safely.
//
// The value is never echoed raw. The variable can carry userinfo, and this text
// reaches stderr and CI logs, so it goes through api.OriginOnly, which rebuilds
// scheme+host and therefore drops credentials, path and query. u.Redacted() —
// what login.go and env_token.go use for the same hazard — is not enough here:
// it only masks the password, so a token pasted into the username slot
// ("http://tok@host") would survive.
//
// OriginOnly returns its input unchanged when there is no host to rebuild from,
// which "http://user:secret@" satisfies while still being an http URL that
// RequireSecureURL rejects. That case reports the variable without its value
// rather than leaking it.
func insecureDataOverrideNote() string {
	note := api.BaseURLEnvVar + " is set to an insecure http:// URL"
	raw := api.BaseURL()
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Scheme != "" && u.Host != "" {
		note += " (" + api.OriginOnly(raw) + ")"
	}
	return note
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
