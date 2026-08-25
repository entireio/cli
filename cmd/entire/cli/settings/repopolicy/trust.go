package repopolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// ResolveTrustIdentity derives an exclusive origin or path identity. A
// configured but unparseable origin is an error; path identity is allowed only
// when origin is absent.
func ResolveTrustIdentity(ctx context.Context, repository Repository) (TrustIdentity, error) {
	urls, fetchFound, err := gitremote.GetRemoteURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("reading origin remote: %w", err)
	}
	pushURLs, pushFound, err := gitremote.GetRemotePushURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("reading origin pushurl: %w", err)
	}
	urls = append(urls, pushURLs...)
	if fetchFound || pushFound {
		keys := make([]string, 0, len(urls))
		for _, raw := range urls {
			key := NormalizeOrigin(raw)
			if key == "" {
				return TrustIdentity{}, errors.New("configured origin cannot be normalized")
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			return TrustIdentity{}, errors.New("configured origin has no usable URLs")
		}
		return TrustIdentity{OriginKeys: keys}, nil
	}
	return TrustIdentity{Path: repository.WorktreeRoot}, nil
}

// DecideEgress computes the checkpoint-egress decision.
//
// While the global tier is off or unconfigured nothing changes from main: a
// repo-enabled repository syncs. Once the user turns global tracking on,
// user-level agent hooks fire in every repository on the machine, so consent
// must be machine-local: EVERY active repo — repo-enabled or globally
// tracked — needs trust_all, a trusted origin, or a trusted path in the
// user settings file. `entire enable` records that trust itself and the
// pre-push prompt records it interactively, so the only repos that ever see
// a hold are ones the user never explicitly enabled or trusted.
func DecideEgress(ctx context.Context, policy RepoPolicy, global *GlobalConfig, repository Repository) TrustDecision {
	if !policy.Active {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInactive}
	}
	if global == nil || !global.Enabled {
		return TrustDecision{Allowed: true, Source: TrustSourceLocal}
	}
	identity, err := ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInvalidOrigin}
	}
	decision := TrustDecision{Source: TrustSourceNone, Reason: TrustReasonUntrusted, Identity: identity}
	if global.TrustAll {
		decision.Allowed, decision.Source, decision.Reason = true, TrustSourceAll, TrustReasonNone
		return decision
	}
	if identityTrusted(ctx, global, identity) {
		decision.Allowed, decision.Source, decision.Reason = true, TrustSourceRepo, TrustReasonNone
	}
	return decision
}

func identityTrusted(ctx context.Context, global *GlobalConfig, identity TrustIdentity) bool {
	if identity.OriginKeyed() {
		for _, key := range identity.OriginKeys {
			if !containsOrigin(global.TrustedOrigins, key) {
				return false
			}
		}
		return true
	}
	matched, err := MatchesExcludePathExact(ctx, global.TrustedPaths, identity.Path)
	return err == nil && matched
}

func containsOrigin(entries []string, key string) bool {
	for _, entry := range entries {
		if CanonicalTrustOrigin(entry) == key {
			return true
		}
	}
	return false
}

// CanonicalTrustOrigin normalizes a hand-edited exact trust key.
func CanonicalTrustOrigin(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
