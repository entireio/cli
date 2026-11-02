package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

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

// newTrailAPIClient dials the entire-api cell that owns the forge-qualified
// repository and returns its repo_id for repo-addressed trail reads. It is a
// package seam so tests can substitute a client pointed at a stub server.
var newTrailAPIClient = func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (*api.Client, string, error) {
	return openTrailRepoClient(ctx, forge, owner, repo, func() (cellClientBuilder, error) {
		return newCellClientBuilder(ctx, insecureHTTP)
	})
}

// trailRepoClientOpener is newTrailAPIClient for one operation that reads
// several repositories.
type trailRepoClientOpener func(ctx context.Context, forge, owner, repo string) (*api.Client, string, error)

// newTrailRepoClientOpener returns an opener that resolves the login once and
// shares one CellClientFactory across every repository it opens, as multi-cell
// operations must (docs/development/api-routing.md). It is safe for concurrent
// use. A package seam so tests can substitute stub servers.
var newTrailRepoClientOpener = func(insecureHTTP bool) trailRepoClientOpener {
	var (
		once    sync.Once
		factory cellClientBuilder
		err     error
	)
	return func(ctx context.Context, forge, owner, repo string) (*api.Client, string, error) {
		return openTrailRepoClient(ctx, forge, owner, repo, func() (cellClientBuilder, error) {
			once.Do(func() { factory, err = newCellClientBuilder(ctx, insecureHTTP) })
			return factory, err
		})
	}
}

func openTrailRepoClient(ctx context.Context, forge, owner, repo string, newFactory func() (cellClientBuilder, error)) (*api.Client, string, error) {
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil {
		return nil, "", err
	}
	factory, err := newFactory()
	var client *api.Client
	if err == nil {
		client, err = factory.ClientFor(ctx, placement.Target)
	}
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
