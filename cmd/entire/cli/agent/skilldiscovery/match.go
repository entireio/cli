// Package skilldiscovery holds the per-agent registries (curated built-ins,
// install hints) and the keyword match helper that the `entire review`
// picker uses to discover review-adjacent skills.
package skilldiscovery

import "strings"

// reviewKeywords are the substrings that mark a skill as review-adjacent.
// Kept narrow on purpose — see the spec's "Keyword set — deliberate
// omissions" section for why "lint" is NOT here.
var reviewKeywords = []string{
	"review",
	"audit",
	"inspect",
	"critique",
	"assess",
	"security-scan",
}

// Matches reports whether the given skill invocation contains any
// review-adjacent keyword (case-insensitive) in its name.
//
// We deliberately match on name only. Descriptions often contain words like
// "review" or "inspect" in non-review contexts — "review the plan",
// "inspect recent sessions", "review checkpoints" — which would pull in
// unrelated skills. Legitimate review skills either have the keyword in
// their name directly (e.g. /test-auditor, /superpowers:receiving-code-review)
// or live under a plugin whose prefix contains it (e.g.
// /pr-review-toolkit:silent-failure-hunter matches via "review" in
// "pr-review-toolkit").
//
// The description parameter is retained for signature stability — callers
// still supply it so the picker can show descriptions — but it does not
// affect match decisions.
func Matches(name, description string) bool {
	_ = description
	lname := strings.ToLower(name)
	for _, kw := range reviewKeywords {
		if strings.Contains(lname, kw) {
			return true
		}
	}
	return false
}
