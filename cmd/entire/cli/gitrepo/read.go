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

// nativeGitReadsEnvVar forces every migrated local read back onto native Git,
// so a go-git misbehavior in the field can be worked around without a release.
const nativeGitReadsEnvVar = "ENTIRE_NATIVE_GIT_READS"

// ReadsNeedNativeGit reports whether a local ref or commit read must use native
// Git instead of go-git. It keeps explicit store selectors
// (NativeReadSelectorEnvVars) and CLI-backed reference stores on the native
// path, and ENTIRE_NATIVE_GIT_READS forces native for every caller. Detect
// reftable before opening a repository: even opening its adapter performs a
// native reference lookup. Failed discovery also stays native, never guessing
// a CWD repository. A GIT_DIR naming the discovered Git directory selects the
// store go-git would open anyway; Git exports exactly that to hooks in linked
// worktrees.
//
// Caller contract: the gate covers ref and object reads only. Callers must not
// read the index, attributes, or pathspecs on the go-git path; GIT_INDEX_FILE,
// GIT_ATTR_* and pathspec variables are deliberately not consulted. The guard
// test in read_guard_test.go lists the approved call sites.
func ReadsNeedNativeGit(ctx context.Context) bool {
	if os.Getenv(nativeGitReadsEnvVar) != "" {
		return true
	}
	for _, key := range NativeReadSelectorEnvVars() {
		if key != "GIT_DIR" && os.Getenv(key) != "" {
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
	if gitDir := os.Getenv("GIT_DIR"); gitDir != "" {
		// Relative, symlinked, and case-folded spellings of the discovered
		// directory match; an unreadable GIT_DIR never does.
		same, err := metadataDirectoriesIdentifySameFile(gitDir, metadata.GitDir)
		if err != nil || !same {
			return true
		}
	}
	reftable, err := inspectRepoUsesReftable(metadata.GitDir, metadata.CommonDir)
	return err != nil || reftable
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
