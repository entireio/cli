package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// OriginRepoSlug derives the forge-qualified slug a dispatch addresses from a
// git remote URL: gh/<owner>/<repo> for github.com and entire://…/gh/… remotes,
// et/<project>/<repo> for entire://…/et/… remotes. The forge comes from the URL
// itself (its host, or the path token on an entire:// URL) rather than being
// assumed, so an Entire-native checkout dispatches as the native repo and a
// same-named GitHub mirror cannot answer for it. Any other host is an error:
// Entire holds no checkpoints for it, and the message names the --repos
// shapes so the user can address a repo explicitly.
func OriginRepoSlug(remoteURL string) (string, error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", errors.New("empty remote URL")
	}
	info, err := gitremote.ParseURL(remoteURL)
	if err != nil {
		return "", fmt.Errorf("parsing remote URL: %w", err)
	}
	if !gitremote.IsForgePathToken(info.Forge) {
		return "", fmt.Errorf("origin remote is not a GitHub or Entire-native repository (host: %s); expected %s", info.CanonicalHost(), RepoSlugShapes)
	}
	if strings.Contains(info.Repo, "/") {
		return "", fmt.Errorf("remote path has extra segments beyond owner/repo: %s", gitremote.RedactURL(remoteURL))
	}
	return info.Forge + "/" + info.Owner + "/" + info.Repo, nil
}
