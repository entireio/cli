package ticket

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// activeProvider builds the Provider for the repository's configured platform,
// loading its stored credential. Returns an error when nothing is configured,
// the credential is missing, or no concrete provider is implemented yet for the
// platform. Concrete providers are wired in by their own files (see linear.go).
//
//nolint:unparam // result is always nil until a concrete provider lands (see linear.go); wired now so link/status can resolve it.
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
	if _, err := LoadToken(platform); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no provider implemented for platform %q", platform)
}
