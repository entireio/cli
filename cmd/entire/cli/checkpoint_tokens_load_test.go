package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// testSkillSystematicDebugging is the skill the Claude fixture's first call runs under.
const testSkillSystematicDebugging = "systematic-debugging"

// tokenTestSession builds a stub session with the given metadata and
// transcript for the stub reader.
func tokenTestSession(meta checkpoint.Metadata, transcript []byte) *checkpoint.SessionContent {
	return &checkpoint.SessionContent{Metadata: meta, Transcript: transcript}
}

// tokenTestSummary builds a summary with n sessions at version.
func tokenTestSummary(n, version int) *checkpoint.CheckpointSummary {
	s := &checkpoint.CheckpointSummary{TokenUsageVersion: version, Branch: "stub-branch"}
	for i := range n {
		s.Sessions = append(s.Sessions, checkpoint.SessionFilePaths{Metadata: strings.Repeat("x", i+1) + "/metadata.json"})
	}
	return s
}

// buildStubTokensReport loads and builds a report from a stub reader.
func buildStubTokensReport(t *testing.T, reader *stubCommittedReader, records []checkpoint.StoredTaskRecord) checkpointTokensReport {
	t.Helper()
	ctx := context.Background()
	cpID := id.MustCheckpointID("abc123abc123")
	inputs, err := loadCheckpointTokenInputs(ctx, reader, cpID, reader.summary)
	if err != nil {
		t.Fatalf("loadCheckpointTokenInputs: %v", err)
	}
	inputs.records = append(inputs.records, records...)
	return buildCheckpointTokensReport(ctx, inputs)
}

func renderTokensReport(report *checkpointTokensReport) string {
	var b strings.Builder
	writeCheckpointTokensText(&b, report)
	return b.String()
}

func TestBuildCheckpointTokensReport_LegacyCheckpoint(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{
		SessionID: "legacy-session", Agent: agent.AgentTypeClaudeCode, Model: checkpointTokensFixtureModel,
		TokenUsage: &types.TokenUsage{InputTokens: 1000, CacheCreationTokens: 5000, CacheReadTokens: 100_000, OutputTokens: 2000, APICallCount: 20},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, 0),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, checkpointTokensClaudeFixture)},
	}, nil)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		checkpointTokensLegacyScope,
		"Where it went",
		"Subagent: Explore",
		"Bash · during systematic-debugging",
		"Usage",
		"Cache write",
		"5k",
		"Total",
		"108k",
		"cache-write TTL not recorded; not priced",
	)
	if strings.Contains(out, "Cache write, 1-hour") || strings.Contains(out, "of which 1-hour") {
		t.Errorf("legacy totals carry no TTL split, got:\n%s", out)
	}
	if report.Source != checkpointTokensSourceCommitted {
		t.Errorf("source = %q, want committed_checkpoint for legacy totals", report.Source)
	}
	if report.Tokens == nil || report.Tokens.Total != 108_000 || report.Tokens.APICalls != 20 {
		t.Errorf("tokens = %+v, want the committed totals", report.Tokens)
	}
	if report.Legacy == nil || !report.Legacy.Cumulative || report.Legacy.ThinkingRecorded || report.Legacy.CacheTTLRecorded {
		t.Errorf("legacy = %+v", report.Legacy)
	}
	if !report.view.Report.Cost.CacheWriteUnpriced || report.view.Report.Cost.Units == 0 {
		t.Errorf("cost = %+v, want priced classes with cache write unpriced", report.view.Report.Cost)
	}
	if len(report.Contributors) == 0 {
		t.Error("expected a breakdown over the whole stored transcript")
	}
}

