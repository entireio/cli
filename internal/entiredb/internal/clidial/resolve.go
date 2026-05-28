package clidial

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/entiredb/client"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
)

// ResolveRepoID accepts either a repo ULID (no slash) or a
// <cluster>/<org>/<repo> positional and returns the canonical ULID.
//
// Path input requires data-plane connectivity to the named cluster, since
// core has no path-to-ID lookup. ULID input skips the round-trip entirely.
func ResolveRepoID(cfg cliauth.Config, arg string) (string, error) {
	if !strings.Contains(arg, "/") {
		return arg, nil
	}
	cluster, repoPath, err := ParseRepoArg(arg)
	if err != nil {
		return "", err
	}
	var id string
	if err := ConnectForRepo(cfg, cluster, repoPath, func(_ *client.Client, repoID string) error {
		id = repoID
		return nil
	}); err != nil {
		return "", fmt.Errorf("resolve %s: %w", arg, err)
	}
	return id, nil
}
