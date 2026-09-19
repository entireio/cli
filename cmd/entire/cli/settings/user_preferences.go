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
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// The user settings file (~/.config/entire/settings.json) carries two blocks
// this package owns. usersettings decodes the blocks whose contents it owns
// and hands these back as raw JSON, because their types live here and it
// cannot import this package.
//
//	preferences   machine-wide developer defaults           (UserPreferences)
//	repos         the same shape per repository, keyed by normalized origin
//	              or absolute worktree path
//
// Precedence, lowest to highest:
//
//	.entire/settings.json → clone preferences → preferences → repos[<this repo>] → .entire/settings.local.json
//
// A failure in one of these blocks drops just that block, with the reason
// recorded for the consumer to report — a review preference must never be able
// to switch tracking off. That is the opposite of the rule usersettings
// applies to the block it decodes itself, where an unknown key fails the file,
// because that block names an executable.
const (
	userPreferencesBlock = "preferences"
	userReposBlock       = "repos"
)

// UserPreferences is the allowlist of settings a developer may set for
// themselves, machine-wide (`preferences`) or per repository (`repos`).
//
// Every key here is one whose value is the developer's own business — which
// agents review, whether telemetry is on, how verbose the log is. The
// allowlist IS the boundary: an unknown key rejects the block rather than
// being merged, so widening what may live here is a deliberate edit to this
// struct and not something a settings file can do on its own.
type UserPreferences struct {
	Telemetry             *bool                          `json:"telemetry,omitempty"`
	LogLevel              string                         `json:"log_level,omitempty"`
	ReviewProfiles        map[string]ReviewProfileConfig `json:"review_profiles,omitempty"`
	ReviewDefaultProfile  string                         `json:"review_default_profile,omitempty"`
	ReviewFixAgent        string                         `json:"review_fix_agent,omitempty"`
	Investigate           *InvestigateConfig             `json:"investigate,omitempty"`
	SummaryGeneration     *SummaryGenerationSettings     `json:"summary_generation,omitempty"`
	SummaryTimeoutSeconds int                            `json:"summary_timeout_seconds,omitempty"`

	// Enabled is "did this developer turn Entire on here", which is a fact
	// about a person and a repository and never about one worktree of it.
	// Pointer-shaped so an absent key stays distinguishable from an explicit
	// false: IsSetUpAny has to tell "never configured" from "configured off",
	// and a bool would collapse them.
	Enabled *bool `json:"enabled,omitempty"`

	// CheckpointRemote is where this repository's transcripts go. Typed rather
	// than reached through StrategyOptions, which is a map[string]any and so
	// would accept any key at all — the allowlist is the boundary, and a
	// free-form map inside it is a hole in that boundary. Applied into
	// StrategyOptions so every existing reader is unchanged.
	CheckpointRemote *CheckpointRemoteConfig `json:"checkpoint_remote,omitempty"`
}

// userOverlay is everything the user tier contributes to one settings load.
type userOverlay struct {
	preferences *UserPreferences
	repos       map[string]json.RawMessage
	rejections  []string
}

// loadUserOverlay reads the user settings file. An unreadable or invalid file
// yields an empty overlay rather than failing the load: a repository must keep
// loading its own settings regardless of the state of a machine-wide file, and
// `entire doctor` reports that file separately. A bad preference block is
// dropped on its own, with the reason recorded.
func loadUserOverlay(ctx context.Context) userOverlay {
	var overlay userOverlay
	us, err := usersettings.Load(ctx)
	if err != nil {
		logging.Debug(ctx, "user settings unreadable; skipping user tier",
			slog.String("error", err.Error()))
		return overlay
	}
	if raw, ok := us.Block(userPreferencesBlock); ok {
		prefs, err := decodeUserPreferences(raw)
		if err != nil {
			overlay.reject(ctx, fmt.Sprintf("%s: %v", userPreferencesBlock, err))
		} else {
			overlay.preferences = prefs
		}
	}
	if raw, ok := us.Block(userReposBlock); ok && !usersettings.IsJSONNull(raw) {
		var repos map[string]json.RawMessage
		if err := json.Unmarshal(raw, &repos); err != nil {
			overlay.reject(ctx, fmt.Sprintf("%s: must be an object keyed by host/owner/repo or absolute path: %v", userReposBlock, err))
		} else {
			overlay.repos = repos
		}
	}
	return overlay
}

