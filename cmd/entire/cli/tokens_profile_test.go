package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/redact"
)

// Fixture models: one Anthropic row (prices 1h and 5m cache writes
// differently) and one OpenAI row (no cache-write charge).
const (
	tokensProfileTestClaudeModel = "claude-fable-5"
	tokensProfileTestCodexModel  = "gpt-5.3-codex"
	tokensProfileTestLegacyAgent = types.AgentType("Agent")
	tokensProfileTestMockAgent   = types.AgentType("Mock Lifecycle Agent")
)

// tokensProfileStubStore serves root summaries and per-session metadata from
// memory. A checkpoint absent from summaries reads as nil (unreadable root); a
// nil metadata entry fails ReadSessionMetadata.
type tokensProfileStubStore struct {
	summaries map[id.CheckpointID]*checkpoint.CheckpointSummary
	metas     map[id.CheckpointID][]*checkpoint.Metadata
}

func (s *tokensProfileStubStore) Read(_ context.Context, cpID id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	return s.summaries[cpID], nil
}

func (s *tokensProfileStubStore) ReadSessionMetadata(_ context.Context, cpID id.CheckpointID, i int) (*checkpoint.Metadata, error) {
	metas := s.metas[cpID]
	if i >= len(metas) || metas[i] == nil {
		return nil, errors.New("metadata unreadable")
	}
	return metas[i], nil
}

// tokensProfileFixture accumulates a stub store and its List order (newest
// first, as GitStore.List returns).
type tokensProfileFixture struct {
	store *tokensProfileStubStore
	infos []checkpoint.CheckpointInfo
	at    time.Time
}

func newTokensProfileFixture() *tokensProfileFixture {
	return &tokensProfileFixture{
		store: &tokensProfileStubStore{
			summaries: map[id.CheckpointID]*checkpoint.CheckpointSummary{},
			metas:     map[id.CheckpointID][]*checkpoint.Metadata{},
		},
		at: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	}
}

// add appends a checkpoint created one hour after the previous one. A nil
// meta marks that session's metadata unreadable. createdAt zero is passed
// through on both the info and the metadata when noCreatedAt is set.
func (f *tokensProfileFixture) add(cpID string, version int, noCreatedAt bool, metas ...*checkpoint.Metadata) {
	f.at = f.at.Add(time.Hour)
	created := f.at
	if noCreatedAt {
		created = time.Time{}
	}
	checkpointID := id.MustCheckpointID(cpID)
	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID, TokenUsageVersion: version}
	for _, meta := range metas {
		summary.Sessions = append(summary.Sessions, checkpoint.SessionFilePaths{Metadata: "metadata.json"})
		if meta != nil {
			meta.CheckpointID = checkpointID
			meta.CreatedAt = created
		}
	}
	f.store.summaries[checkpointID] = summary
	f.store.metas[checkpointID] = metas
	// Newest first.
	f.infos = append([]checkpoint.CheckpointInfo{{CheckpointID: checkpointID, CreatedAt: created}}, f.infos...)
}

// addUnreadable lists a checkpoint whose root summary cannot be read.
func (f *tokensProfileFixture) addUnreadable(cpID string) {
	f.at = f.at.Add(time.Hour)
	f.infos = append([]checkpoint.CheckpointInfo{{CheckpointID: id.MustCheckpointID(cpID), CreatedAt: f.at}}, f.infos...)
}

func tokensProfileMeta(agentType types.AgentType, model, sessionID string, usage *types.TokenUsage) *checkpoint.Metadata {
	return &checkpoint.Metadata{SessionID: sessionID, Agent: agentType, Model: model, TokenUsage: usage}
}

func withDuration(meta *checkpoint.Metadata, d time.Duration) *checkpoint.Metadata {
	meta.SessionMetrics = &checkpoint.SessionMetrics{DurationMs: d.Milliseconds()}
	return meta
}

