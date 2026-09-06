package settings

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

type TrustIdentity = repopolicy.TrustIdentity
type TrustSource = repopolicy.TrustSource

const (
	TrustSourceNone  = repopolicy.TrustSourceNone
	TrustSourceLocal = repopolicy.TrustSourceLocal
	TrustSourceAll   = repopolicy.TrustSourceAll
	TrustSourceRepo  = repopolicy.TrustSourceRepo
)

// RepoTrustIdentity derives the current repository's egress-consent identity:
// the normalized keys of its elected checkpoint sync remote, or its canonical
// path when that remote is absent or has a URL that cannot be normalized.
func RepoTrustIdentity(ctx context.Context) (TrustIdentity, error) {
	repository, err := repopolicy.ResolveRepository(ctx)
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("resolving repository: %w", err)
	}
	identity, err := repopolicy.ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("resolving trust identity: %w", err)
	}
	return identity, nil
}

// currentPolicy reuses a hook-boundary snapshot when one is attached to ctx
// and classifies fresh otherwise — foreground commands that mutate
// activation or trust (enable, disable, trust) rely on the fresh read.
func currentPolicy(ctx context.Context) (repopolicy.RepoPolicy, error) {
	if policy, ok := repopolicy.RepoPolicyFromContext(ctx); ok {
		return policy, nil
	}
	policy, err := repopolicy.ClassifyRepoPolicy(ctx)
	if err != nil {
		return policy, fmt.Errorf("classifying repository policy: %w", err)
	}
	return policy, nil
}

// CheckpointEgressAllowed is the single predicate for whether checkpoint
// data may leave this machine (see repopolicy.DecideEgress).
func CheckpointEgressAllowed(ctx context.Context) bool {
	policy, err := currentPolicy(ctx)
	return err == nil && policy.Active && policy.Trust.Allowed
}

func CurrentTrustSource(ctx context.Context) TrustSource {
	policy, err := currentPolicy(ctx)
	if err != nil {
		return TrustSourceNone
	}
	return policy.Trust.Source
}

// GloballyTrackedUnderTrustAll reports a repo the user-global tier captures
// (no repo-level setup of its own) whose checkpoints will sync because
// trust_all is on — the one consented state the user never chose for THIS
// repo, which is why the SessionStart banner names it.
func GloballyTrackedUnderTrustAll(ctx context.Context) bool {
	policy, err := currentPolicy(ctx)
	return err == nil && policy.Active && policy.ActivationSource == repopolicy.ActivationGlobal &&
		policy.Trust.Allowed && policy.Trust.Source == repopolicy.TrustSourceAll
}

// CheckpointEgressHeld reports an active repo whose checkpoints are held
// locally pending trust.
func CheckpointEgressHeld(ctx context.Context) bool {
	policy, err := currentPolicy(ctx)
	return err == nil && policy.Active && !policy.Trust.Allowed
}

// TrustCurrentRepo records trust for the current repository identity.
func TrustCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	identity, err := RepoTrustIdentity(ctx)
	if err != nil {
		return TrustIdentity{}, err
	}
	err = ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		if identity.OriginKeyed() {
			for _, key := range identity.OriginKeys {
				if !containsTrustedOrigin(us.Global.TrustedOrigins, key) {
					us.Global.TrustedOrigins = append(us.Global.TrustedOrigins, key)
				}
			}
			return nil
		}
		if !anyPathEntryIsRoot(ctx, us.Global.TrustedPaths, identity.Path) {
			us.Global.TrustedPaths = append(us.Global.TrustedPaths, identity.Path)
		}
		return nil
	})
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("updating repository trust: %w", err)
	}
	return identity, nil
}

var ErrGlobalModeUnconfigured = errors.New("global mode is not configured; nothing to trust")

func requireConfiguredGlobal(us *UserSettings) error {
	if us.Global == nil {
		return ErrGlobalModeUnconfigured
	}
	return nil
}

func TrustAllRepos(ctx context.Context) error {
	return ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		us.Global.TrustAll = true
		return nil
	})
}

// RevokeCurrentRepo withdraws trust for the current repository. It removes
// both the repo's current sync-remote keys and its current path, so a repo
// trusted by folder that later gained a remote (or vice versa) is fully
// revoked. Entries for other repositories are never touched. A key from a
// PREVIOUS remote URL (or a previously elected remote) of this repo is not
// recognized and stays; the settings file is the audit trail for that.
func RevokeCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	repository, err := repopolicy.ResolveRepository(ctx)
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("resolving repository: %w", err)
	}
	identity, err := repopolicy.ResolveTrustIdentity(ctx, repository)
	if err != nil {
		// Same failure direction as TrustCurrentRepo: with no identity the
		// remote-keyed entries cannot be found, and a "✓ Revoked" over a no-op
		// would leave live trust the user believes is gone.
		return TrustIdentity{}, fmt.Errorf("resolving trust identity: %w", err)
	}
	err = ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		us.Global.TrustedOrigins = slices.DeleteFunc(us.Global.TrustedOrigins, func(entry string) bool {
			return slices.Contains(identity.OriginKeys, repopolicy.CanonicalTrustOrigin(entry))
		})
		us.Global.TrustedPaths = slices.DeleteFunc(us.Global.TrustedPaths, func(entry string) bool {
			return pathEntryIsRoot(ctx, entry, repository.WorktreeRoot)
		})
		return nil
	})
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("revoking repository trust: %w", err)
	}
	return identity, nil
}

func containsTrustedOrigin(entries []string, key string) bool {
	for _, entry := range entries {
		if repopolicy.CanonicalTrustOrigin(entry) == key {
			return true
		}
	}
	return false
}

func pathEntryIsRoot(ctx context.Context, entry, root string) bool {
	ok, err := repopolicy.MatchesExcludePathExact(ctx, []string{entry}, root)
	return err == nil && ok
}

func anyPathEntryIsRoot(ctx context.Context, entries []string, root string) bool {
	for _, entry := range entries {
		if pathEntryIsRoot(ctx, entry, root) {
			return true
		}
	}
	return false
}
