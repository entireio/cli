package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// FetchRefFunc is a function that fetches a ref from the remote.
// Used as a dependency injection point so this package doesn't import cli.
type FetchRefFunc func(ctx context.Context) error

// GetV2MetadataTree resolves the v2 /main ref tree with fetch fallback.
// Follows the same pattern as getMetadataTree() in resume.go:
//  1. Treeless fetch → open fresh repo → read /main ref tree
//  2. Local ref lookup
//  3. Full fetch → read tree
//
// Takes fetch functions as dependencies to avoid importing the cli package.
// openRepoFn opens a fresh repository (needed after fetch to see new packfiles).
func GetV2MetadataTree(ctx context.Context, treelessFetchFn, fullFetchFn FetchRefFunc, openRepoFn func(context.Context) (*git.Repository, error)) (*object.Tree, *git.Repository, error) {
	refName := plumbing.ReferenceName(paths.V2MainRefName)

	if treelessFetchFn != nil {
		if fetchErr := treelessFetchFn(ctx); fetchErr == nil {
			freshRepo, repoErr := openRepoFn(ctx)
			if repoErr == nil {
				tree, treeErr := getV2RefTree(freshRepo, refName)
				if treeErr == nil {
					return tree, freshRepo, nil
				}
			}
		}
	}

	localRepo, repoErr := openRepoFn(ctx)
	if repoErr == nil {
		tree, err := getV2RefTree(localRepo, refName)
		if err == nil {
			return tree, localRepo, nil
		}
	}

	if fullFetchFn != nil {
		if fetchErr := fullFetchFn(ctx); fetchErr == nil {
			freshRepo, repoErr := openRepoFn(ctx)
			if repoErr == nil {
				tree, treeErr := getV2RefTree(freshRepo, refName)
				if treeErr == nil {
					return tree, freshRepo, nil
				}
			}
		}
	}

	return nil, nil, errors.New("v2 /main ref not available")
}

// getV2RefTree reads the tree from a custom ref (not a branch — no refs/heads/ prefix).
func getV2RefTree(repo *git.Repository, refName plumbing.ReferenceName) (*object.Tree, error) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("ref %s not found: %w", refName, err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit for ref %s: %w", refName, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for ref %s: %w", refName, err)
	}
	return tree, nil
}