// buildTokensProfileFixture is the standard fixture: six version-2 Claude Code
// checkpoints (two with durations, one with a nil usage, one with an
// unreadable second session, one with an unknown cache-write TTL), a legacy
// cumulative trio under the pre-agent-field "Agent" label with no model, two
// Codex checkpoints, one Mock Lifecycle Agent checkpoint, one checkpoint with
// no created_at, and one unreadable root.
func buildTokensProfileFixture() *tokensProfileFixture {
	f := newTokensProfileFixture()
	claude := func(sessionID string, usage *types.TokenUsage) *checkpoint.Metadata {
		return tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, sessionID, usage)
	}
	v2 := checkpoint.TokenUsageVersionDelta

	// Legacy trio: one session, running totals, oldest first.
	f.add("aaa000000001", 0, false, tokensProfileMeta(tokensProfileTestLegacyAgent, "", "legacy-session", &types.TokenUsage{CacheReadTokens: 10_000}))
	f.add("aaa000000002", 0, false, tokensProfileMeta(tokensProfileTestLegacyAgent, "", "legacy-session", &types.TokenUsage{CacheReadTokens: 20_000}))
	f.add("aaa000000003", 0, false, tokensProfileMeta(tokensProfileTestLegacyAgent, "", "legacy-session", &types.TokenUsage{CacheReadTokens: 30_000}))

	f.add("ccc000000001", v2, false, withDuration(claude("c1", &types.TokenUsage{CacheReadTokens: 100_000, OutputTokens: 1_000, ThinkingTokens: 500, APICallCount: 4}), 2*time.Hour))
	f.add("ccc000000002", v2, false, withDuration(claude("c2", &types.TokenUsage{CacheReadTokens: 200_000, OutputTokens: 2_000, APICallCount: 6}), 4*time.Hour))
	f.add("ccc000000003", v2, false, claude("c3", &types.TokenUsage{CacheCreationTokens: 10_000, CacheCreation1hTokens: 10_000, OutputTokens: 1_000, ThinkingTokens: 800, APICallCount: 2}))
	f.add("ccc000000004", v2, false, claude("c4", &types.TokenUsage{CacheCreationTokens: 10_000, OutputTokens: 100, APICallCount: 1}))
	f.add("ccc000000005", v2, false, claude("c5", nil))
	f.add("ccc000000006", v2, false, claude("c6", &types.TokenUsage{InputTokens: 300, OutputTokens: 500, ThinkingTokens: 100, APICallCount: 1}), nil)

	f.add("ddd000000001", v2, false, withDuration(tokensProfileMeta(agent.AgentTypeCodex, tokensProfileTestCodexModel, "x1", &types.TokenUsage{InputTokens: 10_000, CacheReadTokens: 50_000, OutputTokens: 1_000, ThinkingTokens: 400, APICallCount: 3}), time.Hour))
	f.add("ddd000000002", v2, false, tokensProfileMeta(agent.AgentTypeCodex, tokensProfileTestCodexModel, "x2", &types.TokenUsage{InputTokens: 1_000, OutputTokens: 3_000, APICallCount: 1}))

	f.add("eee000000001", v2, false, tokensProfileMeta(tokensProfileTestMockAgent, tokensProfileTestClaudeModel, "mock", &types.TokenUsage{InputTokens: 999_999, APICallCount: 1}))
	f.add("fff000000001", v2, true, claude("no-created-at", &types.TokenUsage{InputTokens: 777_777, APICallCount: 1}))
	f.addUnreadable("999000000001")
	return f
}

func renderTokensProfile(report tokensProfileReport) string {
	var b strings.Builder
	writeTokensProfileText(&b, report)
	return b.String()
}

