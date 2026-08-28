package agent

import "github.com/entireio/cli/cmd/entire/cli/agent/types"

// TokenAttributor is implemented by agents whose transcript records
// per-API-call usage and tool calls, so a caller can attribute tokens to the
// tool, skill, or subagent that caused them instead of only seeing a session
// total.
//
// An agent whose transcript does not record usage at the granularity of an
// individual API call (or does not structurally record the tool calls each
// call emitted/consumed) MUST NOT implement this. Not implementing it is the
// honest answer: reports fall back to the session-wide total for that agent,
// and callers are required to treat "no attributor" as "cannot attribute" —
// never as "attributed zero" — for the same reason ToolInvocationScanner
// distinguishes "found nothing" from "cannot look" (see
// tool_invocations.go).
//
// Like ToolUseRef (types package), the returned data deliberately omits the
// raw command: commands are user content and must not be stored.
type TokenAttributor interface {
	// AttributeTokens returns one CallUsage per API call from startLine
	// (line or message index, in the same unit as the agent's
	// CalculateTokenUsage offset) onward, in transcript order, plus
	// SubagentRecords when subagentsDir != "" (live sessions). Committed
	// checkpoints pass "" and supply subagent records from task records
	// instead. Malformed lines are skipped, never fatal.
	AttributeTokens(transcript []byte, startLine int, subagentsDir string) (*types.Attribution, error)
}
