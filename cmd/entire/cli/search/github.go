// Package search provides search functionality via the Entire search service.
package search

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseGitHubRemote extracts owner and repo from a GitHub remote URL.
// Supports SCP-style SSH (git@github.com:owner/repo.git),
// ssh:// URLs (ssh://git@github.com/owner/repo.git),
// and HTTPS (https://github.com/owner/repo.git).
func ParseGitHubRemote(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", errors.New("empty remote URL")
	}

	var path string

	// SCP-style SSH: git@github.com:owner/repo.git
	// Distinguished from ssh:// URLs by having no scheme and a colon before the path.
	if strings.HasPrefix(remoteURL, "git@") && !strings.Contains(remoteURL, "://") {
		idx := strings.Index(remoteURL, ":")
		if idx < 0 {
			return "", "", fmt.Errorf("invalid SSH remote URL: %s", remoteURL)
		}
		host := remoteURL[len("git@"):idx]
		if host != "github.com" {
			return "", "", fmt.Errorf("remote is not a GitHub repository (host: %s)", host)
		}
		path = remoteURL[idx+1:]
	} else {
		// URL format: https://, ssh://, git://
		u, parseErr := url.Parse(remoteURL)
		if parseErr != nil {
			return "", "", fmt.Errorf("parsing remote URL: %w", parseErr)
		}
		host := u.Hostname()
		if host != "github.com" {
			return "", "", fmt.Errorf("remote is not a GitHub repository (host: %s)", host)
		}
		path = strings.TrimPrefix(u.Path, "/")
	}

	// Remove .git suffix
	path = strings.TrimSuffix(path, ".git")

	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("could not extract owner/repo from remote URL: %s", remoteURL)
	}

	return parts[0], parts[1], nil
}
