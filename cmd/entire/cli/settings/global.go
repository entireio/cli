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
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
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
}

// UserSettings is the root of the user-global settings file.
type UserSettings struct {
	// Global == nil is load-bearing: it means the global tier has never
	// been configured (nobody has answered the global-enable question),
	// which is distinct from a configured-but-disabled tier (non-nil with
	// Enabled == false — a recorded "no"). Writers must preserve the
	// distinction: never materialize an empty GlobalConfig as a side
	// effect of an unrelated write.
	Global *GlobalConfig `json:"global,omitempty"`
}

// GlobalConfigured reports whether the global tier has been configured —
// either answer counts (the ask-once wizard keys off this). Nil-safe, so
// callers never repeat the raw us.Global nil-check.
func (us *UserSettings) GlobalConfigured() bool {
	return us != nil && us.Global != nil
}

// GlobalEnabled reports whether the global tier is configured AND enabled.
// Nil-safe. This is only the file's own bit — the full activation gate
// (worktree resolution, exclude lists, fail-closed rules) is GlobalModeActive.
func (us *UserSettings) GlobalEnabled() bool {
	return us.GlobalConfigured() && us.Global.Enabled
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
	// newer file, which fails closed and is therefore safe. Writer
	// implication: SaveUserSettings (below) writes exactly the known schema
	// and never preserves unknown keys — safe only because every writer
	// load-modify-saves through this strict decoder, so a write against a
	// newer file fails here at load rather than silently dropping its keys.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var us UserSettings
	if err := dec.Decode(&us); err != nil {
		return nil, fmt.Errorf("parsing user settings: %w", err)
	}
	return &us, nil
}

// ModifyUserSettings runs a read-modify-write of the user-global settings
// file under a file lock, so two concurrent writers — or a prompt held open
// while another process writes (the enable wizard's window) — cannot clobber
// each other's changes. fn receives the freshly loaded settings; returning an
// error aborts without writing. A load failure aborts too: the strict decoder
// is what keeps this rewrite from silently dropping a newer binary's keys.
func ModifyUserSettings(ctx context.Context, fn func(*UserSettings) error) error {
	dir := userdirs.Config()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	release, err := flock.Acquire(UserSettingsPath() + ".lock")
	if err != nil {
		return fmt.Errorf("lock user settings: %w", err)
	}
	defer release()
	us, err := LoadUserSettings(ctx)
	if err != nil {
		return err
	}
	if err := fn(us); err != nil {
		return err
	}
	return persistUserSettings(us)
}

// SaveUserSettings replaces the user-global settings file with us, delegating
// to ModifyUserSettings so the write happens under the same lock (and the
// same strict-load protection). Prefer ModifyUserSettings directly for
// read-modify-write flows; this whole-struct form suits callers that own the
// complete desired state.
func SaveUserSettings(ctx context.Context, us *UserSettings) error {
	return ModifyUserSettings(ctx, func(cur *UserSettings) error {
		*cur = *us
		return nil
	})
}

// persistUserSettings writes the user-global settings file atomically (temp
// file next to the target + rename, 0o600). A symlinked settings.json is
// resolved and its target rewritten rather than the link being replaced. It
// writes exactly the known schema: unknown keys are never preserved, by
// design — LoadUserSettings decodes strictly, so every load-modify-save cycle
// against a newer file fails at load rather than silently dropping keys here.
// It also resets the process-level caches derived from the file (the
// global-mode memo and the invisible-routing decision), so a writer process
// observes its own write. Callers hold the user-settings lock.
func persistUserSettings(us *UserSettings) error {
	data, err := jsonutil.MarshalIndentWithNewline(us, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding user settings: %w", err)
	}
	// Symlink-following write: a settings.json symlinked by a dotfile manager
	// (the same reason LoadUserSettings avoids readConfined) must be rewritten
	// through to its target, not replaced by the atomic rename.
	if err := jsonutil.WriteFileAtomicFollowingSymlinks(UserSettingsPath(), data, 0o600); err != nil {
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
	host := info.CanonicalHost()
	if host == "" {
		// file:// URLs parse with an empty host; a host-less "origin" cannot
		// take the host/owner/repo shape patterns are written against.
		return ""
	}
	return strings.ToLower(host + "/" + info.Owner + "/" + info.Repo)
}

// expandTilde expands a leading "~" or "~/" to the user home directory and
// cleans the result, so a trailing slash cannot break matching. Surrounding
// whitespace is trimmed first: the file is hand-edited, and a stray trailing
// space would otherwise make the pattern silently never match — a quiet
// fail-open. A blank entry returns ("", nil) — no intent to honor, skip it.
// A non-blank pattern that cannot be resolved (unavailable home dir, the
// unsupported "~user" form, or a relative pattern, which can never match an
// absolute worktree root) returns an error: the caller fails closed rather
// than tracking a repo the user meant to exclude.
func expandTilde(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", nil
	}
	if pattern == "~" || strings.HasPrefix(pattern, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %w", err)
		}
		return filepath.ToSlash(filepath.Join(home, strings.TrimPrefix(pattern, "~"))), nil
	}
	if strings.HasPrefix(pattern, "~") {
		return "", errors.New("unsupported ~user form")
	}
	slashed := filepath.ToSlash(filepath.Clean(pattern))
	if !strings.HasPrefix(slashed, "/") && !filepath.IsAbs(pattern) {
		return "", errors.New("relative pattern cannot match an absolute worktree root")
	}
	return slashed, nil
}

