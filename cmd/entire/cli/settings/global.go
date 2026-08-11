package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// UserSettingsFileName is the basename of the user-global settings file inside
// userdirs.Config(). It sits beside contexts.json and holds machine-wide
// configuration that is not tied to any repository.
const UserSettingsFileName = "settings.json"

// caseInsensitivePaths folds exclude_paths matching on macOS and Windows,
// whose default filesystems are case-insensitive: the worktree root's casing
// varies with how the agent was launched.
var caseInsensitivePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// GlobalConfig is the "global" section of the user-global settings file.
// It controls global auto-enable: tracking agent sessions in repositories
// that have no repo-level Entire setup.
type GlobalConfig struct {
	// Enabled turns global mode on. Both true and false count as configured.
	Enabled bool `json:"enabled"`

	// ExcludePaths are ~-expanded doublestar globs matched against a
	// repository's worktree root. Any match excludes that worktree.
	ExcludePaths []string `json:"exclude_paths,omitempty"`

	// ExcludeOrigins are doublestar globs matched against the origin remote
	// URL normalized to host/owner/repo. A repo without an origin matches
	// no origin pattern. Origins stored via git insteadOf shorthands (e.g.
	// gh:acme/widgets) normalize to the shorthand form, not the expanded
	// host — patterns match what git config stores.
	ExcludeOrigins []string `json:"exclude_origins,omitempty"`
}

// UserSettings is the root of the user-global settings file.
type UserSettings struct {
	Global *GlobalConfig `json:"global,omitempty"`
}

// UserSettingsPath returns the absolute path of the user-global settings
// file, for callers that name it in error messages.
func UserSettingsPath() string {
	return filepath.Join(userdirs.Config(), UserSettingsFileName)
}

// LoadUserSettings reads the user-global settings file. A missing file is not
// an error: it returns an empty UserSettings (Global == nil, meaning the
// global tier is unconfigured). A malformed file returns an error; callers on
// the hook path must treat that as global-off (fail closed).
func LoadUserSettings(_ context.Context) (*UserSettings, error) {
	// Deliberately plain os.ReadFile: readConfined/os.Root rejects absolute
	// symlinks, and ~/.config/entire/settings.json is commonly symlinked by
	// dotfile managers — with fail-closed semantics that would silently
	// disable global mode.
	data, err := os.ReadFile(UserSettingsPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &UserSettings{}, nil
		}
		return nil, fmt.Errorf("reading user settings: %w", err)
	}
	// Strict decoding: a typo'd exclude key silently failing open (tracking a
	// repo the user meant to exclude) is worse than an older CLI rejecting a
	// newer file, which fails closed and is therefore safe.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var us UserSettings
	if err := dec.Decode(&us); err != nil {
		return nil, fmt.Errorf("parsing user settings: %w", err)
	}
	return &us, nil
}

