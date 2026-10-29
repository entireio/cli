package cli

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// trailRepoIDPath is a subresource of the selected change on the
// repository-ID route, the only route the cell serves runner launches, review
// re-runs, native branch updates, timelines, sessions, and stacks on.
func trailRepoIDPath(selected *trailWorkingContext, suffix string) (string, error) {
	repoID := strings.TrimSpace(selected.RepoID)
	if repoID == "" {
		return "", errors.New("cannot address this trail without its repository ID")
	}
	return "/api/v1/repos/" + url.PathEscape(repoID) + "/trails/" + strconv.Itoa(selected.Work.Number) + suffix, nil
}
