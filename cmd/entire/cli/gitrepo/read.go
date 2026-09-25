package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ReadsNeedNativeGit keeps explicit store selectors and CLI-backed reference
// stores on the native read path. Detect reftable before opening a repository:
// even opening its adapter performs a native reference lookup.
// Failed discovery also stays native, never guessing a CWD repository.
// A GIT_DIR naming the discovered Git directory selects the store go-git would
// open anyway; Git exports exactly that to hooks in linked worktrees.
// GIT_INDEX_FILE is intentionally absent: these reads never consult the index.
func ReadsNeedNativeGit(ctx context.Context) bool {
	for _, key := range []string{
		"GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return true
	}
	metadata, err := ResolveWorktreeMetadata(root)
	if err != nil {
		return true
	}
	if gitDir := os.Getenv("GIT_DIR"); gitDir != "" && !sameDirectory(gitDir, metadata.GitDir) {
		return true
	}
	reftable, err := inspectRepoUsesReftable(metadata.GitDir, metadata.CommonDir)
	return err != nil || reftable
}

// sameDirectory compares by file identity, so relative, symlinked, and
// case-folded spellings of one directory match. Unreadable paths never match.
func sameDirectory(a, b string) bool {
	aInfo, err := os.Stat(a) //nolint:gosec // identity comparison only; nothing is read or written through the Git-selected path.
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false
	}
	return aInfo.IsDir() && os.SameFile(aInfo, bInfo)
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
