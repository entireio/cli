package agentimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// These regression tests pin the trail-817 fix: the subagent-aware importers
// (Claude Code, Factory AI Droid) get their per-turn SubagentTokens as a
// cumulative-since-session-start snapshot (agent IDs are discovered from the
// full transcript prefix and each subagent transcript is re-read from line 0),
// so a subagent spawned in an early turn repeats its full total on every later
// turn. Both import consumers sum per-turn usage — writeSessionState for the
// session total and the per-imported-checkpoint TokenUsage that downstream sums
// — so before the fix a subagent's tokens were multiplied by the number of
// turns after it was first discovered. rescopeSubagentTokensToDeltas
// (linesplit.go) rescopes those snapshots to per-turn deltas so the total is
// counted exactly once. Reverting that call makes both assertions below fail.

const (
	// The single spawned subagent's on-disk transcript totals, asserted to be
	// counted exactly once across a 3-turn session.
	wantSubagentInput  = 50
	wantSubagentOutput = 25
	wantSubagentCalls  = 1

	// Main-agent totals summed across the three turns (per-slice deltas), kept
	// intact by the fix.
	wantMainInput  = 600 // 100 + 200 + 300
	wantMainOutput = 300 // 50 + 100 + 150
	wantMainCalls  = 3
)

// sumTurnSubagentTokens sums each turn's SubagentTokens the way both the session
// total and any downstream sum of per-checkpoint TokenUsage would. With the fix
// the turns hold per-turn deltas, so this reconstructs the subagent total once;
// without it each turn holds the cumulative snapshot and this multiplies.
func sumTurnSubagentTokens(turns []Turn) types.TokenUsage {
	var sum types.TokenUsage
	for _, tr := range turns {
		if tr.Tokens == nil || tr.Tokens.SubagentTokens == nil {
			continue
		}
		s := tr.Tokens.SubagentTokens
		sum.InputTokens += s.InputTokens
		sum.CacheCreationTokens += s.CacheCreationTokens
		sum.CacheReadTokens += s.CacheReadTokens
		sum.OutputTokens += s.OutputTokens
		sum.APICallCount += s.APICallCount
	}
	return sum
}

func assertSubagentCountedOnce(t *testing.T, label string, got *types.TokenUsage) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: SubagentTokens is nil, want input=%d output=%d calls=%d",
			label, wantSubagentInput, wantSubagentOutput, wantSubagentCalls)
	}
	if got.InputTokens != wantSubagentInput || got.OutputTokens != wantSubagentOutput ||
		got.APICallCount != wantSubagentCalls {
		t.Errorf("%s: subagent tokens counted more than once: got input=%d output=%d calls=%d, "+
			"want input=%d output=%d calls=%d (cumulative snapshot summed across turns)",
			label, got.InputTokens, got.OutputTokens, got.APICallCount,
			wantSubagentInput, wantSubagentOutput, wantSubagentCalls)
	}
}

func writeSubagentTranscript(t *testing.T, sf SessionFile, agentID, line string) {
	t.Helper()
	subagentsDir := filepath.Join(filepath.Dir(sf.Path), sf.SessionID, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents dir: %v", err)
	}
	agentPath := filepath.Join(subagentsDir, "agent-"+agentID+".jsonl")
	if err := os.WriteFile(agentPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}
}

// TestRescopeSubagentTokensToDeltas_NilCumulativeThenReappears pins finding
// 019f5ebc-cf27: when a turn's cumulative SubagentTokens snapshot is transiently
// nil (the subagent's agent-<id>.jsonl failed to read, so CalculateTotalTokenUsage
// continue-d past it and left SubagentTokens nil) and a later turn's snapshot
// reappears non-nil, prevCumulative must NOT be reset to nil for the nil turn —
// otherwise SubtractTokenUsage(cumulative, nil) on the reappearing turn returns
// the full cumulative again and reintroduces the double-counting the PR fixes.
// The deltas must still sum to the final cumulative exactly once.
func TestRescopeSubagentTokensToDeltas_NilCumulativeThenReappears(t *testing.T) {
	turns := []Turn{
		{Tokens: &types.TokenUsage{InputTokens: 10, SubagentTokens: &types.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1}}},
		// Transient read failure: main-agent tokens present, subagent snapshot nil.
		{Tokens: &types.TokenUsage{InputTokens: 20}},
		// Snapshot reappears, having grown to 300/150.
		{Tokens: &types.TokenUsage{InputTokens: 30, SubagentTokens: &types.TokenUsage{InputTokens: 300, OutputTokens: 150, APICallCount: 3}}},
	}

	rescopeSubagentTokensToDeltas(turns)

	// Turn 0 delta = 100-0 = 100.
	if turns[0].Tokens.SubagentTokens == nil || turns[0].Tokens.SubagentTokens.InputTokens != 100 {
		t.Fatalf("turn0 subagent delta = %#v, want input=100", turns[0].Tokens.SubagentTokens)
	}
	// Turn 1 had a nil snapshot: its delta stays nil.
	if turns[1].Tokens.SubagentTokens != nil {
		t.Fatalf("turn1 subagent delta = %#v, want nil", turns[1].Tokens.SubagentTokens)
	}
	// Turn 2 delta must be rescoped against turn 0's cumulative (100), NOT nil:
	// 300-100 = 200, not the full 300.
	if turns[2].Tokens.SubagentTokens == nil || turns[2].Tokens.SubagentTokens.InputTokens != 200 {
		t.Fatalf("turn2 subagent delta = %#v, want input=200 (300 cumulative minus turn0 baseline 100)",
			turns[2].Tokens.SubagentTokens)
	}

	// The per-turn deltas must sum to the final cumulative (300) exactly once.
	sum := sumTurnSubagentTokens(turns)
	if sum.InputTokens != 300 || sum.OutputTokens != 150 || sum.APICallCount != 3 {
		t.Fatalf("summed subagent deltas = input=%d output=%d calls=%d, want 300/150/3 (counted once)",
			sum.InputTokens, sum.OutputTokens, sum.APICallCount)
	}
}

