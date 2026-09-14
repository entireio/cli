package cli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// prepareHookPolicy classifies once at a hook boundary. Every downstream
// settings, path, trust, and checkpoint-backend read consumes this snapshot.
// A repo-enabled repository whose settings the full loader rejects
// (ErrScannerConfig, unknown keys) is reported inactive with an error, the
// same fail-closed answer main's IsSetUpAndEnabled gate gave — repopolicy
// cannot run that validation itself because it must not import settings.
func prepareHookPolicy(ctx context.Context) (context.Context, repopolicy.RepoPolicy, error) {
	if policy, ok := repopolicy.RepoPolicyFromContext(ctx); ok {
		return ctx, policy, nil
	}
	policy, err := repopolicy.ClassifyRepoPolicy(ctx)
	if err != nil {
		return ctx, policy, fmt.Errorf("classifying repository policy: %w", err)
	}
	if policy.ActivationSource == repopolicy.ActivationLocal {
		if _, loadErr := settings.Load(ctx); loadErr != nil {
			policy.Active = false
			policy.ActivationSource = repopolicy.ActivationInactive
			policy.InactiveReason = repopolicy.InactiveReasonRepoDisabled
			return ctx, policy, fmt.Errorf("repository settings invalid; skipping hook: %w", loadErr)
		}
	}
	return repopolicy.WithRepoPolicy(ctx, policy), policy, nil
}
