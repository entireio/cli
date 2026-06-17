// Package review contains the env-var contract between `entire review` (which
// spawns the agent process) and the lifecycle hook (which adopts the session).
// These names are stable API; renaming any constant is a breaking change.
//
// Design rationale: each spawned agent inherits its own copy of the process
// environment, so multi-tenant correctness (multiple worktrees, multi-agent
// runs) holds by construction — one agent's env vars cannot bleed into
// another agent's session. The lifecycle UserPromptSubmit hook reads these
// env vars to tag the in-flight session as a review session (Kind =
// "agent_review") and records which skills were run.
package review

import (
	"encoding/json"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/provenance"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Review env vars. Names live in cmd/entire/cli/provenance; review aliases
// them so existing call sites (review.EnvSession, etc.) keep working.
const (
	EnvSession     = provenance.ReviewSession
	EnvAgent       = provenance.ReviewAgent
	EnvSkills      = provenance.ReviewSkills
	EnvPrompt      = provenance.ReviewPrompt
	EnvStartingSHA = provenance.ReviewStartingSHA
)

// EncodeSkills serialises a slice of skill invocation strings to a JSON value
// suitable for storing in the ENTIRE_REVIEW_SKILLS environment variable.
// An empty or nil slice encodes to the literal string "[]".
func EncodeSkills(skills []string) (string, error) {
	if len(skills) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(skills)
	if err != nil {
		return "", fmt.Errorf("encode skills: %w", err)
	}
	return string(b), nil
}

// DecodeSkills deserialises a value previously produced by EncodeSkills.
// An empty string decodes to a nil slice (no skills configured).
// Any other value that is not valid JSON returns an error.
func DecodeSkills(encoded string) ([]string, error) {
	if encoded == "" {
		return nil, nil
	}
	var skills []string
	if err := json.Unmarshal([]byte(encoded), &skills); err != nil {
		return nil, fmt.Errorf("decode skills: %w", err)
	}
	return skills, nil
}

// AppendReviewEnv adds the ENTIRE_REVIEW_* env vars to base, returning
// the new slice. Used by per-agent reviewers in their AgentReviewer.Start
// implementations to propagate the review-session contract to spawned
// agent processes.
//
// agentName must be the agent's stable registry key (e.g. "claude-code").
// cfg carries skills and the starting SHA. prompt is the full composed
// prompt text (result of ComposeReviewPrompt).
//
// Any pre-existing ENTIRE_REVIEW_* AND ENTIRE_INVESTIGATE_* entries in
// base are stripped before the new values are appended. Stripping review
// entries handles nested invocations and stale inheritance from a parent
// shell — duplicate keys would otherwise have implementation-defined
// precedence. Stripping investigate entries prevents an outer
// `entire investigate` session from mis-tagging a child review session if
// invoked nested (symmetric to AppendInvestigateEnv's behavior).
func AppendReviewEnv(base []string, agentName string, cfg reviewtypes.RunConfig, prompt string) []string {
	skillsJSON, _ := EncodeSkills(cfg.Skills) //nolint:errcheck // EncodeSkills only fails on json.Marshal([]string), which is infallible
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		if provenance.IsEntry(kv) {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		EnvSession+"=1",
		EnvAgent+"="+agentName,
		EnvSkills+"="+skillsJSON,
		EnvPrompt+"="+prompt,
		EnvStartingSHA+"="+cfg.StartingSHA,
	)
}
