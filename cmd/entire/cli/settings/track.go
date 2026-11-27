package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// TrackKey is the user settings block that narrows global enable to the
// repositories the developer wants tracked. It is read only from the user
// settings file: a repository cannot deliver content to ~/.config, so nothing
// a repository commits can widen or narrow it.
const TrackKey = "track"

// TrackRule is the `track` block. The default (every field empty) tracks
// every repository. Deny always wins; with Orgs set, only repositories whose
// origin lies in one of them, or that Repos (or a `repos` entry with
// `enabled: true`) names, are tracked. Entries are normalized origin keys
// (forge/owner/repo, host/owner/repo) or, for repositories with no remote,
// absolute paths; org entries are forge/owner or host/owner.
type TrackRule struct {
	Orgs  []string `json:"orgs,omitempty"`
	Repos []string `json:"repos,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// IsZero reports whether the rule is the allow-all default.
func (r TrackRule) IsZero() bool {
	return len(r.Orgs) == 0 && len(r.Repos) == 0 && len(r.Deny) == 0
}

// LoadTrackRule reads the `track` block from the user settings file. An
// absent block is the allow-all default.
func LoadTrackRule(ctx context.Context) (TrackRule, error) {
	us, err := usersettings.Load(ctx)
	if err != nil {
		return TrackRule{}, err //nolint:wrapcheck // usersettings names the file and the reason
	}
	raw, ok := us.Block(TrackKey)
	if !ok {
		return TrackRule{}, nil
	}
	return decodeTrackRule(raw)
}

func decodeTrackRule(raw json.RawMessage) (TrackRule, error) {
	var rule TrackRule
	if raw == nil || usersettings.IsJSONNull(raw) {
		return rule, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rule); err != nil {
		return TrackRule{}, fmt.Errorf("%s: %w", TrackKey, err)
	}
	return rule, nil
}

// ModifyTrackRule applies fn to the `track` block and writes it back, keeping
// every other block. An empty rule removes the block.
func ModifyTrackRule(ctx context.Context, fn func(*TrackRule)) (string, error) {
	path, err := usersettings.Modify(ctx, func(blocks map[string]json.RawMessage) error {
		rule, err := decodeTrackRule(blocks[TrackKey])
		if err != nil {
			return err
		}
		fn(&rule)
		if rule.IsZero() {
			delete(blocks, TrackKey)
			return nil
		}
		out, err := json.Marshal(rule)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", TrackKey, err)
		}
		blocks[TrackKey] = out
		return nil
	})
	ClearTrackCache()
	return path, err //nolint:wrapcheck // usersettings names the file and the reason
}

// NormalizeTrackOrg turns "github.com/entireio", "https://github.com/entireio"
// or "gh/entireio" into the key shape origin keys start with ("gh/entireio"),
// or "" when it names no owner.
func NormalizeTrackOrg(org string) string {
	org = strings.Trim(strings.TrimSpace(org), "/")
	if org == "" {
		return ""
	}
	for _, candidate := range []string{org + "/x", "https://" + org + "/x"} {
		if key := usersettings.NormalizeOrigin(candidate); key != "" {
			return strings.TrimSuffix(key, "/x")
		}
	}
	return ""
}

// NormalizeTrackRepo turns a repository reference into a track key: a path
// (absolute or ~) stays a path, anything else must normalize as an origin.
func NormalizeTrackRepo(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if isPathKey(ref) {
		return usersettings.ExpandTilde(ref) //nolint:wrapcheck // the message names the problem
	}
	for _, candidate := range []string{ref, "https://" + ref} {
		if key := usersettings.NormalizeOrigin(candidate); key != "" {
			return key, nil
		}
	}
	return "", fmt.Errorf("%q is neither an absolute path nor a repository like github.com/owner/repo", ref)
}

// trackResult is one RepoTrackable answer.
type trackResult struct {
	ok     bool
	reason string
}

var (
	trackMu     sync.Mutex
	trackByRoot = map[string]trackResult{}
)

// ClearTrackCache drops memoized RepoTrackable answers.
func ClearTrackCache() {
	trackMu.Lock()
	defer trackMu.Unlock()
	clear(trackByRoot)
}

// RepoTrackable reports whether global enable may record sessions and link
// commits in the repository at repoRoot, and why. First match wins:
//
//  1. a `repos` entry for it, or an honored settings.local.json, sets
//     enabled:false;
//  2. neither sets enabled:true and its committed .entire/settings.json sets
//     enabled:false;
//  3. track.deny names it;
//  4. the developer enabled it explicitly (a `repos` entry or an honored
//     settings.local.json with enabled:true): trackable, since a project
//     install overrides global enable and only track.deny excludes it;
//  5. track.orgs is set and neither its origin lies in one nor track.repos
//     names it;
//  6. otherwise it is trackable.
//
// Answers are cached per root for the process: a hook is short-lived, and the
// rule changes only through ModifyTrackRule, which clears the cache.
func RepoTrackable(ctx context.Context, repoRoot string) (bool, string) {
	key := filepath.Clean(repoRoot)
	trackMu.Lock()
	cached, ok := trackByRoot[key]
	trackMu.Unlock()
	if ok {
		return cached.ok, cached.reason
	}
	res := resolveTrackable(ctx, key)
	trackMu.Lock()
	trackByRoot[key] = res
	trackMu.Unlock()
	return res.ok, res.reason
}

func resolveTrackable(ctx context.Context, root string) trackResult {
	explicitlyEnabled, disabledReason := explicitEnablement(ctx, root)
	if disabledReason != "" {
		return trackResult{reason: disabledReason}
	}
	rule, err := LoadTrackRule(ctx)
	if err != nil {
		return trackResult{reason: "the track block in your user settings is invalid: " + err.Error()}
	}
	originKeys, err := originKeysCached(ctx, root)
	if err != nil {
		originKeys = nil
	}
	if matchesAnyRepoKey(ctx, rule.Deny, originKeys, root) {
		return trackResult{reason: "denied by track.deny"}
	}
	if explicitlyEnabled {
		return trackResult{ok: true, reason: "enabled in this repository"}
	}
	if len(rule.Orgs) == 0 {
		return trackResult{ok: true, reason: "every repository is tracked"}
	}
	if matchesAnyRepoKey(ctx, rule.Repos, originKeys, root) {
		return trackResult{ok: true, reason: "allowed by name"}
	}
	if org := matchingOrg(rule.Orgs, originKeys); org != "" {
		return trackResult{ok: true, reason: "in tracked org " + org}
	}
	return trackResult{reason: "not in a tracked org (" + strings.Join(rule.Orgs, ", ") + ")"}
}

// explicitEnablement reports whether the developer enabled the repository at
// root themselves, or why it is disabled. The developer's own `enabled`
// (a `repos` entry, then an honored settings.local.json) overrides the
// committed settings.json, as it does in the settings merge.
func explicitEnablement(ctx context.Context, root string) (enabled bool, disabledReason string) {
	overlay := loadUserOverlay(ctx)
	for _, prefs := range overlay.repoPreferences(ctx, root) {
		if prefs.Enabled != nil {
			if !*prefs.Enabled {
				return false, "disabled for this repository in your user settings (repos)"
			}
			enabled = true
		}
	}
	if local := localEnabledAt(ctx, root); local != nil {
		if !*local {
			return false, "disabled by this worktree's " + EntireSettingsLocalFile
		}
		enabled = true
	}
	if enabled {
		return true, ""
	}
	if committed, err := loadFromFile(filepath.Join(root, EntireSettingsFile)); err == nil && !committed.Enabled {
		return false, "disabled by this repository's " + EntireSettingsFile
	}
	return false, ""
}

// localEnabledAt is the `enabled` value of the settings.local.json in the
// worktree at root, or nil when the file is absent, unreadable, tracked by git
// (so not the developer's own), or carries no `enabled` key.
func localEnabledAt(ctx context.Context, root string) *bool {
	path := filepath.Join(root, EntireSettingsLocalFile)
	data, err := readConfined(path)
	if err != nil || classifyLocalSettings(ctx, path) == localTracked {
		return nil
	}
	var raw struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	return raw.Enabled
}

func matchesAnyRepoKey(ctx context.Context, keys, originKeys []string, root string) bool {
	return slices.ContainsFunc(keys, func(key string) bool {
		if !isPathKey(key) {
			if normalized, err := NormalizeTrackRepo(key); err == nil {
				key = normalized
			}
		}
		return repoKeyMatches(ctx, key, originKeys, root)
	})
}

// matchingOrg returns the first configured org one of originKeys lies in.
func matchingOrg(orgs, originKeys []string) string {
	for _, org := range orgs {
		prefix := NormalizeTrackOrg(org)
		if prefix == "" {
			continue
		}
		for _, key := range originKeys {
			if strings.HasPrefix(key, prefix+"/") {
				return org
			}
		}
	}
	return ""
}

// TracksKey reports whether the rule tracks the repository named key (a
// repository key as RepoKey returns it) on the rule alone: not denied, and
// either no org rule, allowed by name, or in an allowed org.
func (r TrackRule) TracksKey(key string) bool {
	if slices.Contains(r.Deny, key) {
		return false
	}
	return len(r.Orgs) == 0 || slices.Contains(r.Repos, key) || matchingOrg(r.Orgs, []string{key}) != ""
}

// InstallPastHomeFloor reports whether the developer asked for the
// repository at root themselves, so a lazy hook install may go there even
// outside the home directory: they enabled it (`entire enable` there, a
// `repos` entry or an honored settings.local.json) or allowed it by name
// (`entire track allow`, track.repos).
func InstallPastHomeFloor(ctx context.Context, root string) bool {
	if enabled, _ := explicitEnablement(ctx, root); enabled {
		return true
	}
	rule, err := LoadTrackRule(ctx)
	if err != nil {
		return false
	}
	originKeys, err := originKeysCached(ctx, root)
	if err != nil {
		originKeys = nil
	}
	return matchesAnyRepoKey(ctx, rule.Repos, originKeys, root)
}

// HookRepoTrackable is the agent-hook gate under global enable: with
// global_enable off it is always true (the tracking rule only narrows global
// enable), and otherwise it is RepoTrackable for the repository at root.
func HookRepoTrackable(ctx context.Context, root string) bool {
	if !UserTierEnablesEverywhere(ctx) {
		return true
	}
	ok, _ := RepoTrackable(ctx, root)
	return ok
}

// UserRepoKeys returns the keys of the `repos` block in the user settings
// file, sorted: the repositories the developer configured by name.
func UserRepoKeys(ctx context.Context) []string {
	overlay := loadUserOverlay(ctx)
	keys := make([]string, 0, len(overlay.repos))
	for key := range overlay.repos {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