func TestBuildTokensProfileReport_GroupsByAgentAndDedupesLegacy(t *testing.T) {
	t.Parallel()

	f := buildTokensProfileFixture()
	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}

	if report.CheckpointsAvailable != 14 || report.CheckpointsAnalyzed != 14 {
		t.Errorf("available/analysed = %d/%d, want 14/14", report.CheckpointsAvailable, report.CheckpointsAnalyzed)
	}
	if report.Collapsed != 2 || report.ExcludedTestAgents != 1 || report.MetadataReadWarnings != 1 {
		t.Errorf("collapsed=%d excluded=%d warnings=%d, want 2/1/1", report.Collapsed, report.ExcludedTestAgents, report.MetadataReadWarnings)
	}
	if report.TotalTokens != 419_900 || report.CheckpointsWithTokenData != 8 {
		t.Errorf("total=%d with_tokens=%d, want 419900/8", report.TotalTokens, report.CheckpointsWithTokenData)
	}
	if len(report.Agents) != 2 || report.Agents[0].Agent != string(agent.AgentTypeClaudeCode) || report.Agents[1].Agent != string(agent.AgentTypeCodex) {
		t.Fatalf("agents = %+v, want Claude Code then Codex", report.Agents)
	}

	claude := report.Agents[0]
	if claude.Checkpoints != 7 || claude.WithTokens != 6 || claude.Collapsed != 2 {
		t.Errorf("claude counts = %d/%d/%d, want 7/6/2", claude.Checkpoints, claude.WithTokens, claude.Collapsed)
	}
	if claude.TokensPerCheckpoint == nil || claude.TokensPerCheckpoint.Median != 11_000 || claude.TokensPerCheckpoint.P90 != 202_000 {
		t.Errorf("claude tokens/checkpoint = %+v, want median 11000 p90 202000", claude.TokensPerCheckpoint)
	}
	if claude.DurationSeconds.RecordedOn != 2 || claude.DurationSeconds.Median != 7200 || claude.DurationSeconds.P90 != 14400 {
		t.Errorf("claude duration = %+v, want 2 recorded, median 7200, p90 14400", claude.DurationSeconds)
	}
	if claude.TokensPerHourMedian != 50_500 {
		t.Errorf("claude tokens/hour median = %d, want 50500", claude.TokensPerHourMedian)
	}
	wantLargest := map[string]int{tokensProfileClassCacheRead: 2, tokensProfileClassOutput: 2, tokensProfileClassCacheWrite: 1}
	if len(claude.LargestCostClass) != len(wantLargest) {
		t.Errorf("claude largest cost class = %v, want %v", claude.LargestCostClass, wantLargest)
	}
	for class, n := range wantLargest {
		if claude.LargestCostClass[class] != n {
			t.Errorf("claude largest cost class[%s] = %d, want %d", class, claude.LargestCostClass[class], n)
		}
	}
	if claude.CostByClass == nil || claude.CostByClass.Priced != 5 || claude.CostByClass.CacheWriteRecordedOn != 2 || claude.CostByClass.CacheWrite1hRecordedOn != 1 || !claude.CostByClass.CacheWriteUnpriced {
		t.Errorf("claude cost by class = %+v", claude.CostByClass)
	}
	if claude.ThinkingShare.RecordedOn != 5 || claude.ThinkingShare.Median != 0.2 {
		t.Errorf("claude thinking share = %+v, want median 0.2 on 5", claude.ThinkingShare)
	}
	if claude.Effort != tokenNotRecorded {
		t.Errorf("claude effort = %q, want not recorded", claude.Effort)
	}
	if len(claude.WorthOpening) != 2 || claude.WorthOpening[0].CheckpointID != "ccc000000002" || claude.WorthOpening[1].CheckpointID != "ccc000000003" {
		t.Fatalf("claude worth opening = %+v, want c2 then c3", claude.WorthOpening)
	}
	if claude.WorthOpening[0].Standout != "cache read 67%" || claude.WorthOpening[1].Standout != "thinking 80%" {
		t.Errorf("claude standouts = %q / %q", claude.WorthOpening[0].Standout, claude.WorthOpening[1].Standout)
	}

	codex := report.Agents[1]
	if codex.Checkpoints != 2 || codex.WithTokens != 2 || codex.Collapsed != 0 {
		t.Errorf("codex counts = %d/%d/%d, want 2/2/0", codex.Checkpoints, codex.WithTokens, codex.Collapsed)
	}
	if codex.TokensPerCheckpoint == nil || codex.TokensPerCheckpoint.Median != 4_000 || codex.TokensPerCheckpoint.P90 != 61_000 {
		t.Errorf("codex tokens/checkpoint = %+v", codex.TokensPerCheckpoint)
	}
	if codex.CostByClass == nil || codex.CostByClass.CacheWriteRecordedOn != 0 || codex.CostByClass.CacheWriteUnpriced {
		t.Errorf("codex cost by class = %+v, want no TTL bookkeeping", codex.CostByClass)
	}
	if len(codex.WorthOpening) != 2 || codex.WorthOpening[0].Standout != "output 96%" || codex.WorthOpening[1].Standout != "input 43%" {
		t.Errorf("codex worth opening = %+v", codex.WorthOpening)
	}

	assertContainsAll(t, strings.Join(report.Limitations, "\n"),
		"1 checkpoint skipped: no created_at recorded",
		"1 checkpoint could not be read",
		"1 checkpoint had unreadable session metadata",
		"1 checkpoint has no recorded model",
		"Cache writes on 1 checkpoint have no recorded TTL",
		"Recurring contributors are not computed for profiles (no transcripts are read).",
		"Cost shares use list-price ratios per model family, not your plan's rates.",
		"Legacy checkpoints (no token_usage_version) were deduped per session; 2 legacy running-total rows collapsed.",
	)
	for _, l := range report.Limitations {
		if strings.Contains(l, "fff000000001") || strings.Contains(l, "Limited to latest") {
			t.Errorf("unexpected limitation %q", l)
		}
	}
}

