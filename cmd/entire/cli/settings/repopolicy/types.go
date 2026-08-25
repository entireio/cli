// Package repopolicy owns the dependency-light inputs and decisions used to
// classify repository tracking policy.
package repopolicy

// ActivationSource identifies the authority that activated tracking.
type ActivationSource string

const (
	ActivationInactive ActivationSource = "inactive"
	ActivationLocal    ActivationSource = "local"
	ActivationGlobal   ActivationSource = "global"
)

// RuntimeLayout identifies where mutable runtime data belongs.
type RuntimeLayout string

const (
	RuntimeUnknown   RuntimeLayout = "unknown"
	RuntimeWorktree  RuntimeLayout = "worktree"
	RuntimeGitCommon RuntimeLayout = "git_common"
)

// RuntimeRoute is the selected or proposed runtime-data location.
type RuntimeRoute struct {
	Version            int           `json:"version"`
	Layout             RuntimeLayout `json:"layout"`
	CanonicalWorktree  string        `json:"canonical_worktree"`
	CanonicalGitCommon string        `json:"canonical_git_common"`
}

// SetupRecord tracks independently repairable lazy-setup components for one
// worktree. Identity fields prevent a copied registry record from suppressing
// setup in another worktree.
type SetupRecord struct {
	Version            int    `json:"version"`
	GitHooksSpec       int    `json:"git_hooks_spec,omitempty"`
	PrimaryRefSpec     int    `json:"primary_ref_spec,omitempty"`
	CanonicalWorktree  string `json:"canonical_worktree"`
	CanonicalGitCommon string `json:"canonical_git_common"`
}

// TrustSource names the authority behind an egress decision.
type TrustSource string

const (
	TrustSourceNone  TrustSource = "none"
	TrustSourceLocal TrustSource = "local_activation"
	TrustSourceAll   TrustSource = "trust_all"
	TrustSourceRepo  TrustSource = "repo"
)

// TrustReason explains a held egress decision.
type TrustReason string

const (
	TrustReasonNone          TrustReason = ""
	TrustReasonInactive      TrustReason = "inactive"
	TrustReasonUntrusted     TrustReason = "untrusted"
	TrustReasonInvalidOrigin TrustReason = "invalid_origin"
	TrustReasonSettings      TrustReason = "settings_error"
)

// TrustIdentity is the exclusive origin-or-path key used for egress consent.
type TrustIdentity struct {
	OriginKeys []string `json:"origin_keys,omitempty"`
	Path       string   `json:"path,omitempty"`
}

// OriginKeyed reports whether this identity is remote-origin based.
func (id TrustIdentity) OriginKeyed() bool { return len(id.OriginKeys) > 0 }

// DisplayScope returns the user-facing identity scope.
func (id TrustIdentity) DisplayScope() string {
	if id.OriginKeyed() {
		return id.OriginKeys[0]
	}
	return id.Path
}

// TrustDecision is the repository's checkpoint-egress decision.
type TrustDecision struct {
	Allowed  bool
	Source   TrustSource
	Reason   TrustReason
	Identity TrustIdentity
}

// InactiveReason explains why repository tracking is inactive.
type InactiveReason int

const (
	InactiveReasonNone InactiveReason = iota
	InactiveReasonRepoDisabled
	InactiveReasonGlobalExcluded
	InactiveReasonGlobalOff
)

// RepoPolicy is one immutable repository-policy classification.
type RepoPolicy struct {
	Active           bool
	ActivationSource ActivationSource
	InactiveReason   InactiveReason
	WorktreeRoot     string
	GitCommonDir     string
	WorktreeKey      string
	Route            RuntimeRoute
	Trust            TrustDecision
}

