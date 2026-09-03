package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{{AgentID: "active"}, {AgentID: "archived"}})
	require.NoError(t, err)
	require.Equal(t, activePath, result.Children[0].ResolvedPath)
	require.Equal(t, archivedPath, result.Children[1].ResolvedPath)
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
		child, _ := extractChild(t, ag, ref)
		require.Equal(t, exact, child.ResolvedPath)
	}
}

func TestResolveRollout_RejectsInferredAndAmbiguousCandidates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := filepath.Join(root, "sessions")
	ag := &CodexAgent{RolloutRoots: []string{active}}
	writeRollout(t, active, "2026/08/31/rollout-child.jsonl", "childish", nil)
	child, usage := extractChild(t, ag, agent.SubagentReference{AgentID: "child"})
	require.Empty(t, child.ResolvedPath)
	require.False(t, *usage.SubagentTokensComplete)

	writeRollout(t, active, "2026/08/30/rollout-child-one.jsonl", "child", nil)
	writeRollout(t, active, "2026/08/31/rollout-child-two.jsonl", "child", nil)
	child, _ = extractChild(t, ag, agent.SubagentReference{AgentID: "child"})
	require.Empty(t, child.ResolvedPath)
}

func TestResolveRollout_RejectsSymlinkHint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := writeRollout(t, root, "target.jsonl", "child", nil)
	link := filepath.Join(root, "child-link.jsonl")
	require.NoError(t, os.Symlink(target, link))

	ag := &CodexAgent{RolloutRoots: []string{}}
	child, _ := extractChild(t, ag, agent.SubagentReference{
		AgentID:                "child",
		DeclaredTranscriptPath: link,
	})
	require.Empty(t, child.ResolvedPath)
}

func TestTerminalTurnIDs_OnlyAcceptsUnambiguousBoundaries(t *testing.T) {
	t.Parallel()

	valid := []json.RawMessage{
		taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one")),
		taskEvent("task_started", stringPointer("two")), taskEvent("task_complete", nil),
	}
	require.Equal(t, []string{"one", "two"}, analyzeRollout(rolloutData(t, "child", valid), 0).TerminalTurnIDs)
	withUnknownEvent := append(append([]json.RawMessage(nil), valid...), json.RawMessage(`{"type":"event_msg","payload":{"type":"future_event","turn_id":7}}`))
	require.Equal(t, []string{"one", "two"}, analyzeRollout(rolloutData(t, "child", withUnknownEvent), 0).TerminalTurnIDs)

	tests := []struct {
		name   string
		events []json.RawMessage
	}{
		{"completion without start", []json.RawMessage{taskEvent("task_complete", stringPointer("one"))}},
		{"start without id", []json.RawMessage{taskEvent("task_started", nil)}},
		{"unclosed start", []json.RawMessage{taskEvent("task_started", stringPointer("one"))}},
		{"overlapping starts", []json.RawMessage{taskEvent("task_started", stringPointer("one")), taskEvent("task_started", stringPointer("two"))}},
		{"mismatched completion", []json.RawMessage{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("two"))}},
		{"duplicate turn", []json.RawMessage{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one")), taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", stringPointer("one"))}},
		{"duplicate completion", []json.RawMessage{taskEvent("task_started", stringPointer("one")), taskEvent("task_complete", nil), taskEvent("task_complete", nil)}},
		{"invalid id type", []json.RawMessage{json.RawMessage(`{"type":"event_msg","payload":{"type":"task_started","turn_id":7}}`)}},
		{"malformed tail", append(valid, json.RawMessage(`{"type":"event_msg","payload":{"type":"task_started","turn_id":`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Empty(t, analyzeRollout(rolloutData(t, "child", tt.events), 0).TerminalTurnIDs)
		})
	}
}

func TestExactTokenUsage_UsesOnlyLastRecognizableSnapshot(t *testing.T) {
	t.Parallel()

	valid := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 15, "cached_input_tokens": 12, "output_tokens": 3,
		"reasoning_output_tokens": 2, "total_tokens": 18,
	}})
	usage := analyzeRollout(rolloutData(t, "child", []json.RawMessage{valid}), 0).ExactTokenUsage
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 12, OutputTokens: 3}, usage)

	malformedLast := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 10, "cached_input_tokens": 11, "output_tokens": 3,
	}})
	require.Nil(t, analyzeRollout(rolloutData(t, "child", []json.RawMessage{valid, malformedLast}), 0).ExactTokenUsage)

	missingRequired := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{
		"input_tokens": 10, "output_tokens": 3,
	}})
	require.Nil(t, analyzeRollout(rolloutData(t, "child", []json.RawMessage{missingRequired}), 0).ExactTokenUsage)
}

