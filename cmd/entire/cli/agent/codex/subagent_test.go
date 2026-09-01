package codex

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestResolveRollout_UsesExactMetadataID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := filepath.Join(root, "sessions")
	archived := filepath.Join(root, "archived_sessions")
	ag := &CodexAgent{RolloutRoots: []string{active, archived}}

	activePath := writeRollout(t, active, "2026/08/31/rollout-near-child-a.jsonl", "child-a", nil)
	archivedPath := writeRollout(t, archived, "2026/08/30/rollout-child-b.jsonl", "child-b", nil)
	writeRollout(t, active, "2026/08/31/rollout-child-a-suffix.jsonl", "not-child-a", nil)

	got := ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "child-a"})
	require.Equal(t, activePath, got)

	got = ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "child-b"})
	require.Equal(t, archivedPath, got)
}

func TestResolveRollout_DefaultCodexHomeIncludesArchivedSessions(t *testing.T) {
	// This test changes CODEX_HOME, so it must not run in parallel.
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")

	active := filepath.Join(codexHome, "sessions")
	archived := filepath.Join(codexHome, "archived_sessions")
	activePath := writeRollout(t, active, "2026/08/31/rollout-active.jsonl", "active", nil)
	archivedPath := writeRollout(t, archived, "2026/08/30/rollout-archived.jsonl", "archived", nil)
	ag := &CodexAgent{}

	require.Equal(t, activePath, ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "active"}))
	require.Equal(t, archivedPath, ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "archived"}))
}

func TestResolveRollout_MismatchedKnownPathsFallBackOnlyToExactID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := filepath.Join(root, "sessions")
	ag := &CodexAgent{RolloutRoots: []string{active}}
	mismatch := writeRollout(t, root, "declared.jsonl", "wrong", nil)
	exact := writeRollout(t, active, "2026/08/31/rollout-child.jsonl", "child", nil)

	for _, ref := range []agent.SubagentReference{
		{AgentID: "child", DeclaredTranscriptPath: mismatch},
		{AgentID: "child", ResolvedTranscriptPath: mismatch},
	} {
		require.Equal(t, exact, ag.resolveSubagentRollout(ref))
	}
}

func TestResolveRollout_KnownExactPathsNeedNoFallbackRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	declared := writeRollout(t, root, "declared.jsonl", "declared", nil)
	resolved := writeRollout(t, root, "resolved.jsonl", "resolved", nil)
	ag := &CodexAgent{RolloutRoots: []string{filepath.Join(root, "no-fallback-here")}}

	require.Equal(t, declared, ag.resolveSubagentRollout(agent.SubagentReference{
		AgentID:                "declared",
		DeclaredTranscriptPath: declared,
	}))
	require.Equal(t, resolved, ag.resolveSubagentRollout(agent.SubagentReference{
		AgentID:                "resolved",
		ResolvedTranscriptPath: resolved,
	}))
}

func TestResolveRollout_RejectsInferredAndAmbiguousCandidates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := filepath.Join(root, "sessions")
	ag := &CodexAgent{RolloutRoots: []string{active}}
	writeRollout(t, active, "2026/08/31/rollout-child.jsonl", "childish", nil)
	require.Empty(t, ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "child"}))

	writeRollout(t, active, "2026/08/30/rollout-child-one.jsonl", "child", nil)
	writeRollout(t, active, "2026/08/31/rollout-child-two.jsonl", "child", nil)
	require.Empty(t, ag.resolveSubagentRollout(agent.SubagentReference{AgentID: "child"}))
}

func TestResolveRollout_RejectsSymlinkHint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := writeRollout(t, root, "target.jsonl", "child", nil)
	link := filepath.Join(root, "child-link.jsonl")
	require.NoError(t, os.Symlink(target, link))

	ag := &CodexAgent{RolloutRoots: []string{}}
	require.Empty(t, ag.resolveSubagentRollout(agent.SubagentReference{
		AgentID:                "child",
		DeclaredTranscriptPath: link,
	}))
}

