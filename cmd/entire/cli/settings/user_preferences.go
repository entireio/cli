package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// The user settings file (~/.config/entire/settings.json) has four blocks.
// repopolicy owns and strictly decodes the two whose contents are consent or
// an executable (`global`, `redaction`); a failure in either fails the file.
// The two preference blocks are decoded here, where their types live, and a
// failure in one drops just that block with a warning — a review preference
// must never switch tracking off:
//
//	preferences   machine-wide developer defaults           (UserPreferences)
//	repos         the same shape per repository, keyed like trusted_origins
//
// Precedence, lowest to highest:
//
//	.entire/settings.json → clone preferences → preferences → repos[<this repo>] → .entire/settings.local.json
//
// The OPF command is the one exception (the user file wins; see
// enforceOPFCommandTrust).
const (
	userPreferencesBlock = "preferences"
	userReposBlock       = "repos"
)

// UserPreferences is the allowlist of settings a developer may set for
// themselves, machine-wide (`preferences`) or per repository (`repos`).
// Every key here is one whose value is the developer's own business — which
// agents review, whether telemetry is on, how verbose the log is. Nothing that
// activates tracking, changes what is redacted or where checkpoints go, or
// names an executable belongs here: those are team policy (the project file),
// consent (`global`), or exec (`redaction`), and an unknown key rejects the
// block rather than being merged.
type UserPreferences struct {
	Telemetry             *bool                          `json:"telemetry,omitempty"`
	LogLevel              string                         `json:"log_level,omitempty"`
	ReviewProfiles        map[string]ReviewProfileConfig `json:"review_profiles,omitempty"`
	ReviewDefaultProfile  string                         `json:"review_default_profile,omitempty"`
	ReviewFixAgent        string                         `json:"review_fix_agent,omitempty"`
	Investigate           *InvestigateConfig             `json:"investigate,omitempty"`
	SummaryGeneration     *SummaryGenerationSettings     `json:"summary_generation,omitempty"`
	SummaryTimeoutSeconds int                            `json:"summary_timeout_seconds,omitempty"`
}

// userOverlay is everything the user tier contributes to one settings load.
type userOverlay struct {
	opf         *repopolicy.UserOPFConfig
	preferences *UserPreferences
	repos       map[string]json.RawMessage
	rejections  []string
}

// loadUserOverlay reads the user settings file. An unreadable or invalid file
// yields an empty overlay: that file's own consumers (the global tier) already
// fail closed on it and `entire doctor` reports it, and a repo the user enabled
// here must keep loading its settings regardless — the same rule
// repopolicy.ClassifyRepoPolicyAt applies. A bad preference block is dropped
// on its own, with a warning, and recorded for UserLayerRejections.
func loadUserOverlay(ctx context.Context) userOverlay {
	var overlay userOverlay
	us, err := repopolicy.LoadUserSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "user settings unreadable; skipping user tier",
			slog.String("error", err.Error()))
		return overlay
	}
	overlay.opf = us.OPFConfig()
	if raw, ok := us.Block(userPreferencesBlock); ok {
		prefs, err := decodeUserPreferences(raw)
		if err != nil {
			overlay.reject(ctx, fmt.Sprintf("%s: %v", userPreferencesBlock, err))
		} else {
			overlay.preferences = prefs
		}
	}
	if raw, ok := us.Block(userReposBlock); ok && !isJSONNull(raw) {
		var repos map[string]json.RawMessage
		if err := json.Unmarshal(raw, &repos); err != nil {
			overlay.reject(ctx, fmt.Sprintf("%s: must be an object keyed by host/owner/repo or absolute path: %v", userReposBlock, err))
		} else {
			overlay.repos = repos
		}
	}
	return overlay
}

