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

	"github.com/bmatcuk/doublestar/v4"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
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
	Global *GlobalConfig `json:"global,omitempty"`
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
	data, err := os.ReadFile(filepath.Join(userdirs.Config(), UserSettingsFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &UserSettings{}, nil
		}
		return nil, fmt.Errorf("reading user settings: %w", err)
	}
	// Strict decoding: a typo'd exclude key silently failing open (tracking a
	// repo the user meant to exclude) is worse than an older CLI rejecting a
	// newer file, which fails closed and is therefore safe. Writer implication
	// (split C): an older binary cannot load a newer file at all, so a future
	// writer must not blind read-modify-write this struct — it needs a raw-map
	// preserve path or must refuse to write when unknown fields are present.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var us UserSettings
	if err := dec.Decode(&us); err != nil {
		return nil, fmt.Errorf("parsing user settings: %w", err)
	}
	return &us, nil
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
	roots := []string{filepath.ToSlash(worktreeRoot)}
	if resolved, err := filepath.EvalSymlinks(worktreeRoot); err == nil {
		if s := filepath.ToSlash(resolved); s != roots[0] {
			roots = append(roots, s)
		}
	}
	for i, p := range patterns {
		expanded, err := expandTilde(p)
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
// The Debug logs below are best-effort traces, NOT a diagnostic channel: on
// the hook paths that call this predicate, logging.Init runs only after the
// gate passes, so on every failing path these records are dropped by the
// default slog handler. A user-facing "global mode configured but inactive
// because X" surface (status/doctor) is the planned diagnosability story.
func GlobalModeActive(ctx context.Context) bool {
	us, err := LoadUserSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "user settings unreadable; treating global mode as inactive",
			slog.String("error", err.Error()))
		return false
	}
	if us.Global == nil || !us.Global.Enabled {
		return false
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "worktree unresolvable; treating global mode as inactive",
			slog.String("error", err.Error()))
		return false
	}
	excluded, err := matchesExcludePath(ctx, us.Global.ExcludePaths, root)
	if err != nil {
		logging.Debug(ctx, "unusable exclude_paths pattern; treating global mode as inactive (fail closed)",
			slog.String("error", err.Error()))
		return false
	}
	if excluded {
		return false
	}
	if len(us.Global.ExcludeOrigins) > 0 {
		origins, found, err := gitremote.GetRemoteURLsInDirIfSet(ctx, root, "origin")
		if err != nil {
			logging.Debug(ctx, "origin lookup failed; treating global mode as inactive",
				slog.String("error", err.Error()))
			return false
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
					return false
				}
				matched, err := matchesExcludeOrigin(ctx, us.Global.ExcludeOrigins, normalized)
				if err != nil {
					logging.Debug(ctx, "unusable exclude_origins pattern; treating global mode as inactive (fail closed)",
						slog.String("error", err.Error()))
					return false
				}
				if matched {
					return false
				}
			}
		}
	}
	return true
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
	if IsSetUpAny(ctx) {
		return IsSetUpAndEnabled(ctx)
	}
	return GlobalModeActive(ctx)
}
