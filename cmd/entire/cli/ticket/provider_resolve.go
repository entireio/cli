package ticket

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// activeProvider builds the Provider for the repository's configured platform,
// loading its stored credential. Returns an error when nothing is configured or
// the credential is missing.
func activeProvider(ctx context.Context) (Provider, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	tc := s.TicketConfig()
	if tc.IsZero() {
		return nil, errors.New("no ticket platform configured — run `entire ticket setup` first")
	}
	platform, err := ParsePlatform(tc.Platform)
	if err != nil {
		return nil, err
	}
	token, err := LoadToken(platform)
	if err != nil {
		return nil, err
	}
	return buildProvider(platform, token, tc.Team)
}

// buildProvider constructs the Provider implementation for a platform.
func buildProvider(platform Platform, token, team string) (Provider, error) {
	switch platform {
	case PlatformLinear:
		return newLinearProvider(token, team), nil
	default:
		return nil, fmt.Errorf("no provider implemented for platform %q", platform)
	}
}

// canonicalID best-effort normalizes a user-supplied ticket identifier (a full
// URL or a differently-cased id) into the configured provider's canonical form
// (e.g. "MOH-57"). Normalization is pure string parsing, so it works without a
// stored credential; it returns id unchanged when no platform is configured or
// the id cannot be parsed, keeping link/start usable offline.
func canonicalID(ctx context.Context, id string) string {
	s, err := settings.Load(ctx)
	if err != nil {
		return id
	}
	tc := s.TicketConfig()
	if tc.IsZero() {
		return id
	}
	platform, err := ParsePlatform(tc.Platform)
	if err != nil {
		return id
	}
	prov, err := buildProvider(platform, "", tc.Team)
	if err != nil {
		return id
	}
	if canonical, ok := prov.CanonicalID(id); ok {
		return canonical
	}
	return id
}