func TestBuildCheckpointTokensReport_SlicesAtTokenTranscriptStart(t *testing.T) {
	t.Parallel()

	// Line 7 is the Agent call; only it and the Skill call follow.
	meta := checkpoint.Metadata{
		SessionID: "sliced", Agent: agent.AgentTypeClaudeCode, Model: checkpointTokensFixtureModel,
		TokenTranscriptStart: 7, CheckpointTranscriptStart: 0,
		TokenUsage: &types.TokenUsage{InputTokens: 30, CacheCreationTokens: 80, CacheReadTokens: 1300, OutputTokens: 40, APICallCount: 1},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, checkpointTokensClaudeFixture)},
	}, nil)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		"Skill: artifact-design (loaded)",
		"(usage not recorded)",
		"Subagent: Explore",
		"Context replay (cache read)",
		"1 API call",
		"1 call with no usage recorded",
		"1 subagent call has no committed task record; that usage is not included",
	)
	if strings.Contains(out, "systematic-debugging") || strings.Contains(out, checkpointTokensLegacyScope) {
		t.Errorf("calls before the token window must not appear, got:\n%s", out)
	}
	if report.Source != checkpointTokensSourceTranscript || report.Tokens == nil || report.Tokens.Total != 1450 {
		t.Errorf("source=%q tokens=%+v", report.Source, report.Tokens)
	}
	if report.view.UnknownUsageCalls != 1 || report.view.Report.Calls != 1 {
		t.Errorf("unknown=%d calls=%d", report.view.UnknownUsageCalls, report.view.Report.Calls)
	}
}

func TestBuildCheckpointTokensReport_TaskRecordsAcrossSessions(t *testing.T) {
	t.Parallel()

	meta := func(sessionID string) checkpoint.Metadata {
		return checkpoint.Metadata{SessionID: sessionID, Agent: agent.AgentTypeClaudeCode, Model: checkpointTokensFixtureModel}
	}
	started := time.Date(2026, 8, 27, 10, 0, 7, 0, time.UTC)
	records := []checkpoint.StoredTaskRecord{
		// Out of chronological order on purpose: the orphan starts later.
		{ToolUseID: "toolu_orphan", SubagentType: "Reviewer", TokenUsage: &types.TokenUsage{OutputTokens: 900, APICallCount: 2, Model: "claude-haiku-4-5"}, StartedAt: started.Add(time.Minute)},
		{ToolUseID: "toolu_t1", SubagentType: "Explore", TokenUsage: &types.TokenUsage{OutputTokens: 400, APICallCount: 1, Model: "claude-haiku-4-5"}, StartedAt: started},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary: tokenTestSummary(2, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{
			0: tokenTestSession(meta("one"), checkpointTokensClaudeFixture),
			1: tokenTestSession(meta("two"), checkpointTokensClaudeFixture),
		},
	}, records)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		"Sessions:   2",
		"Subagent: Explore",
		"Subagent: Reviewer",
		"Breakdown merged across 2 sessions; sub-row call and token counts are lower bounds.",
	)
	if report.Tokens == nil || report.Tokens.SubagentTotal != 1300 {
		t.Errorf("subagent_total = %+v, want 1300 (both records counted once)", report.Tokens)
	}
	var explore, reviewer *tokenreport.Contributor
	for i := range report.Contributors {
		switch report.Contributors[i].Label {
		case "Explore":
			explore = &report.Contributors[i]
		case "Reviewer":
			reviewer = &report.Contributors[i]
		}
	}
	if explore == nil || explore.Usage.OutputTokens != 40+40+400 || explore.Model != "" {
		t.Errorf("Explore row = %+v, want both sessions' output plus one record and a mixed model", explore)
	}
	if reviewer == nil || reviewer.Source != tokenreport.SourceTaskRecord || reviewer.Model != "claude-haiku-4-5" {
		t.Errorf("Reviewer row = %+v, want an orphan task-record row", reviewer)
	}
}

func TestBuildCheckpointTokensReport_CursorShapedSessionPrintsTotalsOnly(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{
		SessionID: "cursor", Agent: agent.AgentTypeCursor,
		TokenUsage:     &types.TokenUsage{InputTokens: 1000, OutputTokens: 500, APICallCount: 3},
		SessionMetrics: &checkpoint.SessionMetrics{DurationMs: 3_600_000},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, []byte("not a transcript\n"))},
	}, nil)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		"Duration:   1h 00m · 3 API calls · 1.5k tokens",
		"Usage",
		"of which thinking",
		"not recorded",
		"Cursor records session totals only; the per-call breakdown is not verified for this agent.",
	)
	if strings.Contains(out, "Where it went") || strings.Contains(out, "Effort:") {
		t.Errorf("a totals-only agent has no breakdown and no effort, got:\n%s", out)
	}
	if !strings.Contains(out, tokenTableUnpriced) {
		t.Errorf("an unknown model prints — in the share column, got:\n%s", out)
	}
}

