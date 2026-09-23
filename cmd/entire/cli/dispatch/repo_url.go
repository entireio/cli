package dispatch

import (
	"regexp"
	"strings"
)

var (
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// githubRepoURL links a repo name the API or the origin remote produced to
// its github.com page. A gh/ slug and a bare owner/repo both link: the API
// still spells GitHub repos bare, and the origin remote is GitHub by
// construction. Any other forge, or an unsafe name, gets no link. Never
// called with user input; SplitRepoSlug refuses bare names there.
func githubRepoURL(fullName string) string {
	name := strings.TrimSpace(fullName)
	if forge, slugName, ok := SplitRepoSlug(name); ok {
		if forge != GitHubForge {
			return ""
		}
		name = slugName
	}
	owner, repoName, ok := strings.Cut(name, "/")
	if !ok || strings.Contains(repoName, "/") || repoName == "." || repoName == ".." {
		return ""
	}
	if !githubOwnerPattern.MatchString(owner) || !githubRepoPattern.MatchString(repoName) {
		return ""
	}
	return "https://github.com/" + owner + "/" + repoName
}
