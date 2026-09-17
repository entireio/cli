package cli

import (
	"fmt"
	"strings"
)

const mirrorRepoRefHelp = "Repository references name their forge: /gh/<owner>/<repo> for GitHub, " +
	"/et/<project>/<repo> for Entire. This operation currently supports GitHub mirrors only."

// parseGitHubMirrorRepoRef separates repository syntax from the forges that
// mirror operations support. A repository is named /<forge>/<a>/<b> and no
// other way, so a bare pair cannot select a forge implicitly and a GitHub URL
// is recognised only to say which ref it should have been — the same trade
// `repo clone` makes in invalidCloneRefError.
func parseGitHubMirrorRepoRef(ref string) (owner, repo string, err error) {
	ref = strings.TrimSpace(ref)
	// Declaring the native forge is the whole answer, so the ref is never
	// parsed: these verbs refuse every /et/ ref, which makes how the project
	// and repo are spelled irrelevant. Validating first gave one answer for a
	// well-formed ref and a name-rule lecture for a malformed one, sending the
	// reader to fix a name that would be refused either way. The name is worth
	// checking in `repo grant`, which can act on it.
	if declaresForge(ref, nativeCloneForge) {
		return "", "", fmt.Errorf("this operation does not support Entire repository %q; it currently supports GitHub mirrors only", ref)
	}
	if declaresForge(ref, mirrorCloneForge) {
		_, owner, repo, err = parseMirrorCloneRef(ref)
		if err != nil {
			return "", "", fmt.Errorf("invalid <repo> %q: %w", ref, err)
		}
		return owner, repo, nil
	}
	// A GitHub URL names its forge, so it is unambiguous — but it is still not
	// how a repository is named here, and accepting it would leave two
	// spellings for one repo.
	if o, r, uerr := parseHostedGitHubURL(ref); uerr == nil {
		return "", "", fmt.Errorf("invalid <repo> %q: pass GitHub repositories as /%s/%s/%s", ref, mirrorCloneForge, o, r)
	}
	// GitHub-only, so only the mirror reading is offered: suggesting the
	// native one would name a ref this same function refuses above.
	if suggestions := bareRefSuggestions(ref, mirrorCloneForge); len(suggestions) > 0 {
		return "", "", fmt.Errorf("invalid <repo>: repository reference must name its forge; did you mean %s?", strings.Join(suggestions, " or "))
	}
	return "", "", fmt.Errorf("invalid <repo>: expected a forge-qualified repository reference such as /gh/owner/repo or /et/project/repo, got %q", ref)
}
