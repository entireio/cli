package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// TrustIdentity is the egress-consent key for one repository. Exactly one side
// is set: OriginKeys when every configured origin URL (fetch and push)
// normalizes — consent requires all of them — or Path (the symlink-resolved
// worktree root) when there is no origin or ANY URL fails to normalize. The
// split is exclusive, never a union; an identity flip re-asks. The zero value
// (returned alongside a non-nil error) must not be consulted.
type TrustIdentity struct {
	OriginKeys []string
	Path       string
}

// OriginKeyed reports which side of the identity is set: true for an
// origin-keyed repo, false for a path-keyed one.
func (id TrustIdentity) OriginKeyed() bool {
	return len(id.OriginKeys) > 0
}

// DisplayScope is the identity's user-facing name: the first origin key for
// an origin-keyed repo, else the worktree root path.
func (id TrustIdentity) DisplayScope() string {
	if id.OriginKeyed() {
		return id.OriginKeys[0]
	}
	return id.Path
}

// TrustSource names what grants egress consent for a repository in the
// global trust store, for status/doctor/revoke messaging.
type TrustSource string

const (
	TrustSourceAll  TrustSource = "trust_all"
	TrustSourceRepo TrustSource = "repo"
	TrustSourceNone TrustSource = "none"
)

// RepoTrustIdentity derives the trust identity of the current worktree — the
// single derivation shared by the egress gate, the trust command, status, and
// doctor. Errors are returned for callers to fail closed on.
func RepoTrustIdentity(ctx context.Context) (TrustIdentity, error) {
	id, _, err := currentTrustIdentity(ctx)
	return id, err
}

// currentTrustIdentity is RepoTrustIdentity plus the symlink-resolved worktree
// root, which the write helpers need even for origin-keyed repos.
func currentTrustIdentity(ctx context.Context) (TrustIdentity, string, error) {
	root, err := worktreeRootFn(ctx)
	if err != nil {
		return TrustIdentity{}, "", fmt.Errorf("resolving worktree root: %w", err)
	}
	resolvedRoot := filepath.ToSlash(root)
	if resolved, symErr := filepath.EvalSymlinks(root); symErr == nil {
		resolvedRoot = filepath.ToSlash(resolved)
	} else {
		logging.Debug(ctx, "trust identity: worktree root symlink resolution failed; keying on the unresolved path",
			slog.String("error", symErr.Error()))
	}
	urls, urlsFound, err := gitremote.GetRemoteURLsInDirIfSet(ctx, root, "origin")
	if err != nil {
		return TrustIdentity{}, "", fmt.Errorf("reading origin remote: %w", err)
	}
	// Push URLs join the set: git's pushurl-replaces-url rule means egress
	// follows the pushurl when one is set.
	pushURLs, pushFound, err := gitremote.GetRemotePushURLsInDirIfSet(ctx, root, "origin")
	if err != nil {
		return TrustIdentity{}, "", fmt.Errorf("reading origin pushurl: %w", err)
	}
	urls = append(urls, pushURLs...)
	if urlsFound || pushFound {
		var keys []string
		for _, u := range urls {
			key := normalizeOrigin(u)
			if key == "" {
				keys = nil // any unkeyable URL flips the identity to path (see TrustIdentity)
				break
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			return TrustIdentity{OriginKeys: keys}, resolvedRoot, nil
		}
	}
	return TrustIdentity{Path: resolvedRoot}, resolvedRoot, nil
}

// CheckpointEgressAllowed reports whether checkpoint data may leave this
// machine for the current repository: repo-level setup is explicit consent
// (which also makes any checkpoint_remote configuration moot — it can only
// live in repo-level settings); otherwise the globally-enrolled repo must be
// trusted. Every error path returns false (fail closed).
func CheckpointEgressAllowed(ctx context.Context) bool {
	return IsSetUpAny(ctx) || globalTrustSource(ctx) != TrustSourceNone
}

// CurrentTrustSource names what grants egress consent in the global trust
// store only — repo-level setup is deliberately ignored — so revoke can detect
// a removal masked by trust_all and status can name the consent source.
func CurrentTrustSource(ctx context.Context) TrustSource {
	return globalTrustSource(ctx)
}

// RepoUntrustedEnrolled reports the held state: enrolled by the global tier,
// no repo-level setup, egress not consented. It is the ONE predicate behind
// every untrusted-enrolled surface (banner, status, doctor).
func RepoUntrustedEnrolled(ctx context.Context) bool {
	return !IsSetUpAny(ctx) && GlobalModeActive(ctx) && globalTrustSource(ctx) == TrustSourceNone
}

// globalTrustSource is the single trust decision tree behind both
// CheckpointEgressAllowed and CurrentTrustSource. Every error path resolves
// to TrustSourceNone (fail closed).
func globalTrustSource(ctx context.Context) TrustSource {
	us, err := LoadUserSettings(ctx)
	if err != nil || us.Global == nil {
		return TrustSourceNone
	}
	if us.Global.TrustAll {
		return TrustSourceAll
	}
	id, err := RepoTrustIdentity(ctx)
	if err != nil {
		return TrustSourceNone
	}
	if identityTrusted(ctx, us.Global, id) {
		return TrustSourceRepo
	}
	return TrustSourceNone
}

// TrustCurrentRepo records egress consent for the current repository's
// identity and returns it. Origin-keyed repos get ALL their URL keys written;
// any trusted_paths entry for this root is pruned in the same write, or it
// would resurrect trust once the origin is removed. Idempotent.
func TrustCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	id, root, err := currentTrustIdentity(ctx)
	if err != nil {
		return TrustIdentity{}, err
	}
	err = ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		g := us.Global
		if len(id.OriginKeys) > 0 {
			for _, key := range id.OriginKeys {
				if !containsTrustedOrigin(g.TrustedOrigins, key) {
					g.TrustedOrigins = append(g.TrustedOrigins, key)
				}
			}
			g.TrustedPaths = withoutPathEntriesFor(ctx, g.TrustedPaths, root)
			return nil
		}
		if !anyPathEntryIsRoot(ctx, g.TrustedPaths, root) {
			g.TrustedPaths = append(g.TrustedPaths, root)
		}
		return nil
	})
	if err != nil {
		return TrustIdentity{}, err
	}
	return id, nil
}

