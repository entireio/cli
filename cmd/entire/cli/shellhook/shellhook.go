// Package shellhook stores the per-user preferences and throttling state for
// the optional `entire shellhook` shell integration.
//
// The shell hook fires on every directory change, so everything here is
// best-effort and non-fatal: a missing preferences file simply means the hook
// is off, and a corrupt state file degrades to "warn once, then rewrite".
//
// All paths resolve exclusively through internal/entireclient/userdirs, which
// is the single source of truth for the per-user config and cache directories
// (and gives tests ENTIRE_CONFIG_DIR / XDG_CACHE_HOME isolation for free).
package shellhook

import "time"

// Mode selects what the shell hook does when it lands in a repository where
// Entire has not been set up.
type Mode string

const (
	// ModeOff disables the hook entirely. It is the zero value and the
	// behavior for a missing preferences file, so an un-installed hook can
	// never produce output.
	ModeOff Mode = "off"
	// ModeWarn prints a single throttled warning line to stderr.
	ModeWarn Mode = "warn"
	// ModeAuto offers to run `entire enable` when a terminal is available,
	// and degrades to ModeWarn when it is not.
	ModeAuto Mode = "auto"
)

// Valid reports whether m is a mode the CLI knows how to execute.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeWarn, ModeAuto:
		return true
	default:
		return false
	}
}

const (
	// PreferencesVersion is the current on-disk preferences schema version.
	PreferencesVersion = 1

	// DefaultWarnThrottleHours is how long a repository stays quiet after a
	// warning when the preferences do not say otherwise.
	DefaultWarnThrottleHours = 24

	// MaxStateEntries caps the per-repository state map so a user who visits
	// many repositories cannot grow the cache file without bound.
	MaxStateEntries = 500

	preferencesFileName = "shellhook.json"
	stateFileName       = "shellhook_state.json"

	// dirPerm/filePerm keep per-user state readable only by its owner: the
	// state file records which repositories the user visits.
	dirPerm  = 0o700
	filePerm = 0o600
)

// Preferences is the user-level configuration for the shell hook, stored at
// userdirs.Config()/shellhook.json.
type Preferences struct {
	Version int  `json:"version"`
	Mode    Mode `json:"mode"`
	// DefaultAgents are the agents passed to `entire enable` in ModeAuto, on
	// top of whatever is detected in the repository.
	DefaultAgents []string `json:"default_agents,omitempty"`
	// AutoEnableNoConfirm skips the confirmation prompt in ModeAuto.
	AutoEnableNoConfirm bool `json:"auto_enable_no_confirm,omitempty"`
	// WarnThrottleHours overrides DefaultWarnThrottleHours when positive.
	WarnThrottleHours int `json:"warn_throttle_hours,omitempty"`
}

// WarnThrottle returns the configured quiet period between warnings for a
// single repository.
func (p *Preferences) WarnThrottle() time.Duration {
	if p == nil || p.WarnThrottleHours <= 0 {
		return DefaultWarnThrottleHours * time.Hour
	}
	return time.Duration(p.WarnThrottleHours) * time.Hour
}

// RepoState is the per-repository throttling record.
type RepoState struct {
	// LastWarnedAt doubles as the prune recency key, so MarkDismissed stamps
	// it too — otherwise a dismissal would be the first thing evicted.
	LastWarnedAt time.Time `json:"last_warned_at,omitempty"`
	Dismissed    bool      `json:"dismissed,omitempty"`
}

// State is the throttling cache, stored at
// userdirs.Cache()/shellhook_state.json and keyed by git common directory.
type State struct {
	Repos map[string]RepoState `json:"repos,omitempty"`
}
