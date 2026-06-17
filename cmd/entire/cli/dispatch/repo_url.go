package dispatch

import (
	"regexp"
	"strings"
)

var (
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func githubRepoURL(fullName string) string {
	owner, repoName, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	if !ok || strings.Contains(repoName, "/") || repoName == "." || repoName == ".." {
		return ""
	}
	if !githubOwnerPattern.MatchString(owner) || !githubRepoPattern.MatchString(repoName) {
		return ""
	}
	return "https://github.com/" + owner + "/" + repoName
}