// reject records a dropped block for the consumers that surface it. Debug, not
// Warn: settings.Load is uncached and runs several times per command, so a
// Warn here prints the same line once per Load, raw on stderr, before any
// logger is initialized.
func (o *userOverlay) reject(ctx context.Context, reason string) {
	o.rejections = append(o.rejections, reason)
	logging.Debug(ctx, "user settings block ignored",
		slog.String("file", usersettings.Path()),
		slog.String("reason", reason))
}

// decodeUserPreferences strictly decodes one preferences object. A JSON null
// is "unset". Cross-field validation (a summary model without a provider) is
// deferred to the merged result, as it is for the project and local files —
// the provider may legitimately come from another layer.
func decodeUserPreferences(raw json.RawMessage) (*UserPreferences, error) {
	if usersettings.IsJSONNull(raw) {
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

// repoPreferences returns the `repos` entries that name this worktree, in key
// order, each decoded like `preferences`. An entry is keyed either by a
// normalized origin (host/owner/repo, derived from every fetch and push URL of
// `origin`) or, for a repository with no usable origin, by its absolute
// worktree path. Nothing is resolved unless the block has entries, so a user
// file without `repos` costs no git reads at all.
//
// Several entries can match one repository (an origin with two URLs, or a path
// and an origin); they apply in sorted key order so the result is
// deterministic. Entries that fail to decode are dropped individually.
func (o *userOverlay) repoPreferences(ctx context.Context, worktreeRoot string) []*UserPreferences {
	if len(o.repos) == 0 || worktreeRoot == "" {
		return nil
	}
	originKeys, _, err := originKeysCached(ctx, worktreeRoot)
	if err != nil {
		// Present but unnormalizable, or unreadable: fall back to matching by
		// path. Treating this as "no origin" would silently drop an entry the
		// user can see in their own file.
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

// originKeysByRoot memoizes usersettings.OriginKeys for the process lifetime.
//
// settings.Load has no caching of its own and runs ~5 times per hook, and
// resolving origin keys shells out to git twice (fetch + push URLs) — so once a
// repository has a `repos` block, an unmemoized resolve turns two subprocess
// spawns into ten per hook. A repository's origin URLs cannot change inside a
// single short-lived hook process. Only successful determinations are cached;
// an error means "could not read the remote config" and may be transient.
//
// Same shape, and the same long-lived-process caveat, as versionedPaths in
// opf_command_trust.go: a developer who changes a remote mid-session in
// `entire mcp` keeps the old answer until restart.
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

func originKeysCached(ctx context.Context, worktreeRoot string) ([]string, bool, error) {
	cacheKey := filepath.Clean(worktreeRoot)

	originKeysMu.Lock()
	cached, ok := originKeysByRoot[cacheKey]
	originKeysMu.Unlock()
	if ok {
		return cached.keys, cached.present, nil
	}

	keys, present, err := usersettings.OriginKeys(ctx, worktreeRoot)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // the caller logs and degrades to path matching
	}

	originKeysMu.Lock()
	originKeysByRoot[cacheKey] = originKeysResult{keys: keys, present: present}
	originKeysMu.Unlock()
	return keys, present, nil
}

// repoKeyMatches decides whether one `repos` key names this worktree. A key
// that reads as a filesystem path is compared as a path; anything else is an
// origin key, compared case-folded because normalization lower-cases.
func repoKeyMatches(ctx context.Context, key string, originKeys []string, worktreeRoot string) bool {
	if isPathKey(key) {
		return usersettings.PathNamesThisClone(ctx, key, worktreeRoot)
	}
	return slices.Contains(originKeys, strings.ToLower(strings.TrimSpace(key)))
}

func isPathKey(key string) bool {
	key = strings.TrimSpace(key)
	return strings.HasPrefix(key, "/") || strings.HasPrefix(key, "~") ||
		filepath.IsAbs(key) || filepath.VolumeName(key) != ""
}

// applyUserPreferences overlays one preferences object onto the merged
// settings with the same per-key semantics the local file gets from mergeJSON:
// scalars override when set, review profiles merge by name, and the summary
// provider goes through SetProvider so a provider switch drops a model that
// belonged to the old provider.
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
	if prefs.Enabled != nil {
		settings.Enabled = *prefs.Enabled
	}
	if prefs.CheckpointRemote != nil {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = map[string]any{}
		}
		// Written as the same map shape GetCheckpointRemote already decodes,
		// rather than the struct: that reader type-asserts map[string]any, and
		// handing it a *CheckpointRemoteConfig would make it return nil — a
		// configured destination silently reading as unconfigured.
		settings.StrategyOptions["checkpoint_remote"] = map[string]any{
			"provider": prefs.CheckpointRemote.Provider,
			"repo":     prefs.CheckpointRemote.Repo,
		}
	}
}

// UserTierSetsCheckpointRemote reports whether the effective checkpoint_remote
// came from the user settings file.
//
// This is the user-tier half of the ownership question CheckpointRemoteIsLocalOnly
// answers for .entire/settings.local.json: "did this developer choose this
// destination, or did they inherit it from a repository they cloned?" The user
// file needs no probe to answer it — a repository cannot deliver content to
// ~/.config, so a destination found there is the developer's by construction.
func UserTierSetsCheckpointRemote(ctx context.Context, worktreeRoot string) bool {
	overlay := loadUserOverlay(ctx)
	if overlay.preferences != nil && overlay.preferences.CheckpointRemote != nil {
		return true
	}
	for _, prefs := range overlay.repoPreferences(ctx, worktreeRoot) {
		if prefs.CheckpointRemote != nil {
			return true
		}
	}
	return false
}

// UserTierConfiguresRepo reports whether the user settings file says anything
// about this repository — a matching `repos` entry, or a machine-wide
// `enabled`.
//
// IsSetUpAny uses this. Without it, a linked worktree of a repository enabled
// only through this tier has no .entire directory of its own, reads as "never
// set up", and every hook becomes a silent no-op — the exact failure the
// IsSetUpAny guard exists to prevent, reached through a different door.
func UserTierConfiguresRepo(ctx context.Context, worktreeRoot string) bool {
	overlay := loadUserOverlay(ctx)
	if overlay.preferences != nil && overlay.preferences.Enabled != nil {
		return true
	}
	return len(overlay.repoPreferences(ctx, worktreeRoot)) > 0
}

// applyUserTier applies the machine-wide preferences and then this
// repository's `repos` entries, recording anything dropped on the settings for
// the consumer to report.
//
// Called between the clone-preferences layer and the local-file merge, which
// is what puts the user tier above a preference the developer set for the
// clone and below one they set for this worktree. The local file keeps the
// last word for now; demoting it is a separate change, because doing it here
// would alter behaviour for every existing settings.local.json in the same
// commit that introduces the tier.
func applyUserTier(ctx context.Context, settings *EntireSettings, worktreeRoot string) {
	overlay := loadUserOverlay(ctx)
	owned := userPromptOwnership{profiles: map[string]bool{}}

	apply := func(prefs *UserPreferences) {
		if prefs == nil {
			return
		}
		applyUserPreferences(settings, prefs)
		owned.note(prefs)
	}

	apply(overlay.preferences)
	for _, prefs := range overlay.repoPreferences(ctx, worktreeRoot) {
		apply(prefs)
	}
	settings.userPromptOwnership = owned
	settings.userLayerRejections = overlay.rejections
}

// userPromptOwnership records which agent instruction fields came from the
// user settings file.
//
// enforceAgentPromptTrust drops any instruction field it cannot attribute to a
// developer-owned layer, and it knew about exactly two: a verified
// settings.local.json and clone preferences. The user file is developer-owned
// by a stronger argument than either — a repository cannot deliver content to
// ~/.config at all — but an unrecognised source is indistinguishable from an
// untrusted one, so investigate.always_prompt and every review profile task or
// prompt set there were silently dropped with a reason naming two files the
// developer had not used.
type userPromptOwnership struct {
	investigate bool
	profiles    map[string]bool
}

func (o *userPromptOwnership) note(prefs *UserPreferences) {
	if prefs.Investigate != nil {
		o.investigate = true
	}
	for name := range prefs.ReviewProfiles {
		if o.profiles == nil {
			o.profiles = map[string]bool{}
		}
		o.profiles[name] = true
	}
}

func (o *userPromptOwnership) ownsProfile(name string) bool {
	return o.profiles[name]
}

// UserLayerRejections reports the user-settings preference blocks (or this
// repository's repos entries) that Load dropped, one human-readable line each,
// or nil.
//
// Consumers that would have applied a dropped block should surface these,
// because it is the only signal that a setting the user can see in their own
// file is not in effect. Note what is NOT here: a failure in the block
// usersettings decodes itself fails the whole file, since that block names an
// executable — this list is only for the fail-open preference blocks.
func (s *EntireSettings) UserLayerRejections() []string {
	if s == nil {
		return nil
	}
	return s.userLayerRejections
}
