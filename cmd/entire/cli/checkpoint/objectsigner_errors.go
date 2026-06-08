package checkpoint

import (
	"context"
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// ErrSigningDisabled is returned by SignCommit when checkpoint signing is
// disabled in settings.
var ErrSigningDisabled = errors.New("checkpoint signing disabled")

// ShouldSkipPushSigning reports whether the push-time signing loop should
// be bypassed entirely. Returns true only when checkpoint signing is
// disabled in settings.
func ShouldSkipPushSigning(ctx context.Context) bool {
	return !settings.IsSignCheckpointCommitsEnabled(ctx)
}
