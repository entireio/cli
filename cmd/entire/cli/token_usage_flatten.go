package cli

import (
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// flattenTokenUsage collapses a usage and its SubagentTokens chain into one
// level, summing every level through types.AddTokenUsage so subset fields
// (ThinkingTokens, CacheCreation1hTokens) and the Model label travel with the
// four token classes and APICallCount. The result has no SubagentTokens and
// shares no memory with u; u is not mutated.
//
// Like AddTokenUsage, the walk stops after types.MaxSubagentDepth nested levels
// (real chains are depth 1). A nil input yields nil, matching AddTokenUsage, so
// callers can keep the "no token data" signal distinct from an all-zero usage.
func flattenTokenUsage(u *agent.TokenUsage) *agent.TokenUsage {
	if u == nil {
		return nil
	}
	var flat *agent.TokenUsage
	for level, depth := u, 0; level != nil && depth <= types.MaxSubagentDepth; level, depth = level.SubagentTokens, depth+1 {
		leaf := *level
		leaf.SubagentTokens = nil
		flat = types.AddTokenUsage(flat, &leaf)
	}
	return flat
}
