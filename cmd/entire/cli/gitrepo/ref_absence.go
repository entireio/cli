package gitrepo

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
)

// ReferenceIsAbsent requires both loose storage and the reference reader to
// establish absence. A read failure must not authorize destructive cleanup.
func ReferenceIsAbsent(repo *git.Repository, refName plumbing.ReferenceName) (bool, error) {
	if err := refName.Validate(); err != nil {
		return false, fmt.Errorf("validate reference: %w", err)
	}
	// go-git falls back to packed refs after any loose-ref read error, so its
	// not-found result alone cannot distinguish a directory from absence.
	if storage, ok := repo.Storer.(*gitfilesystem.Storage); ok {
		info, statErr := storage.Filesystem().Lstat(refName.String())
		if statErr == nil {
			if info.IsDir() {
				return false, fmt.Errorf("ref %s is a directory", refName)
			}
			return false, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return false, fmt.Errorf("inspect ref %s: %w", refName, statErr)
		}
	}
	_, err := repo.Reference(refName, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ref %s: %w", refName, err)
	}
	return false, nil
}
