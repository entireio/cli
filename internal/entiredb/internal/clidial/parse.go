package clidial

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseRepoArg splits a positional repo argument into its data-plane cluster
// host and the repo path. Accepted forms:
//
//	cluster.example/<prefix>/<path>
//	entire://cluster.example/<prefix>/<path>
//
// The path must carry an explicit surface prefix — "et/" or "git/" for native
// repos, "gh/" for GitHub mirrors. The prefix names the data-plane lookup path
// and the audience of the repo-scoped JWT the resolve call mints, so it can't
// be guessed: an unprefixed "<owner>/<repo>" is rejected here rather than
// surfacing later as an opaque token-exchange failure. A pasted
// `entire://cluster/git/<owner>/<repo>` clone URL already carries the prefix
// and round-trips unchanged.
func ParseRepoArg(s string) (cluster, repoPath string, err error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return "", "", errors.New("repo argument is empty")
	}

	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", fmt.Errorf("parse %q: %w", raw, perr)
		}
		if u.Scheme != "entire" {
			return "", "", fmt.Errorf("unsupported scheme %q (only entire:// is accepted)", u.Scheme)
		}
		if u.Host == "" {
			return "", "", fmt.Errorf("invalid repo URL %q: missing host", raw)
		}
		path := strings.Trim(u.Path, "/")
		if path == "" {
			return "", "", fmt.Errorf("invalid repo URL %q: missing repo path", raw)
		}
		if err := validateRepoSurfacePrefix(path); err != nil {
			return "", "", err
		}
		return u.Host, path, nil
	}

	cluster, repoPath, ok := strings.Cut(raw, "/")
	if !ok || cluster == "" || repoPath == "" {
		return "", "", fmt.Errorf("invalid repo argument %q: expected <cluster>/<prefix>/<path>", raw)
	}
	if err := validateRepoSurfacePrefix(repoPath); err != nil {
		return "", "", err
	}
	return cluster, repoPath, nil
}

// validateRepoSurfacePrefix rejects repo paths without an explicit surface
// prefix. Mirrors the server's hasRepoSurfacePrefix check so the failure lands
// at the CLI boundary with an actionable message instead of as a token mint
// error.
func validateRepoSurfacePrefix(repoPath string) error {
	if strings.HasPrefix(repoPath, "et/") ||
		strings.HasPrefix(repoPath, "git/") ||
		strings.HasPrefix(repoPath, "gh/") {
		return nil
	}
	return fmt.Errorf("repo path %q must start with a surface prefix: %q (native), %q (legacy native), or %q (GitHub mirror) — e.g. et/%s",
		repoPath, "et/", "git/", "gh/", repoPath)
}