func TestWriteTokensProfileText_Layout(t *testing.T) {
	t.Parallel()

	f := buildTokensProfileFixture()
	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	out := renderTokensProfile(report)

	if got, want := tokensProfileHeader(report), "Token profile — last 14 committed checkpoints (14 available; 2 overlapping checkpoints collapsed, 1 test-agent checkpoint excluded)"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	assertContainsAll(t, out,
		"Token profile — last 14 committed checkpoints (14 available; 2 overlapping\ncheckpoints collapsed, 1 test-agent checkpoint excluded)",
		"Claude Code · 7 checkpoints (6 with tokens; 2 overlapping collapsed)",
		"  duration / session         median  2h 00m   p90  4h 00m      tokens per hour  median 50.5k   (recorded on 2 of 7)",
		"  tokens / checkpoint        median  11k    p90  202k   (recorded on 6 of 7)",
		"  largest cost class         cache read in 2 · output in 2 · cache write in 1",
		"  cost by class (sum)        cache read 41% · output 31% · cache write 27% (1-hour on 1 of 2 recorded) · input <1%",
		"  thinking share of output   median 20%   (recorded on 5 of 7)",
		"  effort                     not recorded",
		"  Worth opening   ccc000000002 (202k, cache read 67%)\n                  ccc000000003 (11k, thinking 80%)\n                  → entire checkpoint tokens <id>\n",
		"Codex · 2 checkpoints",
		"  duration / session         median  1h 00m   p90  1h 00m      tokens per hour  median 61k   (recorded on 1 of 2)",
		"  tokens / checkpoint        median  4k    p90  61k",
		"  Worth opening   ddd000000002 (4k, output 96%)\n                  ddd000000001 (61k, input 43%)\n                  → entire checkpoint tokens <id>\n",
		"Notes",
		"  - Legacy checkpoints (no token_usage_version) were deduped per session; 2 legacy",
		"  - 1 checkpoint has no recorded model and is counted by volume only.",
		"  - Recurring contributors are not computed for profiles (no transcripts are read).",
		"  - Total: 419.9k tokens (sum after collapsing overlaps).",
	)
	for _, absent := range []string{"Recommendations", "Repeated signals", "Mock Lifecycle Agent", "Patterns", "remember:"} {
		if strings.Contains(out, absent) {
			t.Errorf("output must not contain %q:\n%s", absent, out)
		}
	}
	if strings.Index(out, "Claude Code ·") > strings.Index(out, "Codex ·") {
		t.Errorf("agents must be ordered by checkpoint count:\n%s", out)
	}
	if strings.Count(out, "→ entire checkpoint tokens <id>") != 2 {
		t.Errorf("expected one hint per agent block:\n%s", out)
	}
}

