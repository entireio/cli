package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

var errUnsupportedCheckpointPolicy = errors.New("checkpoint policy cannot be satisfied by this Entire CLI")
var errUnreadableCheckpointPolicy = errors.New("checkpoint policy could not be read")

func checkpointVersionForNewCheckpoint(ctx context.Context, repo *git.Repository) (string, error) {
	policy, err := checkpointPolicyForCheckpointData(ctx, repo)
	if err != nil {
		return "", err
	}
	if !checkpointpolicy.CanSatisfyPolicy(policy) {
		return "", unsupportedCheckpointPolicyError(policy)
	}
	return checkpointpolicy.Normalize(policy).CheckpointVersion, nil
}

func ensureCheckpointPolicyAllowsCheckpointData(ctx context.Context, repo *git.Repository) error {
	policy, err := checkpointPolicyForCheckpointData(ctx, repo)
	if err != nil {
		return err
	}
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return nil
	}
	return unsupportedCheckpointPolicyError(policy)
}

func checkpointPolicyForCheckpointData(ctx context.Context, repo *git.Repository) (checkpointpolicy.Policy, error) {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		return checkpointpolicy.Policy{}, unreadableCheckpointPolicyError(err)
	}
	return state.Policy, nil
}

func unsupportedCheckpointPolicyError(policy checkpointpolicy.Policy) error {
	message := strings.TrimSpace(checkpointpolicy.UnsupportedPolicyMessage(
		policy,
		versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version),
	))
	return fmt.Errorf("%w:\n%s", errUnsupportedCheckpointPolicy, message)
}

func unreadableCheckpointPolicyError(err error) error {
	return fmt.Errorf("%w: %w", errUnreadableCheckpointPolicy, err)
}