func TestExactTokenUsage_RejectsEveryUnavailableOrInconsistentSnapshot(t *testing.T) {
	t.Parallel()

	valid := func(values map[string]any) []byte {
		return rolloutData(t, "child", []json.RawMessage{tokenCountEvent(map[string]any{"total_token_usage": values})})
	}
	require.Nil(t, analyzeRollout(rolloutData(t, "child", nil), 0).ExactTokenUsage)

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
		require.Nil(t, analyzeRollout(valid(values), 0).ExactTokenUsage)
	}

	zeros := analyzeRollout(valid(map[string]any{"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0}), 0).ExactTokenUsage
	require.Equal(t, &agent.TokenUsage{}, zeros)

	multiple := rolloutData(t, "child", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 9, "cached_input_tokens": 1, "output_tokens": 2}}),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 4, "cached_input_tokens": 1, "output_tokens": 2}}),
	})
	usage := analyzeRollout(multiple, 0).ExactTokenUsage
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 1, OutputTokens: 2}, usage)
	require.Zero(t, usage.APICallCount, "snapshot record count is not an API-call count")

	malformedFinal := rolloutData(t, "child", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 4, "cached_input_tokens": 1, "output_tokens": 2}}),
		tokenCountEvent(map[string]any{"total_token_usage": "not-an-object"}),
	})
	require.Nil(t, analyzeRollout(malformedFinal, 0).ExactTokenUsage, "must not fall back to the earlier valid snapshot")
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), parent, 0, []agent.SubagentReference{
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{
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

	root := t.TempDir()
	walks := 0
	ag := &CodexAgent{
		RolloutRoots: []string{root},
		walkDir: func(root string, visit fs.WalkDirFunc) error {
			walks++
			return filepath.WalkDir(root, visit)
		},
	}
	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, nil)
	require.NoError(t, err)
	require.Zero(t, walks, "an exact empty inventory has no unresolved child and must not scan rollout archives")
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{{
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{{
		AgentID:                "child",
		DeclaredTranscriptPath: path,
	}})
	require.NoError(t, err)
	require.Empty(t, result.Children[0].ResolvedPath)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
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

	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{{AgentID: "child"}})
	require.NoError(t, err)
	require.Empty(t, result.Children[0].ResolvedPath)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
	require.Nil(t, result.TokenUsage.SubagentTokens)
}

func TestRolloutScanLimits_Defaults(t *testing.T) {
	t.Parallel()

	require.Equal(t, 500*time.Millisecond, defaultRolloutScanLimits.timeout)
	require.Equal(t, 20_000, defaultRolloutScanLimits.candidateLimit)
	require.Equal(t, int64(64<<10), defaultRolloutScanLimits.metadataByteLimit)
	require.Equal(t, int64(128<<20), defaultRolloutScanLimits.bodyByteLimit)
	require.Equal(t, int64(256<<20), defaultRolloutScanLimits.aggregateByteLimit)
	require.Equal(t, 128, defaultRolloutScanLimits.readDirBatch)
}

func TestRolloutScanBudget_DefaultBoundaries(t *testing.T) {
	t.Parallel()

	start := time.Unix(1, 0)
	now := start
	limits := defaultRolloutScanLimits
	limits.now = func() time.Time { return now }

	candidates := newRolloutScanBudget(t.Context(), limits)
	for range limits.candidateLimit {
		require.NoError(t, candidates.observeCandidate())
	}
	require.ErrorIs(t, candidates.observeCandidate(), errRolloutScanBudget)

	bytes := newRolloutScanBudget(t.Context(), limits)
	require.NoError(t, bytes.observeBytes(limits.aggregateByteLimit))
	require.ErrorIs(t, bytes.observeBytes(1), errRolloutScanBudget)

	deadline := newRolloutScanBudget(t.Context(), limits)
	now = start.Add(limits.timeout - time.Nanosecond)
	require.NoError(t, deadline.check())
	now = start.Add(limits.timeout)
	require.ErrorIs(t, deadline.check(), errRolloutScanBudget)
}