// ErrGlobalModeUnconfigured is returned by the trust writers when the global
// tier was never configured, as a sentinel so `entire trust` can swap it for
// a friendly message.
var ErrGlobalModeUnconfigured = errors.New("global mode is not configured; nothing to trust")

// requireConfiguredGlobal guards every trust writer: materializing the global
// block would flip GlobalConfigured() and suppress the ask-once question.
func requireConfiguredGlobal(us *UserSettings) error {
	if us.Global == nil {
		return ErrGlobalModeUnconfigured
	}
	return nil
}

// TrustAllRepos records machine-wide egress consent (trust_all). Per-repo keys
// are left untouched, so disabling trust_all later restores the per-repo state.
func TrustAllRepos(ctx context.Context) error {
	return ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		us.Global.TrustAll = true
		return nil
	})
}

// RevokeCurrentRepo withdraws egress consent for the current repository's
// identity and returns it. It removes the identity's origin keys AND any
// trusted_paths entry for this root regardless of identity side (a leftover
// path entry would resurrect trust once the origin is removed); other repos'
// keys, the exclude lists, and trust_all are never touched. Idempotent for a
// configured tier; an unconfigured tier returns ErrGlobalModeUnconfigured.
// Not retroactive.
func RevokeCurrentRepo(ctx context.Context) (TrustIdentity, error) {
	id, root, err := currentTrustIdentity(ctx)
	if err != nil {
		return TrustIdentity{}, err
	}
	err = ModifyUserSettings(ctx, func(us *UserSettings) error {
		if err := requireConfiguredGlobal(us); err != nil {
			return err
		}
		g := us.Global
		if len(id.OriginKeys) > 0 {
			var kept []string
			for _, e := range g.TrustedOrigins {
				if !slices.Contains(id.OriginKeys, canonicalOriginEntry(e)) {
					kept = append(kept, e)
				}
			}
			g.TrustedOrigins = kept
		}
		g.TrustedPaths = withoutPathEntriesFor(ctx, g.TrustedPaths, root)
		return nil
	})
	if err != nil {
		return TrustIdentity{}, err
	}
	return id, nil
}

// identityTrusted checks id against the trust lists, honoring the exclusivity
// documented on TrustIdentity: origin-keyed repos consult ONLY
// trusted_origins (every key required), path-keyed repos ONLY trusted_paths.
func identityTrusted(ctx context.Context, g *GlobalConfig, id TrustIdentity) bool {
	if len(id.OriginKeys) > 0 {
		for _, key := range id.OriginKeys {
			if !containsTrustedOrigin(g.TrustedOrigins, key) {
				return false
			}
		}
		return true
	}
	// Per-entry so one unreadable hand-edit can't veto every valid grant; an
	// unreadable entry cannot grant trust either way (still fail-closed).
	return anyPathEntryIsRoot(ctx, g.TrustedPaths, id.Path)
}

// canonicalOriginEntry defines hand-edited-entry tolerance for
// trusted_origins: trimmed and case-folded, never glob-expanded. Both the
// gate's membership check and revoke's removal filter go through it.
func canonicalOriginEntry(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

func containsTrustedOrigin(entries []string, key string) bool {
	for _, e := range entries {
		if canonicalOriginEntry(e) == key {
			return true
		}
	}
	return false
}

// pathEntryIsRoot reports whether one trusted_paths entry refers to root, with
// the same given/resolved/case bridging as the gate's matcher. An entry it
// cannot interpret reports false.
func pathEntryIsRoot(ctx context.Context, entry, root string) bool {
	ok, err := matchesExcludePathExact(ctx, []string{entry}, root)
	return err == nil && ok
}

func anyPathEntryIsRoot(ctx context.Context, entries []string, root string) bool {
	for _, e := range entries {
		if pathEntryIsRoot(ctx, e, root) {
			return true
		}
	}
	return false
}

func withoutPathEntriesFor(ctx context.Context, entries []string, root string) []string {
	var kept []string
	for _, e := range entries {
		if !pathEntryIsRoot(ctx, e, root) {
			kept = append(kept, e)
		}
	}
	return kept
}
