package cli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// trailsEnabledForRepo reports the locally cached server-side trails
// enablement for this repo. It intentionally performs no git subprocesses, auth
// work, or network I/O: this runs on the agent turn-start path.
func trailsEnabledForRepo(ctx context.Context) bool {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil || prefs.TrailsEnabled == nil {
		return false
	}
	return *prefs.TrailsEnabled
}

// saveTrailsEnabledForRepo persists the server-side trails enablement cache in
// clone-local preferences (.git/entire/preferences.json). The value is not
// committed and is shared by linked worktrees of the same clone.
func saveTrailsEnabledForRepo(ctx context.Context, enabled bool) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load clone preferences: %w", err)
	}
	prefs.TrailsEnabled = &enabled
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save clone preferences: %w", err)
	}
	return nil
}
