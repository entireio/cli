package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ValidateGlobalConfig checks an enabled global tier's exclude patterns for
// problems the fail-closed rule turns into silent deactivation: an unusable
// exclude_paths or exclude_origins pattern (relative, unsupported ~user form,
// invalid glob) deactivates the tier in every repo it is checked against.
// It returns one problem string per offending pattern, naming the pattern
// index, in the same "exclude_paths[i]: reason" shape the matchers use.
//
// A (nil, nil) result means the tier is unconfigured, disabled, or clean. The
// error reports an unreadable or malformed settings file — the failure hook
// Debug logs can never surface, because on the hook paths logging.Init runs
// only after the gate that reads this file has already failed. Doctor is the
// diagnostic surface for both.
func ValidateGlobalConfig(ctx context.Context) ([]string, error) {
	us, err := LoadUserSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !us.GlobalEnabled() {
		return nil, nil
	}
	var problems []string
	for i, p := range us.Global.ExcludePaths {
		// checkExcludePathPattern is the same per-pattern check the
		// fail-closed matcher applies, so doctor and the gate agree.
		if _, err := checkExcludePathPattern(p); err != nil {
			problems = append(problems, fmt.Sprintf("exclude_paths[%d]: %v", i, err))
		}
	}
	for i, p := range us.Global.ExcludeOrigins {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !doublestar.ValidatePattern(strings.ToLower(p)) {
			problems = append(problems, fmt.Sprintf("exclude_origins[%d]: invalid glob", i))
		}
	}
	return problems, nil
}

// UnnormalizableOrigins returns the current worktree's origin URLs that are
// present but cannot be normalized to the host/owner/repo form exclude_origins
// patterns are written against (bare filesystem paths, file:// URLs). Under
// the fail-closed rule each one deactivates the global tier for this repo:
// exclusion could not be checked. Only meaningful when exclude_origins is
// configured on an enabled tier; every other condition (tier off, no repo,
// no origin remote, lookup failure) returns nil, best-effort.
func UnnormalizableOrigins(ctx context.Context) []string {
	us, err := LoadUserSettings(ctx)
	if err != nil || !us.GlobalEnabled() || len(us.Global.ExcludeOrigins) == 0 {
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
		if normalizeOrigin(origin) == "" {
			bad = append(bad, origin)
		}
	}
	return bad
}