func TestBuildCheckpointTokensReport_CopilotShapedSessionIsNotVerified(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{
		SessionID: "copilot", Agent: agent.AgentTypeCopilotCLI, Model: "gpt-5.4",
		TokenUsage: &types.TokenUsage{InputTokens: 10_000, CacheReadTokens: 50_000, OutputTokens: 2_000, APICallCount: 12},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, []byte("{}\n"))},
	}, nil)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		"Copilot CLI records session totals only; the per-call breakdown is not verified for this agent.",
		"Cost shares use OpenAI list-price ratios (input 1×, no cache-write charge, cache read 0.1×, output 6×), not your plan's rates.",
	)
	if !tokenreport.ProfileFor(agent.AgentTypeCopilotCLI).TotalsOnly {
		t.Fatal("test premise: Copilot CLI is totals-only")
	}
	if strings.Contains(out, "Where it went") {
		t.Errorf("totals only, got:\n%s", out)
	}
}

func TestBuildCheckpointTokensReport_UnknownAgentFallsBackToMetadata(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{
		SessionID: "mystery", Agent: "Mystery Agent", Model: "mystery-model",
		TokenUsage: &types.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1},
	}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, checkpointTokensClaudeFixture)},
	}, nil)

	out := renderTokensReport(&report)
	assertContainsAll(t, out,
		`session 1: agent "Mystery Agent" is not known to this CLI; totals from committed metadata`,
		"no verified capability profile for Mystery Agent; totals shown, breakdown not verified.",
		"no verified price ratios for `mystery-model`",
		"Total",
		"150",
	)
	if report.Source != checkpointTokensSourceCommitted {
		t.Errorf("source = %q", report.Source)
	}
}

func TestBuildCheckpointTokensReport_UnreadableTranscriptIsANote(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{SessionID: "no-transcript", Agent: agent.AgentTypeClaudeCode, Model: checkpointTokensFixtureModel,
		TokenUsage: &types.TokenUsage{InputTokens: 10, OutputTokens: 5, APICallCount: 1}}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(1, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, nil)},
	}, nil)

	if !strings.Contains(strings.Join(report.Limitations, "\n"), "session 1: stored transcript unavailable; totals from committed metadata") {
		t.Errorf("limitations = %+v", report.Limitations)
	}
	if report.Tokens == nil || report.Tokens.Total != 15 {
		t.Errorf("tokens = %+v", report.Tokens)
	}
}

func TestBuildCheckpointTokensReport_MetadataWarningsAreCounted(t *testing.T) {
	t.Parallel()

	meta := checkpoint.Metadata{SessionID: "readable", Agent: agent.AgentTypeClaudeCode, TokenUsage: &types.TokenUsage{InputTokens: 100, APICallCount: 1}}
	report := buildStubTokensReport(t, &stubCommittedReader{
		summary:  tokenTestSummary(2, checkpoint.TokenUsageVersionDelta),
		contents: map[int]*checkpoint.SessionContent{0: tokenTestSession(meta, nil)},
		err:      errors.New("boom"),
	}, nil)

	if !strings.Contains(strings.Join(report.Limitations, "\n"), "1 session metadata file could not be read; those sessions are not in the totals.") {
		t.Errorf("limitations = %+v", report.Limitations)
	}
	if report.SessionCount != 2 || report.SessionID != "" {
		t.Errorf("session_count=%d session_id=%q, want the multi-session shape", report.SessionCount, report.SessionID)
	}
}

// cancelingTokenReader cancels its context on the first metadata read.
type cancelingTokenReader struct {
	stubCommittedReader

	cancel context.CancelFunc
	calls  int
}