func TestRolloutRegularMode_RejectsSpecialEntries(t *testing.T) {
	t.Parallel()

	for _, mode := range []fs.FileMode{0, fs.ModeDir, fs.ModeSymlink, fs.ModeNamedPipe, fs.ModeDevice, fs.ModeSocket} {
		require.Equal(t, mode == 0, rolloutRegularMode(mode), "mode %v", mode)
	}
}

func TestTerminalTurnIDs_OnlyAcceptsUnambiguousBoundaries(t *testing.T) {
	t.Parallel()

	valid := rolloutData(t, "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("one")),
		taskEvent("task_complete", stringPointer("one")),
		taskEvent("task_started", stringPointer("two")),
		taskEvent("task_complete", nil),
	})
	require.Equal(t, []string{"one", "two"}, terminalTurnIDs(valid))

	for _, invalid := range [][]json.RawMessage{
		{taskEvent("task_complete", stringPointer("one"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_started", stringPointer("two")), taskEvent("task_complete", nil)},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("two"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one")), taskEvent("task_complete", stringPointer("one"))},
	} {
		require.Empty(t, terminalTurnIDs(rolloutData(t, "child", invalid)))
	}
}

func TestTerminalTurnIDs_RealWireShape(t *testing.T) {
	t.Parallel()

	modern := rolloutData(t, "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("modern")),
		taskEvent("task_complete", stringPointer("modern")),
	})
	require.Equal(t, []string{"modern"}, terminalTurnIDs(modern))

	legacy := rolloutData(t, "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("legacy")),
		taskEvent("task_complete", nil),
	})
	require.Equal(t, []string{"legacy"}, terminalTurnIDs(legacy))
}

func TestTerminalTurnIDs_RejectsInvalidRealWireBoundaries(t *testing.T) {
	t.Parallel()

	validThenMalformed := []json.RawMessage{
		taskEvent("task_started", stringPointer("one")),
		taskEvent("task_complete", stringPointer("one")),
		json.RawMessage(`{"type":"event_msg","payload":{"type":"task_started","turn_id":`),
	}
	for _, invalid := range [][]json.RawMessage{
		{taskEvent("task_complete", stringPointer("one"))},
		{taskEvent("task_started", nil)},
		{taskEvent("task_started", stringPointer("one"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_started", stringPointer("two"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("two"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one")), taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one"))},
		{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", nil), taskEvent("task_complete", nil)},
		{json.RawMessage(`{"type":"event_msg","payload":{"type":"task_started","turn_id":7}}`)},
		validThenMalformed,
	} {
		require.Empty(t, terminalTurnIDs(rolloutData(t, "child", invalid)))
	}
}

func TestExactTokenUsage_UsesOnlyLastRecognizableSnapshot(t *testing.T) {
	t.Parallel()

	valid := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 15, "cached_input_tokens": 12, "output_tokens": 3,
		"reasoning_output_tokens": 2, "total_tokens": 18,
	}})
	usage := exactCumulativeTokenUsage(rolloutData(t, "child", []json.RawMessage{valid}))
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 12, OutputTokens: 3}, usage)

	malformedLast := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 10, "cached_input_tokens": 11, "output_tokens": 3,
	}})
	require.Nil(t, exactCumulativeTokenUsage(rolloutData(t, "child", []json.RawMessage{valid, malformedLast})))

	missingRequired := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 10, "output_tokens": 3,
	}})
	require.Nil(t, exactCumulativeTokenUsage(rolloutData(t, "child", []json.RawMessage{missingRequired})))
}

