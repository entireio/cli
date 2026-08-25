package cli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// prepareHookPolicy classifies once at a hook boundary and establishes the
// sticky runtime route only for active repositories. Every downstream policy,
// settings, path, trust, and checkpoint-backend read consumes this snapshot.
func prepareHookPolicy(ctx context.Context) (context.Context, repopolicy.RepoPolicy, error) {
	if policy, ok := repopolicy.RepoPolicyFromContext(ctx); ok {
		return ctx, policy, nil
	}
	policy, err := repopolicy.ClassifyRepoPolicy(ctx)
	if err != nil {
		repository, repoErr := repopolicy.ResolveRepository(ctx)
		if repoErr == nil {
			rebound, rebindErr := repopolicy.RebindMovedRepository(ctx, repository)
			if rebindErr != nil {
				return ctx, policy, fmt.Errorf("recovering moved repository policy: %w", rebindErr)
			}
			if rebound {
				policy, err = repopolicy.ClassifyRepoPolicy(ctx)
			}
		}
		if err != nil {
			return ctx, policy, fmt.Errorf("classifying repository policy: %w", err)
		}
	}
	if policy.Active {
		policy, err = repopolicy.EnsureRuntimeRoute(ctx, policy)
		if err != nil {
			return ctx, policy, fmt.Errorf("establishing runtime route: %w", err)
		}
	}
	return repopolicy.WithRepoPolicy(ctx, policy), policy, nil
}
