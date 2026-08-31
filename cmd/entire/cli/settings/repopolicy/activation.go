package repopolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WorktreeRegistryRelative is the git-common-dir subtree holding per-worktree
// runtime data for globally tracked repositories.
const WorktreeRegistryRelative = "entire/worktree"

// RuntimeDir returns the git-common runtime directory for one worktree.
func RuntimeDir(repository Repository) string {
	return filepath.Join(repository.GitCommonDir, WorktreeRegistryRelative, repository.WorktreeKey)
}

var repoSettingsFiles = []string{"settings.json", "settings.local.json"}

// LocalSettingsVerdict is what the tracked-file probe knows about a
// .entire/settings.local.json: repository content, this developer's own file,
// or undecidable.
type LocalSettingsVerdict int

const (
	// LocalSettingsUnverifiable: the repository could not be read (index
	// error, cancelled context). The file's "enabled" still applies — losing
	// every local setting is the worse failure — but it never counts as the
	// developer's verified action (RepoActivation.LocalOverride stays false),
	// so it cannot bypass the user's exclude lists.
	LocalSettingsUnverifiable LocalSettingsVerdict = iota
	// LocalSettingsTracked: proven present in the index (or reached through a
	// symlink). Ignored wholesale — .gitignore does not protect an
	// already-committed path, and a clone must not force activation by
	// shipping a "local" file.
	LocalSettingsTracked
	// LocalSettingsOwn: verified untracked and a regular file, or no
	// repository to have cloned it from.
	LocalSettingsOwn
)

// ClassifyLocalSettings is the tracked-file probe for settings.local.json.
// The leaf cannot import the settings package that owns git access (cycle
// through paths), so settings installs the real probe at package
// initialization (git index + symlink check, memoized per process). The
// default stands in only for tests that link repopolicy alone: it rejects a
// symlinked .entire or file (the shape a clone can ship without the literal
// path ever appearing in the index) and otherwise trusts the file.
var ClassifyLocalSettings = func(_ context.Context, path string) LocalSettingsVerdict {
	for _, candidate := range []string{filepath.Dir(path), path} {
		if info, err := os.Lstat(candidate); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return LocalSettingsTracked
		}
	}
	return LocalSettingsOwn
}

// ReadRepoActivation reads the repository's own settings files. It is the
// leaf-level form of main's repo-level activation gate: a project settings.json
// (default enabled) or a settings.local.json with an explicit "enabled" key
// counts as repo-level configuration; the local value wins. A local file that
// is repository content (tracked, or reached through a symlink) is ignored
// wholesale, exactly as the merged loader ignores it: .gitignore does not
// protect an already-committed path, and a clone must not be able to force
// activation — or bypass the user's exclusions — by shipping one. Malformed
// JSON or a non-boolean "enabled" is an error so callers fail closed.
func ReadRepoActivation(ctx context.Context, worktreeRoot string) (RepoActivation, error) {
	var activation RepoActivation
	for i, name := range repoSettingsFiles {
		path := filepath.Join(worktreeRoot, ".entire", name) // entire-join-ok: repo configuration lives in the literal worktree, never runtime-routed
		isProject := i == 0
		data, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative configuration path
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return RepoActivation{}, fmt.Errorf("reading %s: %w", path, err)
		}
		// Probe only a file that exists: the probe opens the repository, which
		// is the expensive half, and most repos have no local file at all.
		verdict := LocalSettingsOwn
		if !isProject {
			if verdict = ClassifyLocalSettings(ctx, path); verdict == LocalSettingsTracked {
				continue
			}
		}
		enabled, err := enabledFromSettingsData(data)
		if err != nil {
			return RepoActivation{}, fmt.Errorf("%s: %w", path, err)
		}
		if isProject || enabled != nil {
			activation.Configured = true
		}
		switch {
		case enabled != nil:
			activation.Enabled = *enabled
			activation.LocalOverride = !isProject && verdict == LocalSettingsOwn
		case isProject:
			activation.Enabled = true // main's default: a project file without the key is enabled
		}
	}
	return activation, nil
}

func enabledFromSettingsData(data []byte) (*bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}
	value, ok := raw["enabled"]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil //nolint:nilnil // nil means the optional enabled field is absent
	}
	var enabled bool
	if err := json.Unmarshal(value, &enabled); err != nil {
		return nil, errors.New("enabled must be a boolean")
	}
	return &enabled, nil
}