func (r *cancelingTokenReader) ReadSessionMetadata(_ context.Context, _ id.CheckpointID, _ int) (*checkpoint.Metadata, error) {
	r.calls++
	if r.calls == 1 {
		r.cancel()
	}
	return &checkpoint.Metadata{SessionID: "read"}, nil
}

func TestReadCheckpointTokenSessionsStopsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingTokenReader{cancel: cancel}
	sessions, warnings, err := readCheckpointTokenSessions(ctx, reader, id.MustCheckpointID("abc123abc123"), 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got sessions=%+v warnings=%d err=%v", sessions, warnings, err)
	}
	if reader.calls != 1 || sessions != nil || warnings != 0 {
		t.Fatalf("calls=%d sessions=%+v warnings=%d, want one read and no partial results", reader.calls, sessions, warnings)
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, _, err := readCheckpointTokenSessions(canceled, nil, id.MustCheckpointID("abc123abc123"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled before any read, got %v", err)
	}
}

func TestApplySkillEventAnchorsLabelsUnnamedSkillLoads(t *testing.T) {
	t.Parallel()

	attribution := &types.Attribution{Calls: []types.CallUsage{
		{Emitted: []types.ToolUseRef{{ID: "toolu_x", Tool: "Skill"}, {ID: "toolu_named", Tool: "Skill", SkillName: "keep", Detail: "keep"}}},
		{Consumed: []types.ToolResultRef{{ToolUse: types.ToolUseRef{ID: "toolu_x", Tool: "Skill"}}}},
	}}
	applySkillEventAnchors(attribution, []types.SkillEvent{
		{Skill: types.SkillEventSkill{Name: testSkillSystematicDebugging}, TranscriptAnchor: &types.SkillEventTranscriptAnchor{ToolUseID: "toolu_x"}},
		{Skill: types.SkillEventSkill{Name: "other"}, TranscriptAnchor: &types.SkillEventTranscriptAnchor{ToolUseID: "toolu_named"}},
	})
	got := attribution.Calls[0].Emitted[0]
	if got.SkillName != testSkillSystematicDebugging || got.Detail != testSkillSystematicDebugging {
		t.Errorf("emitted ref = %+v, want the anchor's skill name", got)
	}
	if attribution.Calls[0].Emitted[1].SkillName != "keep" {
		t.Errorf("a ref the attributor named must keep its name, got %+v", attribution.Calls[0].Emitted[1])
	}
	if attribution.Calls[1].Consumed[0].ToolUse.SkillName != testSkillSystematicDebugging {
		t.Errorf("consumed ref = %+v", attribution.Calls[1].Consumed[0].ToolUse)
	}
}

func TestUnmatchedSubagentNoteWordsPerAgent(t *testing.T) {
	t.Parallel()

	if got := unmatchedSubagentNote(agent.AgentTypeCodex, 2); got != "Codex subagent tokens are not included (their rollouts are separate sessions)." {
		t.Errorf("codex = %q", got)
	}
	if got := unmatchedSubagentNote(agent.AgentTypeOpenCode, 1); !strings.Contains(got, "OpenCode subagent tokens are not included") {
		t.Errorf("opencode = %q", got)
	}
	if got := unmatchedSubagentNote(agent.AgentTypeClaudeCode, 2); got != "2 subagent calls have no committed task record; that usage is not included (this backend may not store task records)." {
		t.Errorf("claude = %q", got)
	}
}

func TestModalKeyCountPrefersHighestThenLexical(t *testing.T) {
	t.Parallel()

	k, n := modalKeyCount(map[string]int{"gpt-5.4": 1, "claude-fable-5": 3, "claude-haiku-4-5": 3})
	if k != "claude-fable-5" || n != 3 {
		t.Errorf("got %q/%d, want claude-fable-5/3 (ties resolve lexically)", k, n)
	}
	if k, n := modalKeyCount(nil); k != "" || n != 0 {
		t.Errorf("empty map → %q/%d", k, n)
	}
}
