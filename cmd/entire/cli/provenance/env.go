// Package provenance owns the env-var contract that lets the lifecycle hook
// recognize a spawned agent process as part of `entire review` or `entire
// investigate`. Both spawn families set their own ENTIRE_*_* vars on the
// child agent process; the UserPromptSubmit hook reads them to tag the
// in-flight session with the right Kind and provenance metadata.
//
// Single source of truth for the names. `entire review` lives in this
// repository; `entire investigate` ships as the entire-investigate plugin and
// imports this package for the same constants, so these names are a contract
// across a process boundary as well as inside this binary.
//
// These names are stable API; renaming any constant is a breaking change.
package provenance

import (
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

const (
	ReviewSession     = "ENTIRE_REVIEW_SESSION"
	ReviewAgent       = "ENTIRE_REVIEW_AGENT"
	ReviewSkills      = "ENTIRE_REVIEW_SKILLS"
	ReviewPrompt      = "ENTIRE_REVIEW_PROMPT"
	ReviewStartingSHA = "ENTIRE_REVIEW_STARTING_SHA"

	InvestigateSession     = "ENTIRE_INVESTIGATE_SESSION"
	InvestigateAgent       = "ENTIRE_INVESTIGATE_AGENT"
	InvestigateRunID       = "ENTIRE_INVESTIGATE_RUN_ID"
	InvestigateTopic       = "ENTIRE_INVESTIGATE_TOPIC"
	InvestigateFindingsDoc = "ENTIRE_INVESTIGATE_FINDINGS_DOC"
	InvestigateStateDoc    = "ENTIRE_INVESTIGATE_STATE_DOC"
	InvestigateStartingSHA = "ENTIRE_INVESTIGATE_STARTING_SHA"
)

var reviewPrefixes = []string{
	ReviewSession + "=",
	ReviewAgent + "=",
	ReviewSkills + "=",
	ReviewPrompt + "=",
	ReviewStartingSHA + "=",
}

var investigatePrefixes = []string{
	InvestigateSession + "=",
	InvestigateAgent + "=",
	InvestigateRunID + "=",
	InvestigateTopic + "=",
	InvestigateFindingsDoc + "=",
	InvestigateStateDoc + "=",
	InvestigateStartingSHA + "=",
}

// IsReviewEntry reports whether kv is a "KEY=VALUE" entry whose key is one
// of the ENTIRE_REVIEW_* contract variables.
func IsReviewEntry(kv string) bool {
	return hasAnyPrefix(kv, reviewPrefixes)
}

// IsInvestigateEntry reports whether kv is a "KEY=VALUE" entry whose key is
// one of the ENTIRE_INVESTIGATE_* contract variables.
func IsInvestigateEntry(kv string) bool {
	return hasAnyPrefix(kv, investigatePrefixes)
}

// IsEntry reports whether kv is a "KEY=VALUE" entry from either family.
// Callers strip provenance markers with it before spawning a child that must
// not inherit the parent's tagging — review when it builds a nested agent's
// environment, and the investigate plugin before it launches a fix session.
func IsEntry(kv string) bool {
	return IsReviewEntry(kv) || IsInvestigateEntry(kv)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// runIDPattern matches a valid investigation run ID: exactly 12 lowercase
// hex characters. Re-uses checkpoint/id.Pattern so the format stays in
// lockstep with the checkpoint-ID format used elsewhere in the codebase.
var runIDPattern = regexp.MustCompile("^" + id.Pattern + "$")

// IsValidRunID reports whether runID is exactly 12 lowercase hex
// characters. Lives here (next to the InvestigateRunID env name) so the
// lifecycle hook can validate the env-supplied run ID without pulling in
// the heavier investigate package.
func IsValidRunID(runID string) bool {
	return runID != "" && runIDPattern.MatchString(runID)
}