// SaveUserSettings writes the user-global settings file atomically (temp file
// in userdirs.Config() + rename, 0o600), creating the config directory if
// needed. It writes exactly the known schema: unknown keys are never
// preserved, by design — LoadUserSettings decodes strictly, so a load-modify-
// save cycle against a newer file fails at load rather than silently dropping
// keys here. It also resets the process-level caches derived from the file
// (the GlobalModeActive memo and the invisible-routing decision), so a writer
// process observes its own write.
func SaveUserSettings(_ context.Context, us *UserSettings) error {
	dir := userdirs.Config()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(us, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding user settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(UserSettingsPath(), data, 0o600); err != nil {
		return fmt.Errorf("writing user settings: %w", err)
	}
	ClearGlobalModeCache()
	paths.ClearInvisibleRuntimeCache()
	return nil
}

// normalizeOrigin reduces a git remote URL to lowercase "host/owner/repo" for
// exclude_origins matching. ssh and https forms of the same repo normalize
// identically. Unparseable input returns "" (matches no pattern).
func normalizeOrigin(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	info, err := gitremote.ParseURL(rawURL)
	if err != nil || info == nil || info.Owner == "" || info.Repo == "" {
		return ""
	}
	return strings.ToLower(info.CanonicalHost() + "/" + info.Owner + "/" + info.Repo)
}

// expandTilde expands a leading "~" or "~/" to the user home directory and
// cleans the result, so a trailing slash cannot break matching. Returns ""
// for patterns that cannot be resolved: empty input, an unavailable home dir,
// the unsupported "~user" form, or a relative pattern (which can never match
// an absolute worktree root).
func expandTilde(pattern string) string {
	if pattern == "" {
		return ""
	}
	if pattern == "~" || strings.HasPrefix(pattern, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.ToSlash(filepath.Join(home, strings.TrimPrefix(pattern, "~")))
	}
	if strings.HasPrefix(pattern, "~") {
		return ""
	}
	slashed := filepath.ToSlash(filepath.Clean(pattern))
	if !strings.HasPrefix(slashed, "/") && !filepath.IsAbs(pattern) {
		return ""
	}
	return slashed
}

// matchesExcludePath reports whether worktreeRoot matches any exclude_paths
// pattern. A pattern also excludes everything under it (p matches, or p/**
// matches). Broken patterns (unexpandable ~, relative, invalid glob) are
// skipped so one bad entry cannot disable the rest of the exclude list.
func matchesExcludePath(ctx context.Context, patterns []string, worktreeRoot string) bool {
	return matchesExcludePathFold(ctx, patterns, worktreeRoot, caseInsensitivePaths)
}

// matchesExcludePathFold is the fold-explicit seam behind matchesExcludePath,
// letting tests exercise both case sensitivities on every platform.
func matchesExcludePathFold(ctx context.Context, patterns []string, worktreeRoot string, fold bool) bool {
	root := filepath.ToSlash(worktreeRoot)
	if fold {
		root = strings.ToLower(root)
	}
	for i, p := range patterns {
		expanded := expandTilde(p)
		if expanded == "" {
			logging.Debug(ctx, "skipping unexpandable exclude_paths pattern",
				slog.Int("pattern_index", i))
			continue
		}
		if fold {
			expanded = strings.ToLower(expanded)
		}
		ok, err := doublestar.Match(expanded, root)
		if err != nil {
			logging.Debug(ctx, "skipping invalid exclude_paths glob",
				slog.Int("pattern_index", i), slog.String("error", err.Error()))
			continue
		}
		if ok {
			return true
		}
		if ok, err := doublestar.Match(expanded+"/**", root); err == nil && ok {
			return true
		}
	}
	return false
}

// matchesExcludeOrigin reports whether the normalized origin ("host/owner/repo")
// matches any exclude_origins pattern. An empty origin matches nothing.
func matchesExcludeOrigin(ctx context.Context, patterns []string, normalizedOrigin string) bool {
	if normalizedOrigin == "" {
		return false
	}
	for i, p := range patterns {
		ok, err := doublestar.Match(strings.ToLower(p), normalizedOrigin)
		if err != nil {
			logging.Debug(ctx, "skipping invalid exclude_origins glob",
				slog.Int("pattern_index", i), slog.String("error", err.Error()))
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// InactiveReason explains why the hook gate found Entire inactive for the
// current worktree. It exists so the SessionStart wrong-folder notice can name
// the reason without re-deriving (and possibly contradicting) the gate's
// decision. InactiveReasonNone accompanies an active result.
type InactiveReason int

const (
	// InactiveReasonNone: not inactive (the location is active).
	InactiveReasonNone InactiveReason = iota
	// InactiveReasonRepoDisabled: repo-level setup exists and is disabled (or
	// unreadable — fail closed). This is an explicit veto: callers surfacing
	// inactive-location notices must stay silent for it.
	InactiveReasonRepoDisabled
	// InactiveReasonGlobalExcluded: the global tier is on, but this worktree
	// matches an exclude pattern (or exclusion could not be verified — fail
	// closed).
	InactiveReasonGlobalExcluded
	// InactiveReasonGlobalOff: no repo-level setup, and the global tier is
	// unconfigured, disabled, or unreadable.
	InactiveReasonGlobalOff
)

// globalModeCache memoizes the global-mode probe per worktree root. Hooks are
// one-shot processes that evaluate the gate more than once (logging init plus
// the entry gate), and the exclude_origins check forks git each time; one
// probe per process removes that cost and guarantees every gate in the
// process sees one consistent answer. The root key keeps in-process tests
// with multiple temp repos isolated (same pattern as the invisible-routing
// cache in paths).
var (
	globalModeMu     sync.Mutex
	globalModeRoot   string
	globalModeCached bool
	globalModeActive bool
	globalModeReason InactiveReason
)

// ClearGlobalModeCache resets the memoized global-mode probe result.
// For tests that flip user-global settings mid-process; SaveUserSettings also
// calls it so a writer process observes its own write.
func ClearGlobalModeCache() {
	globalModeMu.Lock()
	globalModeRoot = ""
	globalModeCached = false
	globalModeActive = false
	globalModeReason = InactiveReasonNone
	globalModeMu.Unlock()
}

// GlobalModeActive reports whether the user-global tier activates Entire for
// the current worktree: the global tier is configured and enabled, the
// worktree is resolvable, and no exclude pattern matches. Every error path
// returns false (fail closed) — a hook must never proceed on a guess. A repo
// with no origin remote stays active (it matches no origin pattern), but a
// failed origin lookup deactivates: exclusion could not be checked.
// The result is memoized per process (see globalModeCache above).
func GlobalModeActive(ctx context.Context) bool {
	active, _ := globalModeStatus(ctx)
	return active
}

// globalModeStatus is GlobalModeActive plus the inactive reason, memoized
// together so both callers observe one consistent answer per process.
func globalModeStatus(ctx context.Context) (bool, InactiveReason) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false, InactiveReasonGlobalOff
	}
	globalModeMu.Lock()
	defer globalModeMu.Unlock()
	if globalModeCached && globalModeRoot == root {
		return globalModeActive, globalModeReason
	}
	active, reason := computeGlobalModeStatus(ctx, root)
	globalModeRoot = root
	globalModeCached = true
	globalModeActive = active
	globalModeReason = reason
	return active, reason
}

// computeGlobalModeStatus is the uncached evaluation behind globalModeStatus.
func computeGlobalModeStatus(ctx context.Context, root string) (bool, InactiveReason) {
	us, err := LoadUserSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "user settings unreadable; treating global mode as inactive",
			slog.String("error", err.Error()))
		return false, InactiveReasonGlobalOff
	}
	if us.Global == nil || !us.Global.Enabled {
		return false, InactiveReasonGlobalOff
	}
	if matchesExcludePath(ctx, us.Global.ExcludePaths, root) {
		return false, InactiveReasonGlobalExcluded
	}
	if len(us.Global.ExcludeOrigins) > 0 {
		origins, _, err := gitremote.GetRemoteURLsInDirIfSet(ctx, root, "origin")
		if err != nil {
			logging.Debug(ctx, "origin lookup failed; treating global mode as inactive",
				slog.String("error", err.Error()))
			return false, InactiveReasonGlobalExcluded
		}
		for _, origin := range origins {
			if matchesExcludeOrigin(ctx, us.Global.ExcludeOrigins, normalizeOrigin(origin)) {
				return false, InactiveReasonGlobalExcluded
			}
		}
	}
	return true, InactiveReasonNone
}

