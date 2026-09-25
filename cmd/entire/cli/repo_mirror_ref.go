package cli

import (
	"fmt"
	"strings"
)

const mirrorRepoRefHelp = "Repository references name their forge: /gh/<owner>/<repo> for a GitHub " +
	"mirror, /et/<project>/<repo> for an Entire-native repository."

// mirrorRepoRef is a repository named the one way this subtree names one:
// /<forge>/<a>/<b>. The pair reads owner/repo on GitHub and project/repo on
// Entire, which is the same shape with different words, so the fields carry the
// forge-neutral names and forge says how to read them.
type mirrorRepoRef struct {
	forge string
	owner string // GitHub owner, or Entire project
	repo  string
}

// qualified renders the ref back the way the user must type it.
func (r mirrorRepoRef) qualified() string {
	return "/" + r.forge + "/" + r.owner + "/" + r.repo
}

// forgeNoun / forgePluralNoun name a forge the way an error should: what KIND
// of repository is being talked about, not which path token spells it.
var (
	forgeNoun = map[string]string{
		nativeCloneForge: "Entire repository",
		mirrorCloneForge: "GitHub mirror",
	}
	forgePluralNoun = map[string]string{
		nativeCloneForge: "Entire repositories",
		mirrorCloneForge: "GitHub mirrors",
	}
)

// parseMirrorRepoRef separates repository syntax from the forges a given verb
// serves. A repository is named /<forge>/<a>/<b> and no other way, so a bare
// pair cannot select a forge implicitly and a GitHub URL is recognised only to
// say which ref it should have been — the same trade `repo clone` makes in
// invalidCloneRefError.
//
// forges narrows the answer to the grammars the CALLING verb acts on, exactly
// as bareRefSuggestions does and for the same reason: a verb that serves one
// forge must say so, or it suggests a ref it refuses on the next line. Empty
// means every forge.
func parseMirrorRepoRef(ref string, forges ...string) (mirrorRepoRef, error) {
	ref = strings.TrimSpace(ref)
	// Declaring a forge is the whole answer about which grammar was meant, so a
	// verb that does not serve it never parses the ref: validating first gave
	// one answer for a well-formed ref and a name-rule lecture for a malformed
	// one, sending the reader to fix a name that would be refused either way.
	for _, forge := range []string{nativeCloneForge, mirrorCloneForge} {
		if !declaresForge(ref, forge) {
			continue
		}
		if !suggestsForge(forges, forge) {
			return mirrorRepoRef{}, unsupportedForgeErr(ref, forge, forges)
		}
		return parseDeclaredMirrorRepoRef(ref, forge)
	}
	// A GitHub URL names its forge, so it is unambiguous — but it is still not
	// how a repository is named here, and accepting it would leave two
	// spellings for one repo.
	if suggestsForge(forges, mirrorCloneForge) {
		if o, r, uerr := parseHostedGitHubURL(ref); uerr == nil {
			return mirrorRepoRef{}, fmt.Errorf("invalid <repo> %q: pass GitHub repositories as /%s/%s/%s", ref, mirrorCloneForge, o, r)
		}
	}
	if suggestions := bareRefSuggestions(ref, forges...); len(suggestions) > 0 {
		return mirrorRepoRef{}, fmt.Errorf("invalid <repo>: repository reference must name its forge; did you mean %s?", strings.Join(suggestions, " or "))
	}
	// Placeholders, as mirrorRepoRefHelp spells them: the shapes are what the
	// reader has to fill in, and `/gh/owner/repo` reads like a repo called
	// "repo" owned by "owner".
	return mirrorRepoRef{}, fmt.Errorf("invalid <repo>: expected a forge-qualified repository reference such as /%s/<owner>/<repo> or /%s/<project>/<repo>, got %q", mirrorCloneForge, nativeCloneForge, ref)
}

// parseDeclaredMirrorRepoRef reads a ref that has already named a forge the
// caller serves, through that forge's own grammar — the same parsers `repo
// clone` uses, so the two can never disagree about what a name may contain.
func parseDeclaredMirrorRepoRef(ref, forge string) (mirrorRepoRef, error) {
	var owner, repo string
	var err error
	switch forge {
	case nativeCloneForge:
		owner, repo, err = parseNativeCloneRef(ref)
	case mirrorCloneForge:
		_, owner, repo, err = parseMirrorCloneRef(ref)
	}
	if err != nil {
		return mirrorRepoRef{}, fmt.Errorf("invalid <repo> %q: %w", ref, err)
	}
	return mirrorRepoRef{forge: forge, owner: owner, repo: repo}, nil
}

// unsupportedForgeErr reports a ref whose forge this verb does not act on. It
// names the kind of repository rather than the token, and says what the verb
// does serve, so the reader learns the boundary rather than just being stopped
// at it. A caller that knows where the answer lives can append its own pointer.
//
// No production caller narrows forges today: the mirror verbs read both and
// branch on what they get, and `repo grant` refuses a mirror ref itself, with
// the upstream reason no parser has. So this reaches only the tests that pin
// the refusal — kept because narrowing is the parser's contract, not because
// something currently uses it.
func unsupportedForgeErr(ref, forge string, served []string) error {
	supported := make([]string, 0, len(served))
	for _, f := range served {
		supported = append(supported, forgePluralNoun[f])
	}
	return fmt.Errorf("this operation does not support %s %q; it currently supports %s only",
		forgeNoun[forge], ref, strings.Join(supported, " and "))
}
