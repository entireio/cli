// Package search provides search functionality via the Entire search service.
package search

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// ParseGitHubRemote extracts owner and repo from a git remote URL that resolves
// to GitHub. It accepts direct GitHub remotes (SCP-style SSH, ssh://, and
// https://) as well as Entire mirror remotes (entire://host/gh/owner/repo),
// whose forge prefix maps back to github.com. Remotes resolving to any other
// host, or whose path holds extra segments beyond owner/repo, are rejected.
func ParseGitHubRemote(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", errors.New("empty remote URL")
	}
	info, err := gitremote.ParseURL(remoteURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing remote URL: %w", err)
	}
	if host := info.CanonicalHost(); host != "github.com" {
		return "", "", fmt.Errorf("remote is not a GitHub repository (host: %s)", host)
	}
	if strings.Contains(info.Repo, "/") {
		return "", "", fmt.Errorf("remote path has extra segments beyond owner/repo: %s", gitremote.RedactURL(remoteURL))
	}
	return info.Owner, info.Repo, nil
}