func TestSubagentInventory_FallbackReadsOnlyMetadataForUnrelatedRollouts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	unrelated := writeRollout(t, root, "unrelated.jsonl", "unrelated", nil)
	require.NoError(t, os.WriteFile(unrelated, append(mustReadFile(t, unrelated), []byte(strings.Repeat("x", 1<<20))...), 0o600))
	writeRollout(t, root, "wanted.jsonl", "wanted", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 2, "cached_input_tokens": 0, "output_tokens": 1}}),
	})

	readByPath := make(map[string]int64)
	ag := &CodexAgent{
		RolloutRoots: []string{root},
		observeRolloutRead: func(path string, n int) {
			readByPath[path] += int64(n)
		},
	}
	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{{AgentID: "wanted"}})
	require.NoError(t, err)
	require.True(t, *result.TokenUsage.SubagentTokensComplete)
	require.Less(t, readByPath[unrelated], int64(4<<10), "unrelated rollout must not be read beyond its metadata prefix")
}

func TestRolloutScanLimits_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits rolloutScanLimits
		setup  func(*testing.T, string)
		refs   []agent.SubagentReference
	}{
		{
			name:   "candidate count",
			limits: testRolloutScanLimits(1, 64<<10, 128<<20, 256<<20),
			setup: func(t *testing.T, root string) {
				writeRollout(t, root, "one.jsonl", "one", nil)
				writeRollout(t, root, "two.jsonl", "two", nil)
			},
			refs: []agent.SubagentReference{{AgentID: "one"}, {AgentID: "two"}},
		},
		{
			name:   "metadata bytes",
			limits: testRolloutScanLimits(10, 8, 128<<20, 256<<20),
			setup: func(t *testing.T, root string) {
				writeRollout(t, root, "child.jsonl", "child", nil)
			},
			refs: []agent.SubagentReference{{AgentID: "child"}},
		},
		{
			name:   "body bytes",
			limits: testRolloutScanLimits(10, 64<<10, 80, 256<<20),
			setup: func(t *testing.T, root string) {
				writeRollout(t, root, "child.jsonl", "child", []json.RawMessage{patchEvent("child.txt")})
			},
			refs: []agent.SubagentReference{{AgentID: "child"}},
		},
		{
			name:   "aggregate bytes",
			limits: testRolloutScanLimits(10, 64<<10, 1<<20, 120),
			setup: func(t *testing.T, root string) {
				writeRollout(t, root, "one.jsonl", "one", []json.RawMessage{patchEvent("one.txt")})
				writeRollout(t, root, "two.jsonl", "two", []json.RawMessage{patchEvent("two.txt")})
			},
			refs: []agent.SubagentReference{{AgentID: "one"}, {AgentID: "two"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tt.setup(t, root)
			ag := &CodexAgent{RolloutRoots: []string{root}, scanLimits: &tt.limits}
			result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, tt.refs)
			require.NoError(t, err)
			require.NotNil(t, result.TokenUsage)
			require.Nil(t, result.TokenUsage.SubagentTokens)
			require.NotNil(t, result.TokenUsage.SubagentTokensComplete)
			require.False(t, *result.TokenUsage.SubagentTokensComplete)
			for _, child := range result.Children {
				require.Empty(t, child.ResolvedPath, "a scan breach must discard all partial fallback matches")
			}
		})
	}
}

func TestSubagentInventory_IncrementalCancellation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeRollout(t, root, "child.jsonl", "child", nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	result, err := ag.ExtractWithSubagentInventory(ctx, nil, 0, []agent.SubagentReference{{AgentID: "child"}})
	require.NoError(t, err)
	require.False(t, *result.TokenUsage.SubagentTokensComplete)
	require.Empty(t, result.Children[0].ResolvedPath)
}

func testRolloutScanLimits(candidateLimit int, metadata, body, aggregate int64) rolloutScanLimits {
	return rolloutScanLimits{
		timeout:            time.Hour,
		candidateLimit:     candidateLimit,
		metadataByteLimit:  metadata,
		bodyByteLimit:      body,
		aggregateByteLimit: aggregate,
		readDirBatch:       2,
		now:                time.Now,
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func extractChild(t *testing.T, ag *CodexAgent, ref agent.SubagentReference) (agent.SubagentAnalysis, *agent.TokenUsage) {
	t.Helper()
	result, err := ag.ExtractWithSubagentInventory(t.Context(), nil, 0, []agent.SubagentReference{ref})
	require.NoError(t, err)
	require.Len(t, result.Children, 1)
	return result.Children[0], result.TokenUsage
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
