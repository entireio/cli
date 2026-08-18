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

// TrustIdentity is the egress-consent key for one repository. Exactly one
// side is set: OriginKeys when the origin remote exists and EVERY configured
// URL normalizes — one key per URL, and consent requires all of them — or
// Path (the symlink-resolved worktree root) when there is no origin or ANY
// URL fails to normalize. The URL set covers both remote.origin.url and
// remote.origin.pushurl: git's pushurl-replaces-url rule means egress follows
// the pushurl when one is set, so consent must key what the data actually
// reaches — a pushurl's keys join the set and are required like any other. A
// single unnormalizable URL flips the whole identity to path rather than
// being dropped from the key set: an unkeyable URL is still an egress
// destination (a multi-URL push delivers refs to every URL), so partial
// origin keys would fail open. The split is exclusive, never a union: a repo
// keyed by its origin is not covered by a trusted path, and any identity flip
// re-asks (new egress destination, new consent). The zero value (returned
// alongside a non-nil error) has neither side set and must not be consulted.
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

// RepoTrustIdentity derives the trust identity of the current worktree. It is
// the single derivation shared by the egress gate, the trust command, status,
// and doctor — no caller may re-derive keys another way, or two surfaces
// could disagree about what consent covers. Errors (worktree unresolvable,
// origin config unreadable) are returned for callers to fail closed on.
func RepoTrustIdentity(ctx context.Context) (TrustIdentity, error) {
	id, _, err := currentTrustIdentity(ctx)
	return id, err
}

// currentTrustIdentity is RepoTrustIdentity plus the symlink-resolved
// worktree root, which the write helpers need even for origin-keyed repos
// (pruning stale trusted_paths entries for the root).
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
	// Push URLs join the set: git's pushurl-replaces-url rule means a push
	// delivers to remote.origin.pushurl when set, so an identity keyed only on
	// the fetch URLs would consent to a destination the data never reaches
	// while ignoring the one it does (see TrustIdentity).
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
// machine for the current repository. Repo-level setup (either settings file)
// is explicit consent and short-circuits true — which is also why any
// checkpoint_remote configuration is moot for this gate: it can only live in
// repo-level settings, so the gate has passed before it could matter.
// Otherwise the globally-enrolled repo must be trusted: trust_all, or its
// full identity listed — one decision tree, shared with CurrentTrustSource
// via globalTrustSource so the two can never drift. Every error path returns
// false — held checkpoints are recoverable, leaked ones are not.
func CheckpointEgressAllowed(ctx context.Context) bool {
	return IsSetUpAny(ctx) || globalTrustSource(ctx) != TrustSourceNone
}

// CurrentTrustSource names what grants egress consent for the current
// repository. It describes the global trust store only — repo-level setup is
// deliberately ignored (CheckpointEgressAllowed is the gate) — so revoke can
// detect that a key removal is masked by trust_all, and status can name the
// consent source.
func CurrentTrustSource(ctx context.Context) TrustSource {
	return globalTrustSource(ctx)
}

// RepoUntrustedEnrolled reports the held state: the current repo is enrolled
// by the global tier (active here, no repo-level setup) and checkpoint egress
// is not consented. It is the ONE predicate behind every untrusted-enrolled
// surface (session-start banner line, status trust state, doctor's hold
// note), so the three cannot drift into disagreeing about who is held.
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
// identity and returns that identity. Origin-keyed repos get ALL their URL
// keys written, and any trusted_paths entry for this root is pruned in the
// same write — a stale path entry would resurrect trust the moment the origin
// is removed, defeating a later revoke. Idempotent.
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
// tier was never configured. Exported as a sentinel so `entire trust` can
// swap it for a friendly message without string-matching.
var ErrGlobalModeUnconfigured = errors.New("global mode is not configured; nothing to trust")

// requireConfiguredGlobal is the single guard shared by every trust writer:
// materializing the global block on a machine whose global tier was never
// configured would flip GlobalConfigured() and silently suppress the ask-once
// global-enable question.
func requireConfiguredGlobal(us *UserSettings) error {
	if us.Global == nil {
		return ErrGlobalModeUnconfigured
	}
	return nil
}

// TrustAllRepos records machine-wide egress consent (trust_all): every
// globally enrolled repo on this machine — current and future — syncs
// checkpoints. Per-repo keys are left untouched, so disabling trust_all later
// restores the per-repo consent state.
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
// identity and returns that identity. It removes the identity's origin keys
// AND any trusted_paths entry for this root regardless of which side the
// identity is on — leaving the path entry behind would resurrect trust once
// the origin is removed. Other repos' keys, the exclude lists, and trust_all
// are never touched. Idempotent for a configured tier (revoking an untrusted
// repo is a no-op); an unconfigured tier returns ErrGlobalModeUnconfigured —
// there was never anything to revoke, and a silent success would read as a
// withdrawal that happened. Not retroactive (pushed data stays pushed).
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
	// Per-entry via pathEntryIsRoot (which reuses the exclude_paths_exact
	// matcher for its symlink/case bridging; exact-match-only with no subtree
	// cascade is precisely the trusted_paths contract). Deliberately NOT the
	// whole-list matcher: it aborts on the first unusable entry, so one
	// poisoned hand-edit would veto every valid grant in the list. An
	// unreadable entry cannot grant trust either way, so it is skipped and
	// the rest still count — still fail-closed, entry by entry.
	return anyPathEntryIsRoot(ctx, g.TrustedPaths, id.Path)
}

// canonicalOriginEntry is the one definition of hand-edited-entry tolerance
// for trusted_origins: whitespace-trimmed and case-folded (identity keys are
// produced lowercase) — but never glob-expanded: consent is exact. Both the
// gate's membership check and revoke's removal filter go through it, so an
// entry the gate honors is always one revoke can remove.
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

// pathEntryIsRoot reports whether one trusted_paths entry refers to root,
// with the same given/resolved/case bridging as the gate's matcher. An entry
// it cannot interpret reports false: pruning must not drop what it cannot
// read, and a failed membership probe merely appends root's canonical entry.
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
