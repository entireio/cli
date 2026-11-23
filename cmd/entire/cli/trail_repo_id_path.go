package cli

import (
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func trailRepoBasePath(repoID string) (string, error) {
	path, err := api.RepoChangesPath(repoID)
	if err != nil {
		return "", fmt.Errorf("resolve Change route: %w", err)
	}
	return path, nil
}

// trailRepoIDNumberPath is a change on the repository-ID route
// (/repos/{repo_id}/changes/{number}), shared by all forges.
func trailRepoIDNumberPath(repoID string, number int) (string, error) {
	base, err := trailRepoBasePath(repoID)
	if err != nil {
		return "", err
	}
	return trailNumberPathForBase(base, number), nil
}

// trailRepoIDPath is a subresource of the selected change on the
// repository-ID route.
func trailRepoIDPath(selected *trailWorkingContext, suffix string) (string, error) {
	path, err := trailRepoIDNumberPath(selected.RepoID, selected.Work.Number)
	if err != nil {
		return "", err
	}
	return path + suffix, nil
}