// splitGlobPrefix splits a slash-separated pattern into its literal leading
// directory part and the remainder starting at the first segment containing a
// doublestar meta character (* ? [ {). rest is "" when the whole pattern is
// literal.
func splitGlobPrefix(pattern string) (prefix, rest string) {
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		if strings.ContainsAny(seg, "*?[{") {
			return strings.Join(segments[:i], "/"), strings.Join(segments[i:], "/")
		}
	}
	return pattern, ""
}

// resolveGlobPrefixSymlinks returns a variant of an expanded pattern with its
// literal directory prefix symlink-resolved, or "" when no distinct variant
// exists. This is what lets `~/code/**` exclude repos whose worktree root is
// physical (`/Volumes/dev/code/repo`) while `~/code` is a symlink: the
// pattern is built from the logical home path, the root from git, and the
// two only meet in canonical form.
func resolveGlobPrefixSymlinks(expanded string) string {
	prefix, rest := splitGlobPrefix(expanded)
	if prefix == "" || prefix == "/" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.FromSlash(prefix))
	if err != nil {
		return "" // prefix doesn't exist or can't resolve — the literal form already covers it
	}
	slashed := filepath.ToSlash(resolved)
	if slashed == prefix {
		return ""
	}
	if rest != "" {
		return slashed + "/" + rest
	}
	return slashed
}

// checkExcludePathPattern expands and validates a single exclude_paths
// pattern — the one definition of "usable" shared by the fail-closed matcher
// and doctor's ValidateGlobalConfig, so the two surfaces can never disagree
// about which patterns deactivate the tier. Returns the expanded pattern,
// "" for a blank entry (no intent to honor), or the error both report.
func checkExcludePathPattern(p string) (string, error) {
	expanded, err := expandTilde(p)
	if err != nil {
		return "", err
	}
	if expanded == "" {
		return "", nil
	}
	if !doublestar.ValidatePattern(expanded) {
		return "", errors.New("invalid glob")
	}
	return expanded, nil
}

// matchesExcludePath reports whether worktreeRoot matches any exclude_paths
// pattern. A pattern also excludes everything under it (p matches, or p/**
// matches). Blank entries are skipped; an UNUSABLE pattern (unexpandable ~,
// relative, invalid glob) returns an error so the caller can fail closed —
// the same reasoning LoadUserSettings applies to a typo'd exclude key: a
// typo'd pattern must not silently track a repo the user meant to exclude.
func matchesExcludePath(ctx context.Context, patterns []string, worktreeRoot string) (bool, error) {
	return matchesExcludePathFold(ctx, patterns, worktreeRoot, caseInsensitivePaths)
}

