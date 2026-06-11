// Package investigate contains the env-var contract between `entire
// investigate` (which spawns the agent process) and the lifecycle hook (which
// adopts the session), plus the persisted run state for resuming an
// investigation. These names are stable API; renaming any constant is a
// breaking change.
//
// Design rationale: each spawned agent inherits its own copy of the process
// environment, so multi-tenant correctness (multiple worktrees, multi-agent
// runs) holds by construction — one agent's env vars cannot bleed into
// another agent's session. The lifecycle UserPromptSubmit hook reads these
// env vars to tag the in-flight session as an investigate session (Kind =
// "agent_investigate") and records the run id + topic.
package investigate

import (
	"github.com/entireio/cli/cmd/entire/cli/provenance"
)

// Investigate env vars. Names live in cmd/entire/cli/provenance; aliased
// here for the package's call sites.
const (
	EnvSession     = provenance.InvestigateSession
	EnvAgent       = provenance.InvestigateAgent
	EnvRunID       = provenance.InvestigateRunID
	EnvTopic       = provenance.InvestigateTopic
	EnvFindingsDoc = provenance.InvestigateFindingsDoc
	EnvStateDoc    = provenance.InvestigateStateDoc
	EnvStartingSHA = provenance.InvestigateStartingSHA
)

// AppendOptions carries the data needed to populate the ENTIRE_INVESTIGATE_*
// env vars on a spawned agent process.
type AppendOptions struct {
	AgentName   string
	RunID       string
	Topic       string
	FindingsDoc string
	StateDoc    string
	StartingSHA string
}

// AppendInvestigateEnv adds the ENTIRE_INVESTIGATE_* env vars to base,
// returning the new slice. Used by the loop driver when spawning each per-turn
// agent process to propagate the investigate-session contract.
//
// Any pre-existing ENTIRE_INVESTIGATE_* AND ENTIRE_REVIEW_* entries in base
// are stripped before the new values are appended. Stripping investigate
// entries handles nested invocations and stale inheritance from a parent
// shell — duplicate keys would otherwise have implementation-defined
// precedence. Stripping review entries prevents an outer `entire review`
// session from mis-tagging a child investigate session if invoked nested.
func AppendInvestigateEnv(base []string, opts AppendOptions) []string {
	out := make([]string, 0, len(base)+10)
	for _, kv := range base {
		if provenance.IsEntry(kv) {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		EnvSession+"=1",
		EnvAgent+"="+opts.AgentName,
		EnvRunID+"="+opts.RunID,
		EnvTopic+"="+opts.Topic,
		EnvFindingsDoc+"="+opts.FindingsDoc,
		EnvStateDoc+"="+opts.StateDoc,
		EnvStartingSHA+"="+opts.StartingSHA,
	)
}

// IsInvestigateEnvEntry reports whether kv is a "KEY=VALUE" entry whose key
// is one of the ENTIRE_INVESTIGATE_* contract variables.
func IsInvestigateEnvEntry(kv string) bool {
	return provenance.IsInvestigateEntry(kv)
}
