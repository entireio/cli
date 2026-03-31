//go:build !windows

package testutil

import (
	"os"
	"path/filepath"
)

// linkRepo creates a symlink from the artifact directory to the test repo
// for easy post-test inspection when E2E_KEEP_REPOS is set.
func linkRepo(repoDir, artifactDir string) error {
	return os.Symlink(repoDir, filepath.Join(artifactDir, "repo"))
}
