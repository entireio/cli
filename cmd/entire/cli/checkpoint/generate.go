package checkpoint

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// GenerateCheckpointID mints a new checkpoint ID in the format the configured
// primary store uses: a ULID under the git-refs store, a legacy 12-hex ID
// otherwise. It is the single place the backend-coupled ID format is decided —
// generation sites call it instead of id.Generate() so a git-refs checkpoint is
// always a ULID, which lets reads route by ID kind (ULID ⟹ ref).
//
// Fail-soft: a missing or malformed checkpoints config resolves to the default
// hex format rather than blocking ID generation (a bad block already surfaces
// through checkpoint.Open).
func GenerateCheckpointID(ctx context.Context) (id.CheckpointID, error) {
	// A malformed/missing config resolves to a nil cfg here; PrimaryIsRefs(nil)
	// is false, so we fall through to the default hex format (fail-soft).
	if cfg, err := settings.LoadCheckpointsConfig(ctx); err == nil && PrimaryIsRefs(cfg) {
		return id.GenerateULID() //nolint:wrapcheck // dispatcher; id.GenerateULID already returns a descriptive error
	}
	return id.Generate() //nolint:wrapcheck // dispatcher; id.Generate already returns a descriptive error
}
