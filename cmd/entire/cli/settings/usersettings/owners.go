package usersettings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// GlobalEnableOwnersKey is the top-level list `entire enable --global --owner`
// adds to: owners whose repositories' checkpoints global enable pushes
// without asking.
const GlobalEnableOwnersKey = "global_enable_owners"

// ownerTransports are the transports a forge serves a repository over. A
// file:// URL, a local path or a remote helper names no forge owner, however
// its path is spelled.
var ownerTransports = []string{
	gitremote.ProtocolSSH, gitremote.ProtocolHTTPS, gitremote.ProtocolHTTP,
	gitremote.ProtocolGit, gitremote.ProtocolEntire,
}

var (
	ownerHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	ownerNamePattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)
)

// githubHostAsOwner is GitHub's host, which push URLs normalize to its forge
// token, so an entry spelled with the host is stored as the token.
const githubHostAsOwner = "github.com"

// NormalizeOwner reduces a push URL to the owner key the owner list uses: the
// same forge-or-host prefix as NormalizeOrigin, then the owner. A GitLab
// subgroup belongs to its top-level group (gitlab.com/grp for grp/sub/repo).
// "" when the URL names no forge owner, so it matches no entry.
func NormalizeOwner(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	info, err := gitremote.ParseURL(rawURL)
	if err != nil || info == nil || !slices.Contains(ownerTransports, info.Protocol) {
		return ""
	}
	prefix, ok := forgePrefix(info)
	if !ok || info.Owner == "" || info.Repo == "" {
		return ""
	}
	owner, err := ValidateOwner(prefix + "/" + info.Owner)
	if err != nil {
		return ""
	}
	return owner
}

// forgePrefix is the first segment of a `repos` or owner key for info: the
// Entire forge token when there is one, otherwise the canonical host.
func forgePrefix(info *gitremote.Info) (string, bool) {
	prefix := info.Forge
	if !gitremote.IsForgePathToken(prefix) {
		prefix = info.CanonicalHost()
	}
	return prefix, prefix != ""
}

// ValidateOwner checks an owner list entry and returns it in the form push
// URLs normalize to: lowercase `<forge token or host>/<owner>`, with GitHub's
// host spelled as its forge token. Anything else — a URL, a repository, a
// subgroup, a wildcard — is refused, because the list is matched exactly.
func ValidateOwner(entry string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(entry))
	prefix, owner, found := strings.Cut(normalized, "/")
	if !found || strings.Contains(owner, "/") {
		return "", fmt.Errorf("owner %q must be <forge>/<owner>, such as gh/entireio or gitlab.com/group", entry)
	}
	if prefix == githubHostAsOwner {
		prefix = gitremote.ForgeGitHub
	}
	if !gitremote.IsForgePathToken(prefix) && !ownerHostPattern.MatchString(prefix) {
		return "", fmt.Errorf("owner %q: %q is not a forge (gh, et) or a host name", entry, prefix)
	}
	if !ownerNamePattern.MatchString(owner) || strings.Trim(owner, ".") == "" {
		return "", fmt.Errorf("owner %q: %q is not an owner name", entry, owner)
	}
	return prefix + "/" + owner, nil
}

// AddGlobalEnableOwners adds owners, already validated, to the owner list,
// keeping the entries already there and their order, and returns the list.
func AddGlobalEnableOwners(ctx context.Context, owners []string) ([]string, error) {
	var list []string
	_, err := Modify(ctx, func(blocks map[string]json.RawMessage) error {
		if raw, ok := blocks[GlobalEnableOwnersKey]; ok && !IsJSONNull(raw) {
			if err := json.Unmarshal(raw, &list); err != nil {
				return errors.New(GlobalEnableOwnersKey + " is not a list of owners; not changing it")
			}
		}
		for _, owner := range owners {
			if !slices.Contains(list, owner) {
				list = append(list, owner)
			}
		}
		encoded, err := json.Marshal(list)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", GlobalEnableOwnersKey, err)
		}
		blocks[GlobalEnableOwnersKey] = encoded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}