// matchesExcludePathFold is the fold-explicit seam behind matchesExcludePath,
// letting tests exercise both case sensitivities on every platform. The root
// is matched in its given and symlink-resolved forms, each pattern in its
// expanded and glob-prefix-resolved forms — four combinations, so a logical
// pattern still matches a physical root and vice versa.
func matchesExcludePathFold(_ context.Context, patterns []string, worktreeRoot string, fold bool) (bool, error) {
	roots := rootMatchForms(worktreeRoot)
	for i, p := range patterns {
		expanded, err := checkExcludePathPattern(p)
		if err != nil {
			return false, fmt.Errorf("exclude_paths[%d]: %w", i, err)
		}
		if expanded == "" {
			continue // blank entry — no intent to honor
		}
		variants := []string{expanded}
		if alt := resolveGlobPrefixSymlinks(expanded); alt != "" {
			variants = append(variants, alt)
		}
		for _, pv := range variants {
			if fold {
				pv = strings.ToLower(pv)
			}
			for _, root := range roots {
				if fold {
					root = strings.ToLower(root)
				}
				ok, matchErr := doublestar.Match(pv, root)
				if matchErr != nil {
					return false, fmt.Errorf("exclude_paths[%d]: invalid glob: %w", i, matchErr)
				}
				if ok {
					return true, nil
				}
				// If pv was a valid pattern, pv+"/**" is too — no error path.
				if ok, _ := doublestar.Match(pv+"/**", root); ok { //nolint:errcheck // validity established by the bare-pattern Match above
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// rootMatchForms returns the worktree root in its given and (when distinct)
// symlink-resolved forms, slash-normalized, so a logical pattern still
// matches a physical root and vice versa.
func rootMatchForms(worktreeRoot string) []string {
	roots := []string{filepath.ToSlash(worktreeRoot)}
	if resolved, err := filepath.EvalSymlinks(worktreeRoot); err == nil {
		if s := filepath.ToSlash(resolved); s != roots[0] {
			roots = append(roots, s)
		}
	}
	return roots
}

// matchesExcludePathExact reports whether worktreeRoot IS one of the
// exclude_paths_exact entries. Entries are plain paths, not globs, and there
// is deliberately no subtree cascade — a repo checked out BENEATH an
// excluded-exact path is not excluded (that is the whole point; see the
// ExcludePathsExact field doc). Blank entries are skipped; an unusable entry
// returns an error so the caller fails closed, exactly like exclude_paths.
func matchesExcludePathExact(ctx context.Context, entries []string, worktreeRoot string) (bool, error) {
	return matchesExcludePathExactFold(ctx, entries, worktreeRoot, caseInsensitivePaths)
}

// matchesExcludePathExactFold is the fold-explicit seam behind
// matchesExcludePathExact. Both the root and the entry are tried in given and
// symlink-resolved forms, mirroring matchesExcludePathFold, so an entry typed
// against a logical (symlinked) path still matches the physical worktree root
// git reports, and vice versa.
func matchesExcludePathExactFold(_ context.Context, entries []string, worktreeRoot string, fold bool) (bool, error) {
	roots := rootMatchForms(worktreeRoot)
	for i, e := range entries {
		expanded, err := expandTilde(e)
		if err != nil {
			return false, fmt.Errorf("exclude_paths_exact[%d]: %w", i, err)
		}
		if expanded == "" {
			continue // blank entry — no intent to honor
		}
		variants := []string{expanded}
		if resolved, symErr := filepath.EvalSymlinks(filepath.FromSlash(expanded)); symErr == nil {
			if s := filepath.ToSlash(resolved); s != expanded {
				variants = append(variants, s)
			}
		}
		for _, v := range variants {
			for _, root := range roots {
				if fold {
					v = strings.ToLower(v)
					root = strings.ToLower(root)
				}
				if v == root {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// matchesExcludeOrigin reports whether the normalized origin ("host/owner/repo")
// matches any exclude_origins pattern. An empty origin matches nothing.
// Patterns are whitespace-trimmed (hand-edited file; a stray space must not
// silently disable an exclusion) and blank entries are skipped; an invalid
// glob returns an error so the caller can fail closed.
func matchesExcludeOrigin(_ context.Context, patterns []string, normalizedOrigin string) (bool, error) {
	if normalizedOrigin == "" {
		return false, nil
	}
	for i, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ok, err := doublestar.Match(strings.ToLower(p), normalizedOrigin)
		if err != nil {
			return false, fmt.Errorf("exclude_origins[%d]: invalid glob: %w", i, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
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
// For tests that flip user-global settings mid-process; every user-settings
// writer also reaches it through persistUserSettings (ModifyUserSettings and
// SaveUserSettings alike) so a writer process observes its own write.
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
// returns false (fail closed) — a hook must never proceed on a guess — and
// that includes exclusion errors: an unusable exclude pattern or an origin
// that is present but cannot be normalized deactivates global mode, because
// "could not check the exclusion" must not degrade into "track the repo the
// user meant to exclude". A repo with no origin remote stays active (it
// matches no origin pattern).
//
// The result is memoized per process (see globalModeCache above).
//
// The Debug logs here and in computeGlobalModeStatus are best-effort traces,
// NOT a diagnostic channel: on the hook paths that call this predicate,
// logging.Init runs only after the gate passes, so on every failing path
// these records are dropped by the default slog handler. The user-facing
// diagnosability surfaces are doctor's checkGlobalTracking (unreadable user
// settings, missing/unverifiable user-level hooks, unusable exclude
// patterns) and `entire status`, whose global-tracking line reports the
// per-repo carve-out.
func GlobalModeActive(ctx context.Context) bool {
	active, _ := globalModeStatus(ctx)
	return active
}

// globalModeStatus is GlobalModeActive plus the inactive reason, memoized
// together so both callers observe one consistent answer per process.
func globalModeStatus(ctx context.Context) (bool, InactiveReason) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "worktree unresolvable; treating global mode as inactive",
			slog.String("error", err.Error()))
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
	if !us.GlobalEnabled() {
		return false, InactiveReasonGlobalOff
	}
	excluded, err := matchesExcludePath(ctx, us.Global.ExcludePaths, root)
	if err != nil {
		logging.Debug(ctx, "unusable exclude_paths pattern; treating global mode as inactive (fail closed)",
			slog.String("error", err.Error()))
		return false, InactiveReasonGlobalExcluded
	}
	if excluded {
		return false, InactiveReasonGlobalExcluded
	}
	excludedExact, err := matchesExcludePathExact(ctx, us.Global.ExcludePathsExact, root)
	if err != nil {
		logging.Debug(ctx, "unusable exclude_paths_exact entry; treating global mode as inactive (fail closed)",
			slog.String("error", err.Error()))
		return false, InactiveReasonGlobalExcluded
	}
	if excludedExact {
		return false, InactiveReasonGlobalExcluded
	}
	if len(us.Global.ExcludeOrigins) > 0 {
		origins, found, err := gitremote.GetRemoteURLsInDirIfSet(ctx, root, "origin")
		if err != nil {
			logging.Debug(ctx, "origin lookup failed; treating global mode as inactive",
				slog.String("error", err.Error()))
			return false, InactiveReasonGlobalExcluded
		}
		// found==false means the repo has NO origin remote: nothing exists
		// for a pattern to match, so the tier stays active (documented on
		// ExcludeOrigins). found==true means URLs exist and every one of
		// them must be checkable.
		if found {
			for _, origin := range origins {
				normalized := normalizeOrigin(origin)
				if normalized == "" {
					// Present but unparseable (bare path, file://): exclusion
					// could not be checked against this origin — fail closed.
					logging.Debug(ctx, "origin unparseable; treating global mode as inactive (fail closed)")
					return false, InactiveReasonGlobalExcluded
				}
				matched, err := matchesExcludeOrigin(ctx, us.Global.ExcludeOrigins, normalized)
				if err != nil {
					logging.Debug(ctx, "unusable exclude_origins pattern; treating global mode as inactive (fail closed)",
						slog.String("error", err.Error()))
					return false, InactiveReasonGlobalExcluded
				}
				if matched {
					return false, InactiveReasonGlobalExcluded
				}
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
	return err == nil && us.GlobalEnabled()
}

// IsActiveForRepoWithReason is IsActiveForRepo plus the reason a location is
// inactive, for callers that surface a notice (the SessionStart wrong-folder
// warning). It must stay the same decision as IsActiveForRepo — the reason is
// derived inside the gate, never re-computed by callers.
func IsActiveForRepoWithReason(ctx context.Context) (bool, InactiveReason) {
	if IsSetUpAny(ctx) {
		// IsSetUpAny just established setup exists; going through
		// IsSetUpAndEnabled here would repeat its Lstat pair on every hook
		// invocation.
		if repoSettingsEnabled(ctx) {
			return true, InactiveReasonNone
		}
		return false, InactiveReasonRepoDisabled
	}
	return globalModeStatus(ctx)
}