func TestExactTokenUsage_RejectsEveryUnavailableOrInconsistentSnapshot(t *testing.T) {
	t.Parallel()

	valid := func(values map[string]any) []byte {
		return rolloutData(t, "child", []json.RawMessage{tokenCountEvent(map[string]any{"total_token_usage": values})})
	}
	require.Nil(t, exactCumulativeTokenUsage(rolloutData(t, "child", nil)))

	for _, values := range []map[string]any{
		{"cached_input_tokens": 0, "output_tokens": 1},
		{"input_tokens": 1, "output_tokens": 1},
		{"input_tokens": 1, "cached_input_tokens": 0},
		{"input_tokens": -1, "cached_input_tokens": 0, "output_tokens": 0},
		{"input_tokens": 1, "cached_input_tokens": -1, "output_tokens": 0},
		{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": -1},
		{"input_tokens": 1, "cached_input_tokens": 2, "output_tokens": 0},
		{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1, "total_tokens": -1},
		{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1, "total_tokens": 1},
		{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1, "reasoning_output_tokens": -1},
		{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1, "reasoning_output_tokens": 2},
	} {
		require.Nil(t, exactCumulativeTokenUsage(valid(values)))
	}

	zeros := exactCumulativeTokenUsage(valid(map[string]any{"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0}))
	require.Equal(t, &agent.TokenUsage{}, zeros)

	multiple := rolloutData(t, "child", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 9, "cached_input_tokens": 1, "output_tokens": 2}}),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 4, "cached_input_tokens": 1, "output_tokens": 2}}),
	})
	usage := exactCumulativeTokenUsage(multiple)
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 1, OutputTokens: 2}, usage)
	require.Zero(t, usage.APICallCount, "snapshot record count is not an API-call count")

	malformedFinal := rolloutData(t, "child", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 4, "cached_input_tokens": 1, "output_tokens": 2}}),
		tokenCountEvent(map[string]any{"total_token_usage": "not-an-object"}),
	})
	require.Nil(t, exactCumulativeTokenUsage(malformedFinal), "must not fall back to the earlier valid snapshot")
}

func TestSubagentInventory_CollectsExactEvidenceAndDoesNotPartialAggregate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	childOne := writeRollout(t, root, "rollout-one.jsonl", "one", []json.RawMessage{
		patchEvent("child.txt"),
		taskEvent("task_started", stringPointer("child-turn")),
		taskEvent("task_complete", stringPointer("child-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	childTwo := writeRollout(t, root, "rollout-two.jsonl", "two", []json.RawMessage{
		patchEvent("two.txt"),
		taskEvent("task_started", stringPointer("two-turn")),
		taskEvent("task_complete", stringPointer("two-turn")),
	})
	parent := rolloutData(t, "parent", []json.RawMessage{patchEvent("parent.txt")})

	result, err := ag.ExtractWithSubagentInventory(parent, 0, []agent.SubagentReference{
		{AgentID: "one", DeclaredTranscriptPath: childOne},
		{AgentID: "two", ResolvedTranscriptPath: childTwo},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"parent.txt", "child.txt", "two.txt"}, result.ModifiedFiles)
	require.Len(t, result.Children, 2)
	require.Equal(t, childOne, result.Children[0].ResolvedPath)
	require.Equal(t, []string{"child.txt"}, result.Children[0].ModifiedFiles)
	require.Equal(t, []string{"child-turn"}, result.Children[0].TerminalTurnIDs)
	require.NotNil(t, result.Children[0].TokenUsage)
	require.Equal(t, []string{"two.txt"}, result.Children[1].ModifiedFiles)
	require.Equal(t, []string{"two-turn"}, result.Children[1].TerminalTurnIDs)
	require.Nil(t, result.Children[1].TokenUsage)
	require.NotNil(t, result.TokenUsage)
	require.Nil(t, result.TokenUsage.SubagentTokens)
	require.NotNil(t, result.TokenUsage.SubagentTokensComplete)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
}

func TestSubagentInventory_AggregatesOnlyCompleteExactChildren(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	first := writeRollout(t, root, "first.jsonl", "first", []json.RawMessage{
		patchEvent("first.txt"),
		taskEvent("task_started", stringPointer("first-turn")),
		taskEvent("task_complete", stringPointer("first-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	second := writeRollout(t, root, "second.jsonl", "second", []json.RawMessage{
		patchEvent("second.txt"),
		taskEvent("task_started", stringPointer("second-turn")),
		taskEvent("task_complete", nil),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 10, "cached_input_tokens": 3, "output_tokens": 5}}),
	})

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{
		{AgentID: "first", DeclaredTranscriptPath: first},
		{AgentID: "second", ResolvedTranscriptPath: second},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"first.txt", "second.txt"}, result.ModifiedFiles)
	require.Len(t, result.Children, 2, "one analysis is retained for each supplied reference")
	require.Equal(t, "first", result.Children[0].AgentID)
	require.Equal(t, first, result.Children[0].ResolvedPath)
	require.Equal(t, []string{"first.txt"}, result.Children[0].ModifiedFiles)
	require.Equal(t, []string{"first-turn"}, result.Children[0].TerminalTurnIDs)
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1}, result.Children[0].TokenUsage)
	require.Equal(t, "second", result.Children[1].AgentID)
	require.Equal(t, second, result.Children[1].ResolvedPath)
	require.Equal(t, []string{"second.txt"}, result.Children[1].ModifiedFiles)
	require.Equal(t, []string{"second-turn"}, result.Children[1].TerminalTurnIDs)
	require.Equal(t, &agent.TokenUsage{InputTokens: 7, CacheReadTokens: 3, OutputTokens: 5}, result.Children[1].TokenUsage)
	require.NotNil(t, result.TokenUsage)
	require.NotNil(t, result.TokenUsage.SubagentTokensComplete)
	require.True(t, *result.TokenUsage.SubagentTokensComplete)
	require.Equal(t, &agent.TokenUsage{InputTokens: 10, CacheReadTokens: 5, OutputTokens: 6}, result.TokenUsage.SubagentTokens)
}

