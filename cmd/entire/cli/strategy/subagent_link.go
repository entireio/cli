package strategy

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

// RecordSubagentLink records the explicit parent->child link for one completed
// subagent on the parent session state, so the parent's checkpoint metadata can
// carry a UI-consumable subagent list (see session.State.Subagents).
//
// It is recorded for EVERY completed subagent — including ones that changed no
// files (and therefore produce no task checkpoint) — so the UI gets the complete
// parent->children tree. Upsert keeps it idempotent across a re-fired
// SubagentEnd. A missing session state is treated as a no-op (mirrors
// RecordFilesTouched).
//
// A subagent with neither a ToolUseID nor an AgentID is skipped: there is no
// usable key and no child link to render, and recording it would collapse
// distinct ID-less subagents (e.g. copilot, whose SubagentEnd carries no ids)
// into a single empty-keyed entry.
func RecordSubagentLink(ctx context.Context, sessionID, toolUseID, agentID, subagentType, description string, modified, added, deleted []string) error {
	if toolUseID == "" && agentID == "" {
		return nil
	}
	link := checkpoint.SubagentLink{
		ToolUseID:    toolUseID,
		AgentID:      agentID,
		SubagentType: subagentType,
		Description:  description,
		FileCount:    countUniqueFiles(modified, added, deleted),
	}
	err := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		state.Subagents = upsertSubagentLink(state.Subagents, link)
		return nil
	})
	if errors.Is(err, ErrStateNotFound) {
		return nil
	}
	return err
}

// subagentKey is the identity used to dedup subagent links: the ToolUseID when
// present (Codex/Claude), else the AgentID. Falling back to AgentID keeps
// distinct subagents that lack a ToolUseID from collapsing onto an empty key.
func subagentKey(l checkpoint.SubagentLink) string {
	if l.ToolUseID != "" {
		return l.ToolUseID
	}
	return l.AgentID
}

// upsertSubagentLink appends link, or merges it into the existing entry with the
// same key (see subagentKey). Merge (rather than replace) keeps the first,
// baseline-correct record authoritative; see mergeSubagentLink.
func upsertSubagentLink(existing []checkpoint.SubagentLink, link checkpoint.SubagentLink) []checkpoint.SubagentLink {
	key := subagentKey(link)
	for i := range existing {
		if subagentKey(existing[i]) == key {
			existing[i] = mergeSubagentLink(existing[i], link)
			return existing
		}
	}
	return append(existing, link)
}

// mergeSubagentLink combines a prior entry with a re-fire for the same key. The
// first SubagentEnd is authoritative: it ran with the pre-task baseline, so its
// FileCount is the only trustworthy one — a later, baseline-less re-fire can both
// under- and over-count. So the prior entry is kept as-is and a re-fire only
// fills fields the first record was missing; it can never overwrite a recorded
// FileCount or a non-empty field.
func mergeSubagentLink(prev, next checkpoint.SubagentLink) checkpoint.SubagentLink {
	merged := prev
	if merged.AgentID == "" {
		merged.AgentID = next.AgentID
	}
	if merged.SubagentType == "" {
		merged.SubagentType = next.SubagentType
	}
	if merged.Description == "" {
		merged.Description = next.Description
	}
	return merged
}

// countUniqueFiles counts the distinct paths across the given lists,
// slash-normalized to match mergeFilesTouched.
func countUniqueFiles(lists ...[]string) int {
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, f := range list {
			seen[filepath.ToSlash(f)] = struct{}{}
		}
	}
	return len(seen)
}
