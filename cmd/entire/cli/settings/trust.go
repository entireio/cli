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

func RepoTrustIdentity(ctx context.Context) (TrustIdentity, error) {
	_, identity, err := resolveRepoTrustIdentity(ctx)
	return identity, err
}

func resolveRepoTrustIdentity(ctx context.Context) (repopolicy.Repository, TrustIdentity, error) {
	repository, err := repopolicy.ResolveRepository(ctx)
	if err != nil {
		return repopolicy.Repository{}, TrustIdentity{}, fmt.Errorf("resolving repository: %w", err)
	}
	identity, err := repopolicy.ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return repopolicy.Repository{}, TrustIdentity{}, fmt.Errorf("resolving trust identity: %w", err)
	}
	return repository, identity, nil
}

func currentPolicy(ctx context.Context) (repopolicy.RepoPolicy, error) {
	if policy, ok := repopolicy.RepoPolicyFromContext(ctx); ok {
		return policy, nil
	}
	policy, err := repopolicy.ClassifyRepoPolicy(ctx)
	if err != nil {
		return repopolicy.RepoPolicy{}, fmt.Errorf("classifying repository policy: %w", err)
	}
	return policy, nil
}

func CheckpointEgressAllowed(ctx context.Context) bool {
	policy, err := currentPolicy(ctx)
	if err != nil || !policy.Active || !policy.Trust.Allowed {
		return false
	}
	if policy.ActivationSource == repopolicy.ActivationGlobal {
		if _, err := LoadForRepoPolicy(ctx, policy); err != nil {
			return false
		}
	}
	return true
}

func CurrentTrustSource(ctx context.Context) TrustSource {
	policy, err := currentPolicy(ctx)
	if err != nil {
		return TrustSourceNone
	}
	return policy.Trust.Source
}

func RepoUntrustedEnrolled(ctx context.Context) bool {
	policy, err := currentPolicy(ctx)
	return err == nil && policy.Active && policy.ActivationSource == repopolicy.ActivationGlobal && !policy.Trust.Allowed
}

func TrustCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	repository, identity, err := resolveRepoTrustIdentity(ctx)
	if err != nil {
		return TrustIdentity{}, err
	}
	err = repopolicy.ModifyTrustGrantLedger(ctx, repository, func(ledger repopolicy.TrustGrantLedger) (repopolicy.TrustGrantLedger, error) {
		err := ModifyUserSettings(ctx, func(us *UserSettings) error {
			if err := requireConfiguredGlobal(us); err != nil {
				return err
			}
			removeLedgerOwnedTrust(ctx, us.Global, ledger)
			if identity.OriginKeyed() {
				for _, key := range identity.OriginKeys {
					if !containsTrustedOrigin(us.Global.TrustedOrigins, key) {
						us.Global.TrustedOrigins = append(us.Global.TrustedOrigins, key)
					}
					if !slices.Contains(ledger.OriginKeys, key) {
						ledger.OriginKeys = append(ledger.OriginKeys, key)
					}
				}
			} else {
				if !anyPathEntryIsRoot(ctx, us.Global.TrustedPaths, identity.Path) {
					us.Global.TrustedPaths = append(us.Global.TrustedPaths, identity.Path)
				}
				if !slices.Contains(ledger.Paths, identity.Path) {
					ledger.Paths = append(ledger.Paths, identity.Path)
				}
			}
			return nil
		})
		return ledger, err
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

func RevokeCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	repository, err := repopolicy.ResolveRepository(ctx)
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("resolving repository: %w", err)
	}
	identity, identityErr := repopolicy.ResolveTrustIdentity(ctx, repository)
	if identityErr != nil {
		identity = TrustIdentity{Path: repository.WorktreeRoot}
	}
	err = repopolicy.ModifyTrustGrantLedger(ctx, repository, func(ledger repopolicy.TrustGrantLedger) (repopolicy.TrustGrantLedger, error) {
		err := ModifyUserSettings(ctx, func(us *UserSettings) error {
			if err := requireConfiguredGlobal(us); err != nil {
				return err
			}
			removeLedgerOwnedTrust(ctx, us.Global, ledger)
			ledger.OriginKeys = nil
			ledger.Paths = nil
			return nil
		})
		return ledger, err
	})
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("revoking repository trust: %w", err)
	}
	return identity, nil
}

func removeLedgerOwnedTrust(ctx context.Context, global *GlobalConfig, ledger repopolicy.TrustGrantLedger) {
	origins := global.TrustedOrigins[:0]
	for _, entry := range global.TrustedOrigins {
		if !slices.Contains(ledger.OriginKeys, repopolicy.CanonicalTrustOrigin(entry)) {
			origins = append(origins, entry)
		}
	}
	global.TrustedOrigins = origins
	paths := global.TrustedPaths[:0]
	for _, entry := range global.TrustedPaths {
		owned := false
		for _, recorded := range ledger.Paths {
			if pathEntryIsRoot(ctx, entry, recorded) {
				owned = true
				break
			}
		}
		if !owned {
			paths = append(paths, entry)
		}
	}
	global.TrustedPaths = paths
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
