package settings

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// ValidateGlobalConfig is the compatibility facade for leaf-owned validation.
func ValidateGlobalConfig(ctx context.Context) ([]string, error) {
	return repopolicy.ValidateGlobalConfig(ctx) //nolint:wrapcheck // compatibility facade preserves the public error contract
}

// ValidateGlobalPatterns validates an already-loaded global config.
func ValidateGlobalPatterns(config *GlobalConfig) []string {
	return repopolicy.ValidateGlobalPatterns(config)
}

// UnnormalizableOrigins returns present origin URLs that cannot be compared
// with global origin exclusions. Repository discovery remains a parent concern
// until all callers consume RepoPolicy snapshots.
func UnnormalizableOrigins(ctx context.Context, config *GlobalConfig) []string {
	if config == nil || len(config.ExcludeOrigins) == 0 {
		return nil
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}
	origins, found, err := gitremote.GetRemoteURLsInDirIfSet(ctx, root, "origin")
	if err != nil || !found {
		return nil
	}
	var bad []string
	for _, origin := range origins {
		if repopolicy.NormalizeOrigin(origin) == "" {
			bad = append(bad, origin)
		}
	}
	return bad
}