// GlobalConfig is the "global" section of the user-global settings file.
// It controls global auto-enable: tracking agent sessions in repositories
// that have no repo-level Entire setup. Its JSON contract is intentionally
// stable because users edit this file directly.
type GlobalConfig struct {
	// Enabled turns global mode on. Both true and false count as configured.
	Enabled bool `json:"enabled"`

	// ExcludePaths are ~-expanded doublestar globs matched against a
	// repository's worktree root. Any match excludes that worktree.
	// Matching is symlink-robust in both directions: the root is tried in
	// its given and symlink-resolved forms, and a pattern's literal prefix
	// is also tried symlink-resolved (so `~/code/**` excludes repos under a
	// `~/code` that is a symlink to another volume). An unusable pattern
	// (relative, unsupported ~user form, invalid glob) deactivates global
	// mode entirely — fail closed — rather than silently tracking a repo
	// the user meant to exclude. Note linked worktrees checked out OUTSIDE
	// an excluded path are not covered by path exclusion; exclude_origins
	// covers them, since all worktrees of a clone share git config.
	ExcludePaths []string `json:"exclude_paths,omitempty"`

	// ExcludePathsExact are plain paths (NOT globs — meta characters have no
	// special meaning) excluded exactly: an entry matches only when it IS the
	// worktree root, with no subtree cascade. This closes an expressiveness
	// gap: a bare exclude_paths directory pattern always excludes its whole
	// subtree (p or p/**), so it cannot exclude exactly $HOME — the
	// home-as-dotfiles-repo case — without also excluding every repo beneath
	// it, and such no-origin repos cannot be caught by exclude_origins
	// either. Entries are whitespace-trimmed (blank entries skipped),
	// ~-expanded, cleaned, case-folded per platform, and matched
	// symlink-robustly like exclude_paths; an unusable entry (relative,
	// unsupported ~user form) fails closed the same way.
	ExcludePathsExact []string `json:"exclude_paths_exact,omitempty"`

	// ExcludeOrigins are doublestar globs matched against the origin remote
	// URL normalized to host/owner/repo. A repo without an origin matches
	// no origin pattern, but a repo whose origin is PRESENT and cannot be
	// normalized (bare filesystem path, file:// URL) deactivates global
	// mode — exclusion could not be checked, so fail closed. Hosts with
	// subgroups (GitLab) normalize to host/owner/sub.../repo: `*` does not
	// cross `/`, so use `gitlab.com/acme/**` to cover a whole namespace.
	// Origins stored via git insteadOf shorthands (e.g. gh:acme/widgets)
	// normalize to the shorthand form, not the expanded host — patterns
	// match what git config stores (see GetRemoteURLsInDirIfSet, which
	// reads raw config precisely to preserve this contract).
	ExcludeOrigins []string `json:"exclude_origins,omitempty"`

	// TrustAll grants checkpoint-egress consent machine-wide; enrollment
	// stays with Enabled and the exclude lists. The trust fields live inside
	// this strict-decoded block deliberately: a pre-trust binary reading a
	// trust-bearing file fails LoadUserSettings and global mode dies
	// fail-closed rather than misreading recorded consent.
	TrustAll bool `json:"trust_all,omitempty"`

	// TrustedOrigins are exact normalized origin keys (host/owner/repo, as
	// RepoTrustIdentity derives them from fetch AND push URLs) — never globs.
	// A multi-URL origin syncs only when EVERY configured URL's key is listed.
	TrustedOrigins []string `json:"trusted_origins,omitempty"`

	// TrustedPaths are exact symlink-resolved worktree roots — never globs,
	// no subtree cascade — for repos whose identity falls back to path. Each
	// linked worktree needs its own entry.
	TrustedPaths []string `json:"trusted_paths,omitempty"`
}

// UserSettings is the root of the user-global settings file. A nil Global
// distinguishes an unconfigured tier from a configured but disabled tier.
type UserSettings struct {
	Global *GlobalConfig `json:"global,omitempty"`
}

// GlobalConfigured reports whether the global tier has been configured.
func (us *UserSettings) GlobalConfigured() bool {
	return us != nil && us.Global != nil
}

// GlobalEnabled reports whether the global tier is configured and enabled.
func (us *UserSettings) GlobalEnabled() bool {
	return us.GlobalConfigured() && us.Global.Enabled
}
