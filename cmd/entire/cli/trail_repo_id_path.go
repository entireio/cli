package cli

import (
	"errors"
	"net/url"
	"strings"
)

// trailRepoIDNumberPath is a change on the repository-ID route, the only route
// the cell serves merges, review re-runs, native branch updates, and CI pull
// requests on.
func trailRepoIDNumberPath(repoID string, number int) (string, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return "", errors.New("cannot address this trail without its repository ID")
	}
	return trailNumberPathForBase("/api/v1/repos/"+url.PathEscape(repoID)+"/trails", number), nil
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