func TestSubagentInventory_EmptyInventoryIsExactWithoutChildTotal(t *testing.T) {
	t.Parallel()

	result, err := (&CodexAgent{RolloutRoots: []string{t.TempDir()}}).ExtractWithSubagentInventory(nil, 0, nil)
	require.NoError(t, err)
	require.Empty(t, result.Children)
	require.NotNil(t, result.TokenUsage)
	require.NotNil(t, result.TokenUsage.SubagentTokensComplete)
	require.True(t, *result.TokenUsage.SubagentTokensComplete)
	require.Nil(t, result.TokenUsage.SubagentTokens)
}

func TestSubagentInventory_UnresolvedChildPreventsPartialAggregate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	available := writeRollout(t, root, "available.jsonl", "available", []json.RawMessage{
		patchEvent("available.txt"),
		taskEvent("task_started", stringPointer("available-turn")),
		taskEvent("task_complete", stringPointer("available-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 2, "cached_input_tokens": 1, "output_tokens": 1}}),
	})

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{
		{AgentID: "available", DeclaredTranscriptPath: available},
		{AgentID: "missing"},
	})
	require.NoError(t, err)
	require.Len(t, result.Children, 2)
	require.Equal(t, []string{"available.txt"}, result.ModifiedFiles)
	require.Equal(t, []string{"available-turn"}, result.Children[0].TerminalTurnIDs)
	require.NotNil(t, result.Children[0].TokenUsage)
	require.Empty(t, result.Children[1].ResolvedPath)
	require.Nil(t, result.Children[1].TokenUsage)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
	require.Nil(t, result.TokenUsage.SubagentTokens)
}

func TestSubagentInventory_LoaderFailureFailsClosedBeforeResolution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := writeRollout(t, root, "child.jsonl", "child", []json.RawMessage{
		patchEvent("child.txt"),
		taskEvent("task_started", stringPointer("child-turn")),
		taskEvent("task_complete", stringPointer("child-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 2, "cached_input_tokens": 1, "output_tokens": 1}}),
	})
	readFailure := errors.New("injected child load failure")
	ag := &CodexAgent{
		RolloutRoots: []string{},
		loadRollout: func(gotPath string) (loadedRollout, error) {
			if gotPath == "" {
				return loadedRollout{}, readFailure
			}
			require.Equal(t, path, gotPath)
			return loadedRollout{}, readFailure
		},
	}

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{{
		AgentID:                "child",
		DeclaredTranscriptPath: path,
	}})
	require.NoError(t, err)
	require.Len(t, result.Children, 1)
	require.Equal(t, "child", result.Children[0].AgentID)
	require.Empty(t, result.Children[0].ResolvedPath, "same-byte validation cannot retain a failed load")
	require.Empty(t, result.Children[0].ModifiedFiles)
	require.Empty(t, result.Children[0].TerminalTurnIDs)
	require.Nil(t, result.Children[0].TokenUsage)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
	require.Nil(t, result.TokenUsage.SubagentTokens)
}