func TestBuildTokensProfileReport_LimitAndZeroCreatedAt(t *testing.T) {
	t.Parallel()

	f := buildTokensProfileFixture()
	// Newest three: the unreadable root, the no-created_at row, the mock row.
	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 3)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	if report.CheckpointsAvailable != 14 || report.CheckpointsAnalyzed != 3 {
		t.Errorf("available/analysed = %d/%d, want 14/3", report.CheckpointsAvailable, report.CheckpointsAnalyzed)
	}
	if len(report.Agents) != 0 || report.TotalTokens != 0 || report.ExcludedTestAgents != 1 {
		t.Errorf("agents=%+v total=%d excluded=%d, want none/0/1", report.Agents, report.TotalTokens, report.ExcludedTestAgents)
	}
	assertContainsAll(t, strings.Join(report.Limitations, "\n"),
		"Limited to latest 3 of 14 committed checkpoints; use --limit or --all to change scope.",
		"1 checkpoint skipped: no created_at recorded",
	)
	out := renderTokensProfile(report)
	assertContainsAll(t, out, "Token profile — last 3 committed checkpoints (14 available; 1 test-agent\ncheckpoint excluded)", "Total: 0 tokens")
}

func TestBuildTokensProfileReport_ZeroCreatedAtNeverReachesDedupe(t *testing.T) {
	t.Parallel()

	// A legacy pair whose later running total has no created_at: with the
	// zero time fed to dedupe it would sort first and be collapsed, leaving
	// the smaller row; skipping it keeps the earlier row and collapses none.
	f := newTokensProfileFixture()
	f.add("bbb000000001", 0, false, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "s", &types.TokenUsage{InputTokens: 100}))
	f.add("bbb000000002", 0, true, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "s", &types.TokenUsage{InputTokens: 300}))
	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	if report.Collapsed != 0 || report.TotalTokens != 100 || len(report.Agents) != 1 || report.Agents[0].Checkpoints != 1 {
		t.Errorf("collapsed=%d total=%d agents=%+v; want the zero-created_at row skipped, not collapsed", report.Collapsed, report.TotalTokens, report.Agents)
	}
}

func TestBuildTokensProfileReport_ThinkingShareRule(t *testing.T) {
	t.Parallel()

	f := newTokensProfileFixture()
	// Legacy Claude row with thinking 0: cannot tell "0" from "absent" → not recorded.
	f.add("abc000000001", 0, false, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "l0", &types.TokenUsage{OutputTokens: 100}))
	// Legacy Claude row with thinking > 0: recorded.
	f.add("abc000000002", 0, false, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "l1", &types.TokenUsage{OutputTokens: 100, ThinkingTokens: 50}))
	// Version-2 row with thinking 0: the field is written, so 0 means 0.
	f.add("abc000000003", checkpoint.TokenUsageVersionDelta, false, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "v2", &types.TokenUsage{OutputTokens: 100}))
	// Pi never records thinking, whatever the version.
	f.add("abc000000004", checkpoint.TokenUsageVersionDelta, false, tokensProfileMeta(agent.AgentTypePi, tokensProfileTestClaudeModel, "pi", &types.TokenUsage{OutputTokens: 100, ThinkingTokens: 50}))

	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	if len(report.Agents) != 2 {
		t.Fatalf("agents = %+v, want Claude Code and Pi", report.Agents)
	}
	claude, pi := report.Agents[0], report.Agents[1]
	if claude.ThinkingShare.RecordedOn != 2 || claude.ThinkingShare.Median != 0 {
		t.Errorf("claude thinking = %+v, want recorded on 2 (legacy>0 and v2), lower median 0", claude.ThinkingShare)
	}
	if pi.ThinkingShare.RecordedOn != 0 {
		t.Errorf("pi thinking = %+v, want not recorded", pi.ThinkingShare)
	}
	out := renderTokensProfile(report)
	assertContainsAll(t, out, "  thinking share of output   median 0%   (recorded on 2 of 3)", "  thinking share of output   not recorded")
}

