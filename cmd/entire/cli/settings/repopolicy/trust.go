package repopolicy

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// ResolveTrustIdentity derives the exclusive consent identity for a
// repository: keys for the checkpoint sync remote (see ResolveSyncRemote) when
// EVERY configured URL of that remote normalizes to host/owner/repo, else the
// worktree path — when no remote is configured, or when any URL cannot be
// normalized (a bare local path, file://). The flip to path is whole, never a
// partial key set: a multi-URL remote delivers to every URL, so partial keys
// would fail open, while a hard error would leave the repo with no way to be
// trusted at all. Only a failure to read the remote configuration (or to
// elect the sync remote) is an error.
func ResolveTrustIdentity(ctx context.Context, repository Repository) (TrustIdentity, error) {
	sync, err := ResolveSyncRemote(ctx, repository)
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("resolving checkpoint sync remote: %w", err)
	}
	identity := TrustIdentity{RemoteName: sync.Name, Dedicated: sync.Dedicated}
	keys := make([]string, 0, len(sync.URLs))
	for _, raw := range sync.URLs {
		key := NormalizeOrigin(raw)
		if key == "" {
			keys = nil
			break
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	if len(keys) > 0 {
		identity.OriginKeys = keys
		return identity, nil
	}
	identity.Path = repository.WorktreeRoot
	identity.PathRemotes = append([]string(nil), sync.URLs...)
	return identity, nil
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
// Takes the whole UserSettings, not just its global block: trust now spans two
// top-level blocks — `global` holds the trusted origins and paths, and
// `path_trust_destinations` holds the destination each path consent was
// recorded for. See UserSettings.PathTrust.
func DecideEgress(ctx context.Context, policy RepoPolicy, us *UserSettings, repository Repository) TrustDecision {
	var global *GlobalConfig
	if us != nil {
		global = us.Global
	}
	if !policy.Active {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInactive}
	}
	if global == nil || !global.Enabled {
		return TrustDecision{Allowed: true, Source: TrustSourceLocal}
	}
	// trust_all is consent for every repo on the machine, whatever its remote
	// looks like — it must not depend on resolving an identity. The identity
	// is still attached when it resolves (status names the scope), but a
	// failure to read the remote config or elect the sync remote never turns
	// "Always" into a hold.
	if global.TrustAll {
		identity, _ := ResolveTrustIdentity(ctx, repository) //nolint:errcheck // display-only under trust_all; see above
		return TrustDecision{Allowed: true, Source: TrustSourceAll, Reason: TrustReasonNone, Identity: identity}
	}
	identity, err := ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonIdentityUnresolved}
	}
	decision := TrustDecision{Source: TrustSourceNone, Reason: TrustReasonUntrusted, Identity: identity}
	if identityTrusted(ctx, global, identity) && pathTrustCoversDestination(ctx, us.PathTrust, identity) {
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

// pathTrustCoversDestination reports whether a path-keyed consent still names
// the destination transcripts would go to.
//
// A path names a WORKTREE, not a destination, so on its own it cannot honor
// "new destination, new consent": two filesystem remotes reduce to the same
// path. Recorded destinations close that. An entry with none recorded predates
// the discriminator and is honored on the path alone, so closing the gap does
// not revoke consent anyone already gave.
func pathTrustCoversDestination(ctx context.Context, recorded map[string][]string, identity TrustIdentity) bool {
	// Find the entry the same way TrustedPaths is matched — symlink-aware, via
	// MatchesExcludePathExact — not by raw string compare. The recorded key and
	// the resolved worktree root can be different spellings of one directory
	// (/var vs /private/var on macOS), and a raw compare silently misses,
	// which reads as "no destination recorded" and waves the push through.
	var want []string
	for key, remotes := range recorded {
		matched, err := MatchesExcludePathExact(ctx, []string{key}, identity.Path)
		if err == nil && matched {
			want = remotes
			break
		}
	}
	if len(want) == 0 {
		return true // legacy entry: consented before the discriminator existed
	}
	if len(want) != len(identity.PathRemotes) {
		return false
	}
	have := make([]string, len(identity.PathRemotes))
	for i, u := range identity.PathRemotes {
		have[i] = filepath.ToSlash(u)
	}
	got := make([]string, len(want))
	for i, u := range want {
		got[i] = filepath.ToSlash(u)
	}
	slices.Sort(have)
	slices.Sort(got)
	return slices.Equal(have, got)
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
