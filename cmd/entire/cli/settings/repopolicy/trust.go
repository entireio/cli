package repopolicy

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// ResolveTrustIdentity derives an exclusive origin or path identity: origin
// keys when every configured origin URL normalizes, the worktree path when
// origin is absent or any URL cannot be normalized (see TrustIdentity). Only a
// failure to read the origin config is an error.
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
				// A bare local path, file://, or any other URL that does not
				// reduce to host/owner/repo. The whole identity flips to the
				// path key rather than dropping this URL from the key set: a
				// multi-URL push delivers to every URL, so partial origin keys
				// would fail open — while a hard error would leave the repo
				// with no way to be trusted at all.
				keys = nil
				break
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			return TrustIdentity{OriginKeys: keys}, nil
		}
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
	// trust_all is consent for every repo on the machine, whatever its remote
	// looks like — it must not depend on resolving an identity. The identity
	// is still attached when it resolves (status names the scope), but a
	// failure to read the origin config never turns "Always" into a hold.
	if global.TrustAll {
		identity, _ := ResolveTrustIdentity(ctx, repository) //nolint:errcheck // display-only under trust_all; see above
		return TrustDecision{Allowed: true, Source: TrustSourceAll, Reason: TrustReasonNone, Identity: identity}
	}
	identity, err := ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInvalidOrigin}
	}
	decision := TrustDecision{Source: TrustSourceNone, Reason: TrustReasonUntrusted, Identity: identity}
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