func TestBuildTokensProfileReport_AllCacheWritesUnknownTTL(t *testing.T) {
	t.Parallel()

	// The dominant real-data shape: Anthropic rows with cache writes and no
	// 1h split, so no cache write can be priced.
	f := newTokensProfileFixture()
	for i, cpID := range []string{"cad000000001", "cad000000002"} {
		f.add(cpID, checkpoint.TokenUsageVersionDelta, false, tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, "s"+string(rune('a'+i)),
			&types.TokenUsage{InputTokens: 1_000, CacheCreationTokens: 50_000, CacheReadTokens: 200_000, OutputTokens: 2_000, APICallCount: 5}))
	}
	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	if len(report.Agents) != 1 || report.Agents[0].CostByClass == nil {
		t.Fatalf("agents = %+v, want one priced Claude Code block", report.Agents)
	}
	cost := report.Agents[0].CostByClass
	if cost.CacheWrite != 0 || !cost.CacheWriteUnpriced || cost.CacheWriteRecordedOn != 2 || cost.CacheWrite1hRecordedOn != 0 || cost.Priced != 2 {
		t.Errorf("cost by class = %+v, want cache write 0, unpriced, recorded on 2 with no 1h split", cost)
	}
	assertContainsAll(t, renderTokensProfile(report),
		"  cost by class (sum)        cache read 65% · output 32% · input 3% · cache write not priced (TTL not recorded)",
		"  - Cache writes on 2 checkpoints have no recorded TTL and are not priced.",
	)
}

func TestBuildTokensProfileReport_TwoSessionCheckpointAndDurationDenominator(t *testing.T) {
	t.Parallel()

	claude := func(sessionID string, usage *types.TokenUsage, d time.Duration) *checkpoint.Metadata {
		return withDuration(tokensProfileMeta(agent.AgentTypeClaudeCode, tokensProfileTestClaudeModel, sessionID, usage), d)
	}
	f := newTokensProfileFixture()
	f.add("dad000000001", checkpoint.TokenUsageVersionDelta, false, claude("one", &types.TokenUsage{InputTokens: 1_000}, time.Hour))
	// Two readable sessions: one checkpoint, volume and duration summed.
	f.add("dad000000002", checkpoint.TokenUsageVersionDelta, false,
		claude("two-a", &types.TokenUsage{InputTokens: 2_000}, time.Hour),
		claude("two-b", &types.TokenUsage{InputTokens: 3_000}, 2*time.Hour))
	// A duration with no tokens still counts toward "recorded on".
	f.add("dad000000003", checkpoint.TokenUsageVersionDelta, false, claude("three", nil, 30*time.Minute))

	report, err := buildTokensProfileReport(context.Background(), f.store, f.infos, 50)
	if err != nil {
		t.Fatalf("buildTokensProfileReport: %v", err)
	}
	if len(report.Agents) != 1 {
		t.Fatalf("agents = %+v, want one", report.Agents)
	}
	a := report.Agents[0]
	if a.Checkpoints != 3 || a.WithTokens != 2 || report.TotalTokens != 6_000 {
		t.Errorf("checkpoints=%d with_tokens=%d total=%d, want 3/2/6000", a.Checkpoints, a.WithTokens, report.TotalTokens)
	}
	if a.DurationSeconds.RecordedOn != 3 || a.DurationSeconds.Median != 3600 || a.DurationSeconds.P90 != 10_800 {
		t.Errorf("duration = %+v, want recorded on 3, median 1h, p90 3h", a.DurationSeconds)
	}
	// Tokens per hour is over the with-tokens subset: 1000/1h and 5000/3h.
	if a.TokensPerHourMedian != 1_000 {
		t.Errorf("tokens/hour median = %d, want 1000", a.TokensPerHourMedian)
	}
	if len(a.WorthOpening) != 2 || a.WorthOpening[0].CheckpointID != "dad000000002" || a.WorthOpening[0].Tokens != 5_000 {
		t.Errorf("worth opening = %+v, want the two-session checkpoint first with 5000 tokens", a.WorthOpening)
	}
	out := renderTokensProfile(report)
	assertContainsAll(t, out,
		"Claude Code · 3 checkpoints (2 with tokens)",
		"  duration / session         median  1h 00m   p90  3h 00m      tokens per hour  median 1k\n",
		"  tokens / checkpoint        median  1k    p90  5k   (recorded on 2 of 3)",
	)
}

