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
	RuntimeUnknown   RuntimeLayout = ""
	RuntimeWorktree  RuntimeLayout = "worktree"
	RuntimeGitCommon RuntimeLayout = "git_common"
)

// RuntimeRoute is the selected or proposed runtime-data location. Route
// establishment is implemented separately from the input classification in
// this package.
type RuntimeRoute struct {
	Layout RuntimeLayout
	Root   string
}

// TrustDecision is the repository's checkpoint-egress decision. Trust
// identity and persistence are added independently of activation inputs.
type TrustDecision struct {
	Allowed bool
	Source  string
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

// GlobalConfig is the global section of the user settings file. Its JSON
// contract is intentionally stable because users edit this file directly.
type GlobalConfig struct {
	Enabled           bool     `json:"enabled"`
	ExcludePaths      []string `json:"exclude_paths,omitempty"`
	ExcludePathsExact []string `json:"exclude_paths_exact,omitempty"`
	ExcludeOrigins    []string `json:"exclude_origins,omitempty"`
	TrustAll          bool     `json:"trust_all,omitempty"`
	TrustedOrigins    []string `json:"trusted_origins,omitempty"`
	TrustedPaths      []string `json:"trusted_paths,omitempty"`
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
