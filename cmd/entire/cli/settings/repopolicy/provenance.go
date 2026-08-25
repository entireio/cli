package repopolicy

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
)

const (
	projectSettingsRelative = ".entire/settings.json"
	localSettingsRelative   = ".entire/settings.local.json"
)

// SettingsProvenance contains only canonical, worktree-rooted settings paths
// and whether each path is safe to read for hook-time policy. Project content
// may provide declarative configuration; the local layer additionally must be
// verified untracked.
type SettingsProvenance struct {
	ProjectPath     string
	ProjectPathSafe bool
	ProjectVerified bool
	LocalPath       string
	LocalPathSafe   bool
	LocalVerified   bool
}

// VerifySettingsProvenance lstat-verifies every worktree-relative path
// component from the canonical root. Unsafe, missing, tracked, or
// unverifiable files are reported as unverified rather than followed.
func VerifySettingsProvenance(ctx context.Context, repository Repository) (SettingsProvenance, error) {
	if err := validateRepository(repository); err != nil {
		return SettingsProvenance{}, err
	}
	provenance := SettingsProvenance{
		ProjectPath: filepath.Join(repository.WorktreeRoot, filepath.FromSlash(projectSettingsRelative)),
		LocalPath:   filepath.Join(repository.WorktreeRoot, filepath.FromSlash(localSettingsRelative)),
	}
	projectErr := verifyPathComponents(repository.WorktreeRoot, filepath.FromSlash(projectSettingsRelative), false)
	provenance.ProjectPathSafe = projectErr == nil
	provenance.ProjectVerified = provenance.ProjectPathSafe
	if projectErr != nil && !errors.Is(projectErr, fs.ErrNotExist) {
		// Unsafe path shapes fail toward untrusted provenance, not toward an
		// attacker-controlled diagnostic containing a resolved target.
		provenance.ProjectVerified = false
	}

	localErr := verifyPathComponents(repository.WorktreeRoot, filepath.FromSlash(localSettingsRelative), false)
	if localErr == nil {
		provenance.LocalPathSafe = true
		tracked, err := gitPathTracked(ctx, repository.WorktreeRoot, localSettingsRelative)
		provenance.LocalVerified = err == nil && !tracked
	}
	return provenance, nil
}