// TestImport_ClaudeSubagentTokensCountedOnceAcrossTurns builds a Claude session
// where a subagent is spawned in the first turn and two more user-prompt turns
// follow, then asserts the subagent's tokens are counted exactly once both in
// the summed per-turn/per-checkpoint usage and in the imported session total.
func TestImport_ClaudeSubagentTokensCountedOnceAcrossTurns(t *testing.T) {
	importRepo(t) // chdir into a repo for session-state storage; no t.Parallel (t.Chdir)

	dir := t.TempDir()
	sf := SessionFile{Path: filepath.Join(dir, "s.jsonl"), SessionID: "s"}

	// Turn 1 spawns subagent "subX" (Task tool_use + tool_result carrying the
	// agentId), then three user-prompt turns each with their own assistant
	// usage. The tool_result line is type "user" but has no text, so it does
	// not start a turn.
	full := []byte(strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a0","message":{"content":[{"type":"tool_use","id":"toolu_task1","name":"Task","input":{"prompt":"go"}}]}}`,
		`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_task1","content":"agentId: subX"}]}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":"second"}}`,
		`{"type":"assistant","uuid":"a2","message":{"id":"m2","content":[{"type":"text","text":"ok2"}],"usage":{"input_tokens":200,"output_tokens":100}}}`,
		`{"type":"user","uuid":"u3","message":{"role":"user","content":"third"}}`,
		`{"type":"assistant","uuid":"a3","message":{"id":"m3","content":[{"type":"text","text":"ok3"}],"usage":{"input_tokens":300,"output_tokens":150}}}`,
	}, "\n") + "\n")
	if err := os.WriteFile(sf.Path, full, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	writeSubagentTranscript(t, sf, "subX",
		`{"type":"assistant","uuid":"sa1","message":{"id":"sm1","content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":50,"output_tokens":25}}}`)

	turns, err := claudeImporter{}.SplitTurns(sf, full)
	if err != nil {
		t.Fatalf("SplitTurns: %v", err)
	}
	assertSubagentTurns(t, claudeImporter{}, sf, turns)
}

// TestImport_FactorySubagentTokensCountedOnceAcrossTurns is the Factory AI Droid
// analogue: Droid envelopes, subagent spawned in the first turn, three prompt
// turns, subagent tokens counted exactly once.
func TestImport_FactorySubagentTokensCountedOnceAcrossTurns(t *testing.T) {
	importRepo(t)

	dir := t.TempDir()
	sf := SessionFile{Path: filepath.Join(dir, "s.jsonl"), SessionID: "s"}

	full := []byte(strings.Join([]string{
		`{"type":"message","id":"u1","message":{"role":"user","content":"first"}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_task1","name":"Task","input":{"prompt":"go"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_task1","content":"agentId: subX"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","id":"m1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"message","id":"u2","message":{"role":"user","content":"second"}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"ok2"}],"usage":{"input_tokens":200,"output_tokens":100}}}`,
		`{"type":"message","id":"u3","message":{"role":"user","content":"third"}}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","id":"m3","content":[{"type":"text","text":"ok3"}],"usage":{"input_tokens":300,"output_tokens":150}}}`,
	}, "\n") + "\n")
	if err := os.WriteFile(sf.Path, full, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	writeSubagentTranscript(t, sf, "subX",
		`{"type":"message","id":"se1","message":{"role":"assistant","id":"sm1","content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":50,"output_tokens":25}}}`)

	turns, err := factoryImporter{}.SplitTurns(sf, full)
	if err != nil {
		t.Fatalf("SplitTurns: %v", err)
	}
	assertSubagentTurns(t, factoryImporter{}, sf, turns)
}

// assertSubagentTurns runs the shared assertions for a 3-turn session with one
// spawned subagent: the summed per-turn (== per-checkpoint) subagent tokens and
// the imported session total each count the subagent exactly once, while the
// main-agent totals still sum across turns.
func assertSubagentTurns(t *testing.T, imp Importer, sf SessionFile, turns []Turn) {
	t.Helper()
	ctx := context.Background()
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d", len(turns))
	}

	// Per-checkpoint proof: summing each turn's stored TokenUsage.SubagentTokens
	// (which is exactly what writeTurn persists per imported checkpoint) must
	// reconstruct the subagent total once, not 3x.
	perCheckpoint := sumTurnSubagentTokens(turns)
	assertSubagentCountedOnce(t, "sum of per-turn SubagentTokens", &perCheckpoint)

	// Session-total proof: the imported session.State.TokenUsage folds the
	// turns via writeSessionState the same way production Run does.
	if err := writeSessionState(ctx, imp, sf, turns); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}
	st := loadState(t, sf.SessionID)
	if st == nil || st.TokenUsage == nil {
		t.Fatalf("no imported session token usage written: %+v", st)
	}
	assertSubagentCountedOnce(t, "session total SubagentTokens", st.TokenUsage.SubagentTokens)

	// The main-agent fields are genuine per-slice deltas and must still sum.
	if st.TokenUsage.InputTokens != wantMainInput || st.TokenUsage.OutputTokens != wantMainOutput ||
		st.TokenUsage.APICallCount != wantMainCalls {
		t.Errorf("main-agent totals = input=%d output=%d calls=%d, want input=%d output=%d calls=%d",
			st.TokenUsage.InputTokens, st.TokenUsage.OutputTokens, st.TokenUsage.APICallCount,
			wantMainInput, wantMainOutput, wantMainCalls)
	}
}
