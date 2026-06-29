package cli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

func ensureCommittedCheckpointWritePolicy(ctx context.Context, repo *git.Repository) error {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		return fmt.Errorf("read checkpoint policy: %w", err)
	}
	if !checkpointpolicy.UnsupportedWrite(state.Policy) {
		return nil
	}
	return fmt.Errorf(
		"checkpoint policy requires checkpoint_version %q, which this Entire CLI cannot write; upgrade Entire and rerun the command: %s",
		state.Policy.CheckpointVersion,
		versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version),
	)
}