func TestSubagentInventory_RevalidatesInjectedRolloutBytes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "child.jsonl")
	ag := &CodexAgent{
		RolloutRoots: []string{},
		loadRollout: func(gotPath string) (loadedRollout, error) {
			if gotPath == "" {
				return loadedRollout{}, errors.New("empty path")
			}
			require.Equal(t, path, gotPath)
			return loadedRollout{Path: path, Data: rolloutData(t, "other-child", nil)}, nil
		},
	}

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{{
		AgentID:                "child",
		DeclaredTranscriptPath: path,
	}})
	require.NoError(t, err)
	require.Empty(t, result.Children[0].ResolvedPath)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
}

func TestSubagentInventory_BatchesFallbackTraversal(t *testing.T) {
	t.Parallel()

	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := writeRollout(t, firstRoot, "first.jsonl", "first", []json.RawMessage{tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1}})})
	second := writeRollout(t, secondRoot, "second.jsonl", "second", []json.RawMessage{tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 2, "cached_input_tokens": 0, "output_tokens": 1}})})
	_ = first
	_ = second
	walks := 0
	ag := &CodexAgent{
		RolloutRoots: []string{firstRoot, secondRoot},
		walkDir: func(root string, visit fs.WalkDirFunc) error {
			walks++
			return filepath.WalkDir(root, visit)
		},
	}

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{{AgentID: "first"}, {AgentID: "second"}})
	require.NoError(t, err)
	require.Equal(t, 2, walks, "one traversal per configured root, not per child")
	require.Equal(t, []string{"first", "second"}, []string{result.Children[0].AgentID, result.Children[1].AgentID})
	require.True(t, *result.TokenUsage.SubagentTokensComplete)
}

func TestSubagentInventory_FallbackTraversalFailureDiscardsMatches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeRollout(t, root, "child.jsonl", "child", []json.RawMessage{tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1}})})
	traversalFailure := errors.New("injected traversal failure after match")
	ag := &CodexAgent{
		RolloutRoots: []string{root},
		walkDir: func(root string, visit fs.WalkDirFunc) error {
			require.NoError(t, filepath.WalkDir(root, visit))
			return traversalFailure
		},
	}

	result, err := ag.ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{{AgentID: "child"}})
	require.NoError(t, err)
	require.Empty(t, result.Children[0].ResolvedPath)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
	require.Nil(t, result.TokenUsage.SubagentTokens)
}

func writeRollout(t *testing.T, root, name, id string, events []json.RawMessage) string {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, rolloutData(t, id, events), 0o600))
	return path
}

func rolloutData(t *testing.T, id string, events []json.RawMessage) []byte {
	t.Helper()
	lines := make([][]byte, 0, len(events)+1)
	meta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id}})
	require.NoError(t, err)
	lines = append(lines, meta)
	for _, event := range events {
		lines = append(lines, event)
	}
	return append([]byte(joinLines(lines)), '\n')
}

func tokenCountEvent(info map[string]any) json.RawMessage {
	data, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": info}})
	if err != nil {
		panic(err)
	}
	return data
}

func taskEvent(eventType string, turnID *string) json.RawMessage {
	payload := map[string]any{"type": eventType}
	if turnID != nil {
		payload["turn_id"] = *turnID
	}
	data, err := json.Marshal(map[string]any{"type": "event_msg", "payload": payload})
	if err != nil {
		panic(err)
	}
	return data
}

func stringPointer(value string) *string { return &value }

func patchEvent(path string) json.RawMessage {
	data, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": "*** Update File: " + path}})
	if err != nil {
		panic(err)
	}
	return data
}

func joinLines(lines [][]byte) string {
	var result strings.Builder
	for index, line := range lines {
		if index > 0 {
			result.WriteByte('\n')
		}
		result.Write(line)
	}
	return result.String()
}