// IsActiveForRepo is the gate predicate for hooks: is Entire active for the
// current worktree, via either repo-level setup or the user-global tier?
//
//	repo setup | repo enabled | global tier              | result
//	-----------+--------------+---------------------------+-------
//	yes        | true         | (ignored)                 | true
//	yes        | false        | (ignored)                 | false (explicit veto)
//	yes        | read error   | (ignored)                 | false (fail closed)
//	no         | —            | enabled and not excluded  | true
//	no         | —            | otherwise                 | false
//
// The global tier is a fallback, not a merge layer: any repo-level setup
// (either settings file) makes the repo-level answer final.
func IsActiveForRepo(ctx context.Context) bool {
	active, _ := IsActiveForRepoWithReason(ctx)
	return active
}

// GlobalTierEnabled reports whether the user-global tracking tier is
// configured AND enabled, independent of any repository context. It exists
// for the one gate that runs where the worktree-scoped reason variant cannot:
// the SessionStart wrong-folder notice outside a git repository. Deliberately
// unmemoized — it is only called on that cold path (session-start verb, no
// repo), so the active hook path pays nothing for it. Fail closed on read
// errors, like every other gate over this file.
func GlobalTierEnabled(ctx context.Context) bool {
	us, err := LoadUserSettings(ctx)
	if err != nil {
		return false
	}
	return us.Global != nil && us.Global.Enabled
}

// IsActiveForRepoWithReason is IsActiveForRepo plus the reason a location is
// inactive, for callers that surface a notice (the SessionStart wrong-folder
// warning). It must stay the same decision as IsActiveForRepo — the reason is
// derived inside the gate, never re-computed by callers.
func IsActiveForRepoWithReason(ctx context.Context) (bool, InactiveReason) {
	if IsSetUpAny(ctx) {
		if IsSetUpAndEnabled(ctx) {
			return true, InactiveReasonNone
		}
		return false, InactiveReasonRepoDisabled
	}
	return globalModeStatus(ctx)
}
