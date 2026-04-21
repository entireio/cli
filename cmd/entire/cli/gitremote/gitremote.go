// Package gitremote provides general-purpose git remote URL utilities:
// parsing, resolving, and redacting remote URLs. It has no dependency on
// checkpoint, strategy, or settings packages.
package gitremote

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

const (
	ProtocolSSH   = "ssh"
	ProtocolHTTPS = "https"
)

// Info holds the parsed components of a git remote URL.
type Info struct {
	Protocol string
	Host     string
	Owner    string
	Repo     string
}

// GetRemoteURL returns the URL configured for the named git remote.
func GetRemoteURL(ctx context.Context, remoteName string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", remoteName)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("remote %q not found", remoteName)
	}
	return strings.TrimSpace(string(output)), nil
}

// ParseURL parses a git remote URL (SSH SCP-style or HTTPS) into its components.
func ParseURL(rawURL string) (*Info, error) {
	rawURL = strings.TrimSpace(rawURL)

	if strings.Contains(rawURL, ":") && !strings.Contains(rawURL, "://") {
		parts := strings.SplitN(rawURL, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid SSH URL: %s", RedactURL(rawURL))
		}

		hostPart := parts[0]
		_, host, found := strings.Cut(hostPart, "@")
		if !found {
			host = hostPart
		}

		pathPart := strings.TrimSuffix(parts[1], ".git")
		owner, repo, err := splitOwnerRepo(pathPart)
		if err != nil {
			return nil, err
		}

		return &Info{Protocol: ProtocolSSH, Host: host, Owner: owner, Repo: repo}, nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %s", RedactURL(rawURL))
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("no protocol in URL: %s", RedactURL(rawURL))
	}

	pathPart := strings.TrimPrefix(u.Path, "/")
	owner, repo, err := splitOwnerRepo(pathPart)
	if err != nil {
		return nil, err
	}

	return &Info{Protocol: u.Scheme, Host: u.Hostname(), Owner: owner, Repo: repo}, nil
}

// RedactURL removes credentials and query parameters from a URL for safe logging.
// SCP-style SSH URLs (e.g., git@github.com:org/repo.git) are returned as-is
// since they contain no embedded credentials.
func RedactURL(rawURL string) string {
	// SCP-style SSH: user@host:path — no credentials to redact.
	if strings.Contains(rawURL, ":") && !strings.Contains(rawURL, "://") {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	u.RawQuery = ""
	return u.Scheme + "://" + u.Host + u.Path
}

// ExtractOwnerFromRemoteURL extracts the owner component from a git remote URL.
// Returns an empty string if the URL cannot be parsed.
func ExtractOwnerFromRemoteURL(rawURL string) string {
	info, err := ParseURL(rawURL)
	if err != nil {
		return ""
	}
	return info.Owner
}

// ResolveRemoteRepo returns the host, owner, and repo name for the given git remote.
// It parses the remote URL (SSH or HTTPS) and extracts the components.
// For example, git@github.com:org/my-repo.git returns ("github.com", "org", "my-repo").
func ResolveRemoteRepo(ctx context.Context, remoteName string) (host, owner, repo string, err error) {
	rawURL, err := GetRemoteURL(ctx, remoteName)
	if err != nil {
		return "", "", "", fmt.Errorf("get remote URL for %q: %w", remoteName, err)
	}
	info, err := ParseURL(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse remote URL: %w", err)
	}
	return info.Host, info.Owner, info.Repo, nil
}

func splitOwnerRepo(path string) (string, string, error) {
	path = strings.TrimSuffix(path, ".git")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from path: %s", path)
	}
	return parts[0], parts[1], nil
}
