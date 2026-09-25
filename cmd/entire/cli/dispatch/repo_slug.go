package dispatch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// GitHubForge is the forge token entire.io gives GitHub repos.
const GitHubForge = "gh"

// NativeForge is the forge token of Entire-native repos.
const NativeForge = "et"

// RepoSlugShapes lists every --repos shape, for error text.
const RepoSlugShapes = "gh/<owner>/<repo> or et/<project>/<repo>"

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

// SplitRepoSlug parses "forge/owner/repo" into a forge token and an
// owner/repo name. A bare owner/repo names no forge and is refused: both
// forges take that shape, so guessing one would reintroduce the collision
// the prefix exists to prevent (the same trade `repo clone` makes). ok is
// false for any other shape or an unknown forge token.
func SplitRepoSlug(value string) (forge, name string, ok bool) {
	head, rest, found := strings.Cut(strings.TrimSpace(value), "/")
	if !found || !gitremote.IsForgePathToken(head) || !repoNamePattern.MatchString(rest) {
		return "", "", false
	}
	return head, rest, true
}

// GitHubRepoName returns the owner/repo of a gh/ slug. ok is false for every
// other forge or shape: there is no github.com page for it.
func GitHubRepoName(slug string) (string, bool) {
	forge, name, ok := SplitRepoSlug(slug)
	if !ok || forge != GitHubForge {
		return "", false
	}
	return name, true
}

// normalizeRepoSlug trims a slug and checks it names its forge. A bare
// owner/repo is offered both readings rather than one guess.
func normalizeRepoSlug(value string) (string, error) {
	forge, name, ok := SplitRepoSlug(value)
	if ok {
		return forge + "/" + name, nil
	}
	if bare := strings.TrimSpace(value); repoNamePattern.MatchString(bare) {
		return "", fmt.Errorf("invalid repo %q: a repo must name its forge — did you mean %s/%s or %s/%s?", value, GitHubForge, bare, NativeForge, bare)
	}
	return "", fmt.Errorf("invalid repo %q: expected %s", value, RepoSlugShapes)
}

// normalizeRepoSlugs validates every slug and drops duplicates, so
// "gh/owner/repo" and "gh/OWNER/REPO" count once. Names fold case like
// echoedSlugMatches; the first spelling wins.
func normalizeRepoSlugs(values []string) ([]string, error) {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		slug, err := normalizeRepoSlug(value)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(slug)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, slug)
	}
	return normalized, nil
}

// echoedSlugMatches reports whether a slug the gateway echoed names the
// forge-qualified slug this request sent. The forge must agree and the name
// compares case-insensitively. The gateway learned the prefixed form after
// years of bare GitHub names, so a bare echo is read as GitHub: that is the
// server's legacy spelling, never a user's input, which SplitRepoSlug refuses.
func echoedSlugMatches(echoed, requested string) bool {
	forgeR, nameR, ok := SplitRepoSlug(requested)
	if !ok {
		return false
	}
	forgeE, nameE, ok := SplitRepoSlug(echoed)
	if !ok {
		echoed = strings.TrimSpace(echoed)
		if !repoNamePattern.MatchString(echoed) {
			return false
		}
		forgeE, nameE = GitHubForge, echoed
	}
	return forgeE == forgeR && strings.EqualFold(nameE, nameR)
}
