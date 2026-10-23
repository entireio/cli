package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// newTokenLedger returns the token ledger for a session starting now, or nil
// when the agent reports no running totals. Whatever the transcript already
// holds (a resumed conversation whose earlier turns another session state
// counted) is treated as already checkpointed, so only usage from this turn on
// is counted.
func newTokenLedger(ctx context.Context, agentType types.AgentType, sessionID, transcriptPath string) *session.TokenLedger {
	ag, err := agent.GetByAgentType(agentType)
	if err != nil || transcriptPath == "" {
		return nil
	}
	if _, ok := agent.AsUsageCounterSource(ag); !ok || isSubagentSession(ag, transcriptPath) {
		return nil
	}
	ledger := &session.TokenLedger{}
	observeTokenLedger(ctx, ag, ledger, agent.CumulativeTokenInput{SessionID: sessionID, TranscriptPath: transcriptPath})
	ledger.MarkCheckpointed()
	ledger.Active = ledger.Total() != nil
	return ledger
}

// ObserveTokenLedger reads the session's running totals from its live
// transcript into its token ledger and reports whether the ledger is the
// session's token source; when it is, TokenUsage is set from it. Callers hold
// the session lock, which orders readings. The first reading that finds usage
// activates the ledger, taking over from the per-turn accounting: what that
// accounting already checkpointed counts as checkpointed. false for a session
// without a ledger.
func ObserveTokenLedger(ctx context.Context, state *SessionState) bool {
	if state == nil || state.TokenLedger == nil {
		return false
	}
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return state.TokenLedger.IsActive()
	}
	if isSubagentSession(ag, state.TranscriptPath) {
		// A subagent's own session (a Droid Worker) reaches the parent as one
		// of the parent's children; a total of its own would count it twice.
		state.TokenLedger = nil
		return false
	}
	observeTokenLedger(ctx, ag, state.TokenLedger, agent.CumulativeTokenInput{
		SessionID:      state.SessionID,
		TranscriptPath: state.TranscriptPath,
		Subagents:      SubagentReferences(state),
		Home:           state.AgentHome,
	})
	ledger := state.TokenLedger
	if !ledger.Active && ledger.Total() != nil {
		// Activate keeps only the main counters; the subagent baseline is what
		// checkpoints hold of subagents.
		checkpointed := types.SubtractTokenUsage(state.TokenUsage, state.CheckpointTokenUsage)
		ledger.Activate(checkpointed, state.SubagentTokensBaseline)
		state.CheckpointTokenUsage = nil
	}
	if !ledger.Active {
		return false
	}
	state.TokenUsage = ledger.Total()
	return true
}

// observeTokenLedger folds one reading of the session's running totals into
// ledger. A transcript that cannot be read is skipped rather than read as
// empty: a missing file is not a rewrite to nothing.
func observeTokenLedger(ctx context.Context, ag agent.Agent, ledger *session.TokenLedger, in agent.CumulativeTokenInput) {
	if in.TranscriptPath == "" {
		return
	}
	obs, ok, err := agent.ReadCumulativeUsage(ctx, ag, in, ledger.Cursor)
	if !ok {
		return
	}
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "tokens"), "token ledger: reading running totals failed",
			slog.String("session_id", in.SessionID),
			slog.String("error", err.Error()))
		return
	}
	ledger.Observe(obs)
}

// SubagentReferences returns the session's Codex child inventory in the shape
// agents read it.
func SubagentReferences(state *SessionState) []agent.SubagentReference {
	if len(state.SubagentInventory) == 0 {
		return nil
	}
	refs := make([]agent.SubagentReference, 0, len(state.SubagentInventory))
	for _, entry := range state.SubagentInventory {
		refs = append(refs, agent.SubagentReference{
			ObservedTurnIDs:        entry.ObservedTurnIDs,
			AgentID:                entry.AgentID,
			DeclaredTranscriptPath: entry.DeclaredTranscriptPath,
			ResolvedTranscriptPath: entry.ResolvedTranscriptPath,
		})
	}
	return refs
}

// isSubagentSession reports whether the session behind transcriptPath is a
// subagent's own session, linked to a parent that counts it.
func isSubagentSession(ag agent.Agent, transcriptPath string) bool {
	resolver, ok := agent.AsSubagentSessionResolver(ag)
	if !ok || transcriptPath == "" {
		return false
	}
	_, isSubagent := resolver.ResolveSubagentSession(transcriptPath)
	return isSubagent
}