func TestBuildTokensProfileReport_ContextCancellation(t *testing.T) {
	t.Parallel()

	f := buildTokensProfileFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildTokensProfileReport(ctx, f.store, f.infos, 50); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTokensProfileCmd_TextAndJSONFromCommittedStore(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000001", "profile-one", agent.AgentTypeClaudeCode, &agent.TokenUsage{
		InputTokens: 100, CacheCreationTokens: 100, CacheCreation1hTokens: 100, CacheReadTokens: 800, OutputTokens: 20, ThinkingTokens: 10, APICallCount: 5,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000002", "profile-two", agent.AgentTypeClaudeCode, &agent.TokenUsage{
		InputTokens: 400, OutputTokens: 100, APICallCount: 25,
		SubagentTokens: &agent.TokenUsage{InputTokens: 1_000, Model: tokensProfileTestClaudeModel},
	})
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000003", "profile-missing", agent.AgentTypeClaudeCode, nil)
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000004", "profile-mock", tokensProfileTestMockAgent, &agent.TokenUsage{InputTokens: 5, APICallCount: 1})

	out := runTokensProfileCmd(ctx, t)
	assertContainsAll(t, out,
		"Token profile — last 4 committed checkpoints (4 available; 1 test-agent\ncheckpoint excluded)",
		"Claude Code · 3 checkpoints (2 with tokens)",
		"  tokens / checkpoint        median  1k    p90  1.5k   (recorded on 2 of 3)",
		"  duration / session         not recorded",
		"  largest cost class         input in 1 · cache write in 1",
		"(1-hour on 1 of 1 recorded)",
		"  thinking share of output   median 0%   (recorded on 2 of 3)",
		"  effort                     not recorded",
		"  Worth opening   100aaa000002 (1.5k, input 74%)\n                  100aaa000001 (1k, cache write 42%)\n                  → entire checkpoint tokens <id>\n",
		"Recurring contributors are not computed for profiles",
		"Total: 2.5k tokens (sum after collapsing overlaps).",
	)
	if strings.Contains(out, "Recommendations") || strings.Contains(out, "collapsed") {
		t.Errorf("unexpected section:\n%s", out)
	}

	raw := runTokensProfileCmd(ctx, t, "--json")
	var result tokensProfileReport
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, raw)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("expected a JSON object: %v", err)
	}
	for _, key := range []string{"source", "checkpoints_available", "checkpoints_analyzed", "checkpoints_with_token_data", "collapsed", "excluded_test_agents", "agents", "total_tokens"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing top-level key %q in %s", key, raw)
		}
	}
	for _, key := range []string{"signals", "recommendations", "tokens", "usage_scope", "missing_token_data"} {
		if _, ok := doc[key]; ok {
			t.Errorf("removed key %q still present in %s", key, raw)
		}
	}
	if result.CheckpointsAnalyzed != 4 || result.ExcludedTestAgents != 1 || result.TotalTokens != 2_520 || len(result.Agents) != 1 {
		t.Errorf("report = %+v", result)
	}
	a := result.Agents[0]
	if a.Agent != string(agent.AgentTypeClaudeCode) || a.Checkpoints != 3 || a.WithTokens != 2 || a.Effort != tokenNotRecorded || len(a.WorthOpening) != 2 {
		t.Errorf("agent = %+v", a)
	}
	if a.CostByClass == nil || a.CostByClass.CacheWrite1hRecordedOn != 1 || a.DurationSeconds.RecordedOn != 0 {
		t.Errorf("agent cost/duration = %+v / %+v", a.CostByClass, a.DurationSeconds)
	}
}

func TestTokensProfileCmd_LimitScopesAnalyzedCheckpoints(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	for i, cpID := range []string{"300ccc000001", "300ccc000002", "300ccc000003"} {
		writeProfileTokenCheckpoint(ctx, t, store, cpID, "profile-limit-"+string(rune('a'+i)), agent.AgentTypeClaudeCode, &agent.TokenUsage{InputTokens: 100, OutputTokens: 100, APICallCount: 1})
	}

	out := runTokensProfileCmd(ctx, t, "--limit", "2")
	assertContainsAll(t, out,
		"Token profile — last 2 committed checkpoints (3 available)",
		"Claude Code · 2 checkpoints",
		"Total: 400 tokens (sum after collapsing overlaps).",
		"Limited to latest 2 of 3 committed checkpoints; use --limit or --all to change scope.",
	)

	all := runTokensProfileCmd(ctx, t, "--all")
	assertContainsAll(t, all, "Token profile — last 3 committed checkpoints (3 available)", "Total: 600 tokens")
}

