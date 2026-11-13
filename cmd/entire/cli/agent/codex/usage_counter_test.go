package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// A ledger reading counts an inventoried child with the inventory's own rule
// (its last token_count), apart from the parent's total, on every reading.
func TestUsageCounter_InventoriedChild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	child := writeRollout(t, root, "rollout-child.jsonl", "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("child-turn")),
		taskEvent("task_complete", stringPointer("child-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	parent := filepath.Join(root, "rollout-parent.jsonl")
	require.NoError(t, os.WriteFile(parent, rolloutData(t, "parent", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 10}}),
	}), 0o600))
	in := agent.CumulativeTokenInput{TranscriptPath: parent, Subagents: []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: child}}}

	first, ok, err := agent.ReadCumulativeUsage(t.Context(), ag, in, nil)
	require.True(t, ok)
	require.NoError(t, err)
	require.Equal(t, 100, first.Total.InputTokens, "the child is not in the parent's total")
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1, APICallCount: 1}, first.Children["child"])

	again, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, first.Children, again.Children, "an unchanged child reports the same total")

	// The child grows: its total is read again.
	f, err := os.OpenFile(child, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	line := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 9, "cached_input_tokens": 2, "output_tokens": 4}})
	_, err = f.Write(append(line, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	grown, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, again.Cursor)
	require.NoError(t, err)
	require.Equal(t, 4, grown.Children["child"].OutputTokens)
}

// A reading saves the child totals it read, so the next reading of an
// unchanged inventory reuses them instead of reading every child rollout
// again: a child rewritten with the same size and modification time still
// reports the saved total.
func TestUsageCounter_UnchangedChildReusesSavedTotal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	child := writeRollout(t, root, "rollout-child.jsonl", "child", childUsageEvents(5))
	in := agent.CumulativeTokenInput{TranscriptPath: writeParentRollout(t, root), Subagents: []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: child}}}

	first, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, nil)
	require.NoError(t, err)
	require.Equal(t, 3, first.Children["child"].InputTokens)

	info, err := os.Stat(child)
	require.NoError(t, err)
	writeRollout(t, root, "rollout-child.jsonl", "child", childUsageEvents(6))
	require.NoError(t, os.Chtimes(child, info.ModTime(), info.ModTime()))
	again, err := os.Stat(child)
	require.NoError(t, err)
	require.Equal(t, info.Size(), again.Size(), "the rewrite keeps the child's size")

	second, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, 3, second.Children["child"].InputTokens, "an unchanged inventory reuses the saved child total")
}

// childUsageEvents is a finished child turn whose running total is input
// tokens, two of them cached, and one output token.
func childUsageEvents(input int) []json.RawMessage {
	return []json.RawMessage{
		taskEvent("task_started", stringPointer("child-turn")),
		taskEvent("task_complete", stringPointer("child-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": input, "cached_input_tokens": 2, "output_tokens": 1}}),
	}
}

// writeParentRollout writes a parent rollout with one running total.
func writeParentRollout(t *testing.T, root string) string {
	t.Helper()
	parent := filepath.Join(root, "rollout-parent.jsonl")
	require.NoError(t, os.WriteFile(parent, rolloutData(t, "parent", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 10}}),
	}), 0o600))
	return parent
}

// A reading that could not read a child (here canceled) saves nothing for
// the children, so the next reading of the unchanged inventory reads the
// child again instead of reusing the gap.
func TestUsageCounter_FailedChildReadIsReadAgain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	child := writeRollout(t, root, "rollout-child.jsonl", "child", childUsageEvents(5))
	in := agent.CumulativeTokenInput{TranscriptPath: writeParentRollout(t, root), Subagents: []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: child}}}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	first, _, err := agent.ReadCumulativeUsage(canceled, ag, in, nil)
	require.NoError(t, err)
	require.Nil(t, first.Children["child"], "a canceled reading counts nothing for the child")

	second, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.NotNil(t, second.Children["child"], "the unchanged child is read again")
	require.Equal(t, 3, second.Children["child"].InputTokens)
}

// A child is read from its declared path before its resolved one, so a
// change to the declared rollout is read even when the resolved one, also
// present, is unchanged.
func TestUsageCounter_DeclaredChildChangeIsRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	declared := writeRollout(t, root, "rollout-declared.jsonl", "child", childUsageEvents(5))
	resolved := writeRollout(t, root, "rollout-resolved.jsonl", "child", childUsageEvents(9))
	in := agent.CumulativeTokenInput{TranscriptPath: writeParentRollout(t, root), Subagents: []agent.SubagentReference{{
		AgentID: "child", DeclaredTranscriptPath: declared, ResolvedTranscriptPath: resolved,
	}}}

	first, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, nil)
	require.NoError(t, err)
	require.Equal(t, 3, first.Children["child"].InputTokens, "the declared rollout is read")

	writeRollout(t, root, "rollout-declared.jsonl", "child", childUsageEvents(15))
	second, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, 13, second.Children["child"].InputTokens, "the declared rollout's change is read")
}

// A reading that could not read a child's declared rollout and fell back to
// its resolved one is not cached: the declared file's size and time don't
// change when it becomes readable again, so the next reading must try it.
func TestUsageCounter_FallbackPastDeclaredChildIsReadAgain(t *testing.T) {
	t.Parallel()
	declaredRoot, resolvedRoot := t.TempDir(), t.TempDir()
	declared := writeRollout(t, declaredRoot, "rollout-declared.jsonl", "child", childUsageEvents(5))
	resolved := writeRollout(t, resolvedRoot, "rollout-resolved.jsonl", "child", childUsageEvents(9))
	in := agent.CumulativeTokenInput{TranscriptPath: writeParentRollout(t, resolvedRoot), Subagents: []agent.SubagentReference{{
		AgentID: "child", DeclaredTranscriptPath: declared, ResolvedTranscriptPath: resolved,
	}}}

	// The declared rollout is outside this agent's rollout roots, so its read
	// fails and the child falls back to the resolved one.
	first, _, err := agent.ReadCumulativeUsage(t.Context(), &CodexAgent{RolloutRoots: []string{resolvedRoot}}, in, nil)
	require.NoError(t, err)
	require.Equal(t, 7, first.Children["child"].InputTokens, "the resolved rollout is read")

	second, _, err := agent.ReadCumulativeUsage(t.Context(), &CodexAgent{RolloutRoots: []string{declaredRoot, resolvedRoot}}, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, 3, second.Children["child"].InputTokens, "the declared rollout is read once it can be")
}

// A child rollout path outside the trusted rollout roots is not looked at,
// so the inventory is not cached.
func TestChildFingerprint_PathOutsideRolloutRootsIsNotCached(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(outside, "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	in := agent.CumulativeTokenInput{Subagents: []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: path}}}

	_, _, ok := childFingerprint(in, []string{root})
	require.False(t, ok)

	_, preferred, ok := childFingerprint(in, []string{outside})
	require.True(t, ok)
	require.Equal(t, []string{path}, preferred)
}
