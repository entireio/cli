package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ReadsNeedNativeGit reports repository/object-store selectors that the
// filesystem storer does not interpret. Migrated CLI reads retain native Git
// for these explicit environments instead of silently reading another store.
// GIT_INDEX_FILE is intentionally absent: these reads never consult the index.
func ReadsNeedNativeGit() bool {
	for _, key := range []string{
		"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// CommitAtReference reads an exact reference and peels annotated tags to a
// commit. Unlike ResolveRevision, it does not accept revision expressions or
// abbreviations. Missing refs, missing objects, and non-commit targets remain
// errors; callers decide whether their workflow treats those as absence.
// Replace refs require native interpretation and are returned as unsupported
// objects so migrated callers can use their native compatibility path.
func CommitAtReference(ctx context.Context, repo *git.Repository, name plumbing.ReferenceName) (*object.Commit, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read commit at %s: %w", name, err)
	}
	ref, err := repo.Reference(name, true)
	if err != nil {
		return nil, fmt.Errorf("read reference %s: %w", name, err)
	}
	hash := ref.Hash()
	seen := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read commit at %s: %w", name, err)
		}
		if seen[hash.String()] {
			return nil, fmt.Errorf("tag cycle at %s: %w", name, object.ErrUnsupportedObject)
		}
		seen[hash.String()] = true
		_, replaceErr := repo.Reference(plumbing.ReferenceName("refs/replace/"+hash.String()), false)
		if replaceErr == nil {
			return nil, fmt.Errorf("reference %s has a replacement: %w", name, object.ErrUnsupportedObject)
		}
		if !errors.Is(replaceErr, plumbing.ErrReferenceNotFound) {
			return nil, fmt.Errorf("check replacement for %s: %w", name, replaceErr)
		}
		obj, err := repo.Object(plumbing.AnyObject, hash)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("read commit at %s: %w", name, ctxErr)
		}
		if err != nil {
			return nil, fmt.Errorf("read object at %s: %w", name, err)
		}
		switch obj := obj.(type) {
		case *object.Commit:
			return obj, nil
		case *object.Tag:
			hash = obj.Target
		default:
			return nil, fmt.Errorf("reference %s does not resolve to a commit: %w", name, object.ErrUnsupportedObject)
		}
	}
}