func TestTokensProfileCmd_LimitAndAllAreMutuallyExclusive(t *testing.T) {
	runExplainAutoTestRepo(t)

	cmd := newTokensGroupCmd()
	cmd.SetArgs([]string{"profile", "--limit", "2", "--all"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --limit with --all")
	}
	if !strings.Contains(err.Error(), "limit") || !strings.Contains(err.Error(), "all") {
		t.Fatalf("expected error to mention limit and all, got: %v", err)
	}
}

func TestTokensProfileCmd_EmptyHistory(t *testing.T) {
	runExplainAutoTestRepo(t)

	out := runTokensProfileCmd(context.Background(), t)
	assertContainsAll(t, out,
		"Token profile — last 0 committed checkpoints (0 available)",
		"No committed checkpoints found.",
	)
	if strings.Contains(out, "Worth opening") || strings.Contains(out, "Recommendations") {
		t.Errorf("unexpected section:\n%s", out)
	}
}

// runTokensProfileCmd runs `tokens profile` with args and returns stdout.
func runTokensProfileCmd(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs(append([]string{"profile"}, args...))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("tokens profile %v: %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

func writeProfileTokenCheckpoint(ctx context.Context, t *testing.T, store *checkpoint.GitStore, checkpointID string, sessionID string, agentType types.AgentType, usage *agent.TokenUsage) {
	t.Helper()

	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: id.MustCheckpointID(checkpointID),
		SessionID:    sessionID,
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-profile",
		Agent:        agentType,
		Model:        tokensProfileTestClaudeModel,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"profile"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage:   usage,
	}); err != nil {
		t.Fatalf("WriteCommitted(%s) error = %v", checkpointID, err)
	}
}

// Bare `entire tokens` is the current session's report, exactly as
// `entire session tokens --current` renders it (spec §4.4). Chdir'd into a
// fresh repo so the current-worktree filter has a defined answer.
func TestTokensGroupCmd_BareRunsCurrentSessionReport(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	const sessionID = "bare-tokens-current"
	state := liveSessionTokensState(sessionID, writeSessionTokensTranscript(t, sessionID, ""))
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}
	state.WorktreePath = root
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensGroupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("entire tokens: %v\n%s", err, stdout.String())
	}

	assertContainsAll(t, stdout.String(), "Session tokens", sessionID, "Where it went")
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Commands:") {
		t.Errorf("bare entire tokens printed help instead of the report:\n%s", stdout.String())
	}

	// Parity: the bare command is `session tokens --current`, byte for byte.
	current := newTokensCmd()
	var currentOut bytes.Buffer
	current.SetOut(&currentOut)
	current.SetArgs([]string{"--current"})
	if err := current.ExecuteContext(ctx); err != nil {
		t.Fatalf("session tokens --current: %v", err)
	}
	if stdout.String() != currentOut.String() {
		t.Errorf("bare entire tokens differs from session tokens --current:\n--- bare\n%s\n--- --current\n%s", stdout.String(), currentOut.String())
	}
}

// The group command takes no flags and no arguments of its own: the report's
// flags live on `session tokens`, and a stray word must not silently run the
// bare report.
func TestTokensGroupCmd_BareRejectsArgsAndFlags(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"bogus"}, {"--json"}, {"--agent-brief"}} {
		cmd := newTokensGroupCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Errorf("entire tokens %v should error", args)
		}
	}
}

// Without a session in the worktree the bare command prints the one-line hint
// and exits 0 — plain text, no picker, nothing on stderr (spec §4.4).
func TestTokensGroupCmd_BareWithoutSessionPrintsHint(t *testing.T) {
	setupStopTestRepo(t)

	cmd := newTokensGroupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("entire tokens without a session should exit 0, got: %v", err)
	}

	const want = "no active session — try 'entire checkpoint tokens <id>' or 'entire tokens profile'\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", stderr.String())
	}
}
