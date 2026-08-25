package repopolicy

import (
	"bytes"
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

// ReadRepoActivation reads the repository's own settings files. It is the
// leaf-level twin of settings.IsSetUpAndEnabled: a project settings.json
// (default enabled) or a settings.local.json with an explicit "enabled" key
// counts as repo-level configuration; the local value wins. Malformed JSON
// or a non-boolean "enabled" is an error so callers fail closed.
func ReadRepoActivation(worktreeRoot string) (RepoActivation, error) {
	var activation RepoActivation
	for i, name := range repoSettingsFiles {
		path := filepath.Join(worktreeRoot, ".entire", name) // entire-join-ok: repo configuration lives in the literal worktree, never runtime-routed
		data, err := os.ReadFile(path)                       //nolint:gosec // fixed repo-relative configuration path
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return RepoActivation{}, fmt.Errorf("reading %s: %w", path, err)
		}
		enabled, err := enabledFromSettingsData(data)
		if err != nil {
			return RepoActivation{}, fmt.Errorf("%s: %w", path, err)
		}
		isProject := i == 0
		if isProject || enabled != nil {
			activation.Configured = true
		}
		switch {
		case enabled != nil:
			activation.Enabled = *enabled
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
