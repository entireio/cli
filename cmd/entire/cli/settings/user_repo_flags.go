package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// SetRepoEnabled writes `enabled` into this repository's `repos` entries in
// the user settings file, which is how `entire enable` and `entire disable`
// turn one repository on or off for every clone and worktree of it when it
// was on only through global enable. It returns the key that names the
// repository there.
func SetRepoEnabled(ctx context.Context, worktreeRoot string, enabled bool) (string, error) {
	return setRepoFlag(ctx, worktreeRoot, "enabled", enabled)
}

// SetRepoPushConsent writes `push_checkpoints` into this repository's `repos`
// entries: the developer's answer to whether session checkpoints captured
// there only through global enable may be pushed. It returns the key that
// names the repository there.
func SetRepoPushConsent(ctx context.Context, worktreeRoot string, consent bool) (string, error) {
	return setRepoFlag(ctx, worktreeRoot, "push_checkpoints", consent)
}

// setRepoFlag sets field on EVERY `repos` entry that names this repository —
// several can (an origin with two URLs, an origin and a path), and leaving one
// at the old value would keep it in force through key order. With no matching
// entry it creates one under the first origin key, or the main worktree's path
// when the repository has no usable origin. Sibling keys of each entry are
// kept as written; entries for other repositories are not decoded at all.
func setRepoFlag(ctx context.Context, worktreeRoot, field string, value bool) (string, error) {
	if worktreeRoot == "" {
		return "", errors.New("no repository to configure")
	}
	// Uncached: a write is rare, and the cache is cleared after it anyway.
	originKeys, _, err := usersettings.OriginKeys(ctx, worktreeRoot)
	if err != nil {
		originKeys = nil // unnormalizable or unreadable: key by path, as repoPreferences matches
	}
	fallbackKey, err := repoEntryKey(ctx, worktreeRoot, originKeys)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding %s: %w", field, err)
	}

	var written string
	_, err = usersettings.Modify(ctx, func(blocks map[string]json.RawMessage) error {
		repos, err := decodeReposForWrite(blocks[userReposBlock])
		if err != nil {
			return err
		}
		var matched []string
		for name := range repos {
			if repoKeyMatches(ctx, name, originKeys, worktreeRoot) {
				matched = append(matched, name)
			}
		}
		slices.Sort(matched)
		if len(matched) == 0 {
			matched = []string{fallbackKey}
		}
		for _, name := range matched {
			if err := setRepoEntryField(repos, name, field, encoded); err != nil {
				return err
			}
		}
		out, err := json.Marshal(repos)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", userReposBlock, err)
		}
		blocks[userReposBlock] = out
		written = matched[0]
		return nil
	})
	ClearOriginKeyCache()
	if err != nil {
		return "", err //nolint:wrapcheck // usersettings names the file and the reason
	}
	return written, nil
}

// decodeReposForWrite decodes the `repos` block, absent or null being empty.
// Entries stay raw, so ones this write does not touch are kept as written.
func decodeReposForWrite(raw json.RawMessage) (map[string]json.RawMessage, error) {
	repos := map[string]json.RawMessage{}
	if raw == nil || usersettings.IsJSONNull(raw) {
		return repos, nil
	}
	if err := json.Unmarshal(raw, &repos); err != nil || repos == nil {
		return nil, fmt.Errorf("%s is not an object; not changing it", userReposBlock)
	}
	return repos, nil
}

// setRepoEntryField sets field in repos[name], creating the entry and keeping
// its other keys. An entry that is not an object is refused, not replaced.
func setRepoEntryField(repos map[string]json.RawMessage, name, field string, value json.RawMessage) error {
	entry := map[string]json.RawMessage{}
	if raw, ok := repos[name]; ok && !usersettings.IsJSONNull(raw) {
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			return fmt.Errorf("%s[%q] is not an object; not changing it", userReposBlock, name)
		}
	}
	entry[field] = value
	out, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding %s[%q]: %w", userReposBlock, name, err)
	}
	repos[name] = out
	return nil
}

// repoEntryKey is the key a new `repos` entry for this repository gets: its
// first origin key, or else the path of its main worktree — not this one, so
// the entry reads the same from every worktree and PathNamesThisClone matches
// it from all of them.
func repoEntryKey(ctx context.Context, worktreeRoot string, originKeys []string) (string, error) {
	if len(originKeys) > 0 {
		return originKeys[0], nil
	}
	common, err := gitdir.CommonDirForPath(ctx, worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolving the repository to key it by path: %w", err)
	}
	if filepath.Base(common) == ".git" {
		return filepath.ToSlash(filepath.Dir(common)), nil
	}
	// A bare repository's worktrees have no main checkout to name.
	return filepath.ToSlash(filepath.Clean(worktreeRoot)), nil
}

// UserReposEnableAny reports whether any `repos` entry in the user settings
// file sets `enabled: true`, for any repository.
func UserReposEnableAny(ctx context.Context) bool {
	overlay := loadUserOverlay(ctx)
	for _, raw := range overlay.repos {
		prefs, err := decodeUserPreferences(raw)
		if err == nil && prefs != nil && prefs.Enabled != nil && *prefs.Enabled {
			return true
		}
	}
	return false
}
