//go:build windows

package testutil

import (
	"os"
	"path/filepath"
)

// linkRepo writes a repo.txt file containing the repo path instead of
// creating a symlink, because Windows symlinks require elevated privileges
// or Developer Mode.
func linkRepo(repoDir, artifactDir string) error {
	return os.WriteFile(filepath.Join(artifactDir, "repo.txt"), []byte(repoDir), 0o644)
}