// UserPreferenceRejections reports the user-settings preference blocks (and
// this worktree's repos entries) that Load would drop, computed from the user
// file ALONE. Surfaces that must show these even when the repository's own
// settings cannot be loaded — status's not-set-up, globally-tracked, and
// excluded branches — use this instead of a full Load, so a broken
// .entire/settings.json or clone-preferences file can never hide a warning
// about the machine-wide user file.
func UserPreferenceRejections(ctx context.Context) []string {
	overlay := loadUserOverlay(ctx)
	if len(overlay.repos) > 0 {
		if root, err := paths.WorktreeRoot(ctx); err == nil {
			_ = overlay.repoPreferences(ctx, root)
		}
	}
	return overlay.rejections
}

// reject records a dropped block for the consumers that surface it — `entire
// status` lists rejections and the hook path warns via warnUser. Debug, not
// Warn, here: settings.Load is uncached and runs several times per command,
// so a Warn at this level prints the same line once per Load, raw on stderr,
// before any logger is initialized.
func (o *userOverlay) reject(ctx context.Context, reason string) {
	o.rejections = append(o.rejections, reason)
	logging.Debug(ctx, "user settings block ignored",
		slog.String("file", repopolicy.UserSettingsPath()),
		slog.String("reason", reason))
}

// decodeUserPreferences strictly decodes one preferences object. A JSON null
// is "unset". Cross-field validation (a summary model without a provider) is
// deferred to the merged result, as it is for the project and local files —
// the provider may legitimately come from another layer.
func decodeUserPreferences(raw json.RawMessage) (*UserPreferences, error) {
	if isJSONNull(raw) {
		return nil, nil //nolint:nilnil // nil is the documented "block unset" value, not an error
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var prefs UserPreferences
	if err := decoder.Decode(&prefs); err != nil {
		return nil, err //nolint:wrapcheck // the caller prefixes the block name
	}
	if prefs.SummaryTimeoutSeconds < 0 {
		return nil, fmt.Errorf("summary_timeout_seconds must be greater than or equal to 0 (got %d)", prefs.SummaryTimeoutSeconds)
	}
	return &prefs, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// repoPreferences returns the `repos` entries that name this worktree, in key
// order, each decoded like `preferences`. An entry is keyed either by a
// normalized origin (host/owner/repo — the same key trusted_origins and
// exclude_origins use, derived by repopolicy.OriginKeysAt from every fetch and
// push URL of `origin`) or, for a repository with no usable origin, by its
// absolute worktree path (matched the way trusted_paths is: ~-expanded,
// symlink-resolved on both sides). Nothing is resolved unless the block has
// entries, so a user file without `repos` costs no git reads.
//
// Several entries can match one repository (an origin with two URLs, or a path
// and an origin); they apply in sorted key order so the result is
// deterministic. Entries that fail to decode are dropped individually.
func (o *userOverlay) repoPreferences(ctx context.Context, worktreeRoot string) []*UserPreferences {
	if len(o.repos) == 0 {
		return nil
	}
	originKeys, _, err := originKeysAtCached(ctx, worktreeRoot)
	if err != nil {
		// Present but unnormalizable, or unreadable: the repository keys by
		// path, exactly as its trust identity would.
		logging.Debug(ctx, "repos: origin keys unavailable; matching by path only",
			slog.String("error", err.Error()))
		originKeys = nil
	}
	names := make([]string, 0, len(o.repos))
	for name := range o.repos {
		names = append(names, name)
	}
	slices.Sort(names)

	var matched []*UserPreferences
	for _, name := range names {
		if !repoKeyMatches(ctx, name, originKeys, worktreeRoot) {
			continue
		}
		prefs, err := decodeUserPreferences(o.repos[name])
		if err != nil {
			o.reject(ctx, fmt.Sprintf("%s[%q]: %v", userReposBlock, name, err))
			continue
		}
		if prefs != nil {
			matched = append(matched, prefs)
		}
	}
	return matched
}

// originKeysByRoot memoizes OriginKeysAt for the process lifetime.
//
// settings.Load has no caching of its own and runs ~5 times per hook, and
// OriginKeysAt shells out to git twice (fetch + push URLs) — so once a repo
// has a `repos` block, an unmemoized resolve multiplies two subprocess spawns
// into ten per hook. A repository's origin URLs cannot change inside a single
// short-lived hook process. Only successful determinations are cached; an
// error means "could not read the remote config" and may be transient. Same
// shape, and same long-lived-process caveat, as versionedPaths in
// opf_command_trust.go.
type originKeysResult struct {
	keys    []string
	present bool
}

var (
	originKeysMu     sync.Mutex
	originKeysByRoot = map[string]originKeysResult{}
)

// ClearOriginKeyCache drops the memoized origin keys. Tests that change a
// repository's remotes within one process must call it; every other
// process-wide cache in this codebase ships the same seam.
func ClearOriginKeyCache() {
	originKeysMu.Lock()
	defer originKeysMu.Unlock()
	clear(originKeysByRoot)
}

func originKeysAtCached(ctx context.Context, worktreeRoot string) ([]string, bool, error) {
	cacheKey := filepath.Clean(worktreeRoot)

	originKeysMu.Lock()
	cached, ok := originKeysByRoot[cacheKey]
	originKeysMu.Unlock()
	if ok {
		return cached.keys, cached.present, nil
	}

	keys, present, err := repopolicy.OriginKeysAt(ctx, worktreeRoot)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // pass-through; the caller logs and degrades to path matching
	}

	originKeysMu.Lock()
	originKeysByRoot[cacheKey] = originKeysResult{keys: keys, present: present}
	originKeysMu.Unlock()
	return keys, present, nil
}

// repoKeyMatches decides whether one `repos` key names the worktree. A key
// that reads as a filesystem path (absolute, drive-rooted, or ~-prefixed) is
// compared as a path; anything else is an origin key, compared case-folded
// because NormalizeOrigin lower-cases.
func repoKeyMatches(ctx context.Context, key string, originKeys []string, worktreeRoot string) bool {
	if isPathKey(key) {
		return pathEntryIsRoot(ctx, key, worktreeRoot)
	}
	return slices.Contains(originKeys, strings.ToLower(strings.TrimSpace(key)))
}

func isPathKey(key string) bool {
	key = strings.TrimSpace(key)
	return strings.HasPrefix(key, "/") || strings.HasPrefix(key, "~") ||
		filepath.IsAbs(key) || filepath.VolumeName(key) != ""
}

// applyUserPreferences overlays one preferences object onto the merged
// settings with the same per-key semantics the local file gets from
// mergeJSON: scalars override when set, review profiles merge by name, the
// summary provider goes through SetProvider so a provider switch drops a model
// that belonged to the old provider.
func applyUserPreferences(settings *EntireSettings, prefs *UserPreferences) {
	if settings == nil || prefs == nil {
		return
	}
	if prefs.Telemetry != nil {
		v := *prefs.Telemetry
		settings.Telemetry = &v
	}
	if prefs.LogLevel != "" {
		settings.LogLevel = prefs.LogLevel
	}
	if prefs.ReviewProfiles != nil {
		settings.ReviewProfiles = mergeReviewProfiles(settings.ReviewProfiles, prefs.ReviewProfiles)
	}
	if prefs.ReviewDefaultProfile != "" {
		settings.ReviewDefaultProfile = prefs.ReviewDefaultProfile
	}
	if prefs.ReviewFixAgent != "" {
		settings.ReviewFixAgent = prefs.ReviewFixAgent
	}
	if prefs.Investigate != nil {
		cfg := *prefs.Investigate
		settings.Investigate = &cfg
	}
	if prefs.SummaryGeneration != nil {
		if settings.SummaryGeneration == nil {
			settings.SummaryGeneration = &SummaryGenerationSettings{}
		}
		settings.SummaryGeneration.SetProvider(prefs.SummaryGeneration.Provider, prefs.SummaryGeneration.Model)
	}
	if prefs.SummaryTimeoutSeconds > 0 {
		settings.SummaryTimeoutSeconds = prefs.SummaryTimeoutSeconds
	}
}
