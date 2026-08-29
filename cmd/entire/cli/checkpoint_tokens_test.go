package cli

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
	"github.com/entireio/cli/redact"
)

// checkpointTokensClaudeFixture is a copy of the Claude Code attribution
// fixture (agent/claudecode/testdata/attribution_session.jsonl): four API
// calls on claude-fable-5 at effort high — two Bash calls during the
// systematic-debugging skill, a text-only call, an Agent (Explore, haiku)
// call and a Skill (artifact-design) call with no usage — plus one malformed
// row. Timestamps run 10:00:00 → 10:00:10.
//
//go:embed testdata/checkpoint_tokens_claude_session.jsonl
var checkpointTokensClaudeFixture []byte

const (
	checkpointTokensFixtureModel  = "claude-fable-5"
	checkpointTokensFixtureBranch = "fix/quote-claude-project-dir-hook"
	checkpointTokensFixtureEffort = "high"
)

// checkpointTokensFixtureTaskUsage is the committed task record for the
// fixture's Explore subagent (toolu_t1), on the parent's model so the
// subagent_model recommendation fires.
func checkpointTokensFixtureTaskUsage() *agent.TokenUsage {
	return &agent.TokenUsage{InputTokens: 5000, CacheReadTokens: 20000, OutputTokens: 2000, APICallCount: 6, Model: checkpointTokensFixtureModel}
}

// writeCheckpointTokensFixture commits the Claude fixture as a version-2
// checkpoint with one task record and returns its ID.
func writeCheckpointTokensFixture(ctx context.Context, t *testing.T, store *checkpoint.GitStore, cpID id.CheckpointID) {
	t.Helper()
	started := time.Date(2026, 8, 27, 10, 0, 7, 0, time.UTC)
	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    "checkpoint-tokens-fixture",
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       checkpointTokensFixtureBranch,
		Agent:        agent.AgentTypeClaudeCode,
		Model:        checkpointTokensFixtureModel,
		Transcript:   redact.AlreadyRedacted(checkpointTokensClaudeFixture),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage: &agent.TokenUsage{
			InputTokens: 60, CacheCreationTokens: 380, CacheReadTokens: 3500, OutputTokens: 115, APICallCount: 3,
			ThinkingTokens: 17, CacheCreation1hTokens: 50,
			SubagentTokens: checkpointTokensFixtureTaskUsage(),
		},
		Tasks: []checkpoint.TaskPayload{{
			ToolUseID:                   "toolu_t1",
			AgentID:                     "agent-explore-1",
			SubagentType:                "Explore",
			TaskDescription:             "Look around",
			TokenUsage:                  checkpointTokensFixtureTaskUsage(),
			StartedAt:                   started,
			CompletedAt:                 started.Add(time.Second),
			TranscriptUnavailableReason: "transcript empty",
		}},
	}); err != nil {
		t.Fatalf("Write(fixture checkpoint) error = %v", err)
	}
}

// writeCommittedTokenCheckpoint commits a metadata-only checkpoint: a
// one-line transcript with no API calls, so the report falls back to the
// committed token_usage.
func writeCommittedTokenCheckpoint(ctx context.Context, t *testing.T, store *checkpoint.GitStore, cpID id.CheckpointID, sessionID string, usage *agent.TokenUsage) {
	t.Helper()
	if err := store.Write(ctx, checkpoint.Session{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-compare",
		Agent:        agent.AgentTypeClaudeCode,
		Model:        checkpointTokensFixtureModel,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"compare"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage:   usage,
	}); err != nil {
		t.Fatalf("Write(%s) error = %v", cpID, err)
	}
}

// runCheckpointTokensCmd runs `checkpoint tokens` with args and returns stdout.
func runCheckpointTokensCmd(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs(append([]string{"tokens"}, args...))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("checkpoint tokens %v: %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

// assertContainsAll asserts each check is a substring of out, before or
// after unwrapping the Notes section's hanging-indent continuation lines.
func assertContainsAll(t *testing.T, out string, checks ...string) {
	t.Helper()
	unwrapped := unwrapNotes(out)
	for _, check := range checks {
		if !strings.Contains(out, check) && !strings.Contains(unwrapped, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

// unwrapNotes joins wrapped continuation lines (four-space indent, not a
// table sub-row) back onto the line before them.
func unwrapNotes(out string) string {
	lines := strings.Split(out, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "- ") {
			b.WriteString(" " + strings.TrimSpace(line))
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

// assertLineContainsAll asserts the first line of out containing anchor also
// contains every check.
func assertLineContainsAll(t *testing.T, out, anchor string, checks ...string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, anchor) {
			continue
		}
		for _, check := range checks {
			if !strings.Contains(line, check) {
				t.Errorf("expected %q on the %q line, got %q", check, anchor, line)
			}
		}
		return
	}
	t.Errorf("no line contains %q in:\n%s", anchor, out)
}

func TestCheckpointTokensCmd_TextOutputIsBreakdownFirst(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("beefbeefcafe")
	writeCheckpointTokensFixture(ctx, t, store, cpID)

	out := runCheckpointTokensCmd(ctx, t, "beefbeef")

	assertContainsAll(t, out,
		"Checkpoint tokens",
		"Checkpoint: beefbeefcafe      Agent: Claude Code      Model: claude-fable-5",
		"Session:    checkpoint-tokens-fixture",
		"Duration:   10s · 3 API calls · 31.1k tokens      Effort: high (4 calls)",
		"Branch:     "+checkpointTokensFixtureBranch,
		// Where it went: the subagent row (its record absorbed) leads, its
		// detail prints a call count, the skill annotation rides on Bash.
		"Where it went",
		"est. cost share",
		"Subagent: Explore",
		"Explore (haiku)",
		"Context replay (cache read)",
		"Prompt & system context",
		"Bash · during systematic-debugging",
		"Assistant text",
		"(1 smaller item omitted)",
		// Usage: four classes, the 1h split, thinking as a share of output,
		// Total = Σ classes and the subagent part.
		"Usage",
		"Input (fresh)",
		"Cache write",
		"of which 1-hour",
		"Cache read",
		"Output",
		"of which thinking",
		"1% of output",
		"Total",
		"31.1k",
		"of which subagents",
		"27k",
		"(5×)",
		"(0.1×)",
		// Recommendations: subagent_model fires on the parent-model record.
		"Recommendations",
		"Explore subagents ran on `claude-fable-5` (27k tokens, 93% of cost)",
		// Notes.
		"Notes",
		"Cost shares use Anthropic list-price ratios (input 1×, 5m write 1.25×, 1h write 2×, cache read 0.1×, output 5×), not your plan's rates.",
		"1 call with no usage recorded",
	)
	// Per-call usage blocks record their TTL split, so the two calls' 5m
	// writes are priced: no "TTL not recorded" note on a v2 checkpoint.
	for _, absent := range []string{"Token scope: legacy", "Likely contributors", "Limitations", "Skill: artifact-design", "TTL not recorded", "(from stored transcript)"} {
		if strings.Contains(out, absent) {
			t.Errorf("did not expect %q in output, got:\n%s", absent, out)
		}
	}
	whereIdx, usageIdx, recIdx, notesIdx := strings.Index(out, "Where it went"), strings.Index(out, "\nUsage"), strings.Index(out, "Recommendations"), strings.Index(out, "Notes")
	if whereIdx >= usageIdx || usageIdx >= recIdx || recIdx >= notesIdx {
		t.Fatalf("sections out of order (where=%d usage=%d rec=%d notes=%d):\n%s", whereIdx, usageIdx, recIdx, notesIdx, out)
	}
	assertLineContainsAll(t, out, "Explore (haiku)", "1 call ", " 40 ")
	assertRecommendationFiguresVisible(t, out)
}

func TestCheckpointTokensCmd_JSONAndAgentBriefAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	cmd := newCheckpointGroupCmd()
	cmd.SetArgs([]string{"tokens", "abc123", "--json", "--agent-brief"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutually exclusive error for --json with --agent-brief, got: %v", err)
	}
}

func TestCheckpointTokensCmd_JSONOutputShape(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("cafe00001234")
	writeCheckpointTokensFixture(ctx, t, store, cpID)

	out := runCheckpointTokensCmd(ctx, t, "cafe0000", "--json")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"checkpoint_id", "source", "duration_seconds", "effort", "tokens", "cost", "contributors", "recommendations", "limitations"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q in:\n%s", key, out)
		}
	}
	for _, key := range []string{"legacy", "agent_reported_cost", "comparison"} {
		if _, ok := raw[key]; ok {
			t.Errorf("did not expect JSON key %q in:\n%s", key, out)
		}
	}

	var result checkpointTokensReport
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.CheckpointID != "cafe00001234" || result.SessionID != "checkpoint-tokens-fixture" || result.Agent != testAgentClaude || result.Model != checkpointTokensFixtureModel {
		t.Errorf("identity = %q %q %q %q", result.CheckpointID, result.SessionID, result.Agent, result.Model)
	}
	if result.Source != checkpointTokensSourceTranscript {
		t.Errorf("source = %q, want transcript", result.Source)
	}
	if result.DurationSeconds != 10 {
		t.Errorf("duration_seconds = %d, want 10", result.DurationSeconds)
	}
	if result.Effort == nil || result.Effort.Value != checkpointTokensFixtureEffort || result.Effort.Calls != 4 {
		t.Errorf("effort = %+v, want high/4", result.Effort)
	}
	want := tokenUsageJSON{Total: 31055, Input: 5060, CacheRead: 23500, CacheWrite: 380, Output: 2115, APICalls: 9, SubagentTotal: 27000, ThinkingTokens: 17, CacheCreation1hTokens: 50}
	if result.Tokens == nil || *result.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", result.Tokens, want)
	}
	if result.Cost == nil || result.Cost.Provider != tokenreport.ProviderAnthropic || result.Cost.Family != tokenreport.FamilyAnthropic || result.Cost.Weights == nil || result.Cost.Weights.Output != 5 {
		t.Errorf("cost = %+v", result.Cost)
	}
	// Units: m1 622.5 + m2 415 + m3 460 (5m writes priced at 1.25×) + record
	// 17000 = 18497.5; output units 10575.
	if result.Cost != nil && (math.Abs(result.Cost.Shares.Output-0.5717) > 0.001 || result.Cost.Shares.CacheWriteUnpriced) {
		t.Errorf("cost.shares = %+v, want output ≈0.572 with cache writes priced", result.Cost.Shares)
	}
	if _, ok := raw["context"]; ok {
		t.Errorf("no context key without SessionMetrics:\n%s", out)
	}
	if len(result.Contributors) != 7 {
		t.Fatalf("contributors = %d rows, want 7: %+v", len(result.Contributors), result.Contributors)
	}
	top := result.Contributors[0]
	if top.Kind != tokenreport.KindSubagent || top.Label != "Explore" || top.Model != checkpointTokensFixtureModel || top.Source != tokenreport.SourceTranscript || len(top.Details) != 1 {
		t.Errorf("contributors[0] = %+v", top)
	}
	skillRow := result.Contributors[len(result.Contributors)-1]
	if skillRow.Kind != tokenreport.KindSkill || skillRow.Label != "artifact-design" || tokenVolume(&skillRow.Usage) != 0 {
		t.Errorf("last contributor = %+v, want the zero-token skill row", skillRow)
	}
	if len(result.Recommendations) != 1 {
		t.Fatalf("recommendations = %+v, want one", result.Recommendations)
	}
	rec := result.Recommendations[0]
	if rec.Cause != tokenreport.CauseSubagentModel || rec.ID != string(rec.Cause) || rec.Message != rec.Text || rec.Kind != tokenreport.RecommendationKindSession || len(rec.Cited) != 1 {
		t.Errorf("recommendation = %+v", rec)
	}
	if !strings.Contains(strings.Join(result.Limitations, "\n"), "Anthropic list-price ratios") {
		t.Errorf("limitations = %+v, want the pricing note", result.Limitations)
	}
}

func TestCheckpointTokensCmd_AgentBrief(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("b1efbeefcafe")
	writeCheckpointTokensFixture(ctx, t, store, cpID)

	out := runCheckpointTokensCmd(ctx, t, "b1efbeef", "--agent-brief")

	assertContainsAll(t, out,
		"Checkpoint token brief",
		"Checkpoint: b1efbeefcafe",
		"Token usage: 31.1k total; 3 API calls; 10s; cache read 13% of cost.",
		"Next best action:",
		"Explore subagents ran on `claude-fable-5` (27k tokens, 93% of cost); delegated work like this often runs well on a smaller model.",
		"Signals:",
		"- subagent_model",
	)
	for _, absent := range []string{"Pattern:", "Where it went", "Recommendations", "Notes"} {
		if strings.Contains(out, absent) {
			t.Errorf("did not expect %q in the brief, got:\n%s", absent, out)
		}
	}
}

func TestCheckpointTokensCmd_AgentBriefWithoutUsage(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("deadcafebeef")
	writeCommittedTokenCheckpoint(ctx, t, store, cpID, "checkpoint-token-missing-brief", nil)

	out := runCheckpointTokensCmd(ctx, t, "deadcafe", "--agent-brief")

	assertContainsAll(t, out, "Token usage: unavailable.", "Next best action:", "Token usage is not recorded here", "- token usage not recorded")
}

func TestCheckpointTokensCmd_MetadataFallbackWhenTranscriptHasNoCalls(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("feedfeedcafe")
	writeCommittedTokenCheckpoint(ctx, t, store, cpID, "checkpoint-token-fallback", &agent.TokenUsage{
		InputTokens: 94, CacheCreationTokens: 122171, CacheReadTokens: 6052424, OutputTokens: 38956, APICallCount: 70,
	})

	out := runCheckpointTokensCmd(ctx, t, "feedfeed")
	assertContainsAll(t, out,
		"Duration:   not recorded · 70 API calls · 6.2M tokens",
		"Usage",
		"Cache read",
		"6.1M",
		"Total",
		"6.2M",
		"cache-write TTL not recorded; not priced",
	)
	if strings.Contains(out, "Where it went") {
		t.Errorf("expected no breakdown without attributed calls, got:\n%s", out)
	}

	var result checkpointTokensReport
	if err := json.Unmarshal([]byte(runCheckpointTokensCmd(ctx, t, "feedfeed", "--json")), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Source != checkpointTokensSourceCommitted {
		t.Errorf("source = %q, want committed_checkpoint", result.Source)
	}
	if len(result.Contributors) != 0 || result.Tokens == nil || result.Tokens.Total != 6213645 {
		t.Errorf("contributors = %d, tokens = %+v", len(result.Contributors), result.Tokens)
	}
}

func TestCheckpointTokensCmd_CompareIncludesCostShares(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("aaa111bbb222")
	currentID := id.MustCheckpointID("bbb222ccc333")
	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-baseline", &agent.TokenUsage{
		InputTokens: 200_000, CacheCreationTokens: 50_000, CacheCreation1hTokens: 50_000, CacheReadTokens: 750_000, OutputTokens: 10_000, APICallCount: 10,
	})
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-current", &agent.TokenUsage{
		InputTokens: 150_000, CacheCreationTokens: 25_000, CacheCreation1hTokens: 25_000, CacheReadTokens: 300_000, OutputTokens: 25_000, APICallCount: 4,
	})

	out := runCheckpointTokensCmd(ctx, t, "bbb222", "--compare", "aaa111")
	assertContainsAll(t, out,
		"Comparison",
		"Baseline: aaa111bbb222",
		"Total tokens: down 50.5% (1M → 500k)",
		"Input: down 25% (200k → 150k)",
		"Cache/context replay: down 60% (750k → 300k)",
		"Cache write: down 50% (50k → 25k)",
		"Output: up 150% (10k → 25k)",
		"API calls: down 60% (10 → 4)",
		"Cost share, output:",
		"Qualification",
		"Observed total token use decreased for this checkpoint comparison.",
		"Cost mix:",
	)

	var result checkpointTokensReport
	if err := json.Unmarshal([]byte(runCheckpointTokensCmd(ctx, t, "bbb222", "--compare", "aaa111", "--json")), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := result.Comparison
	if c == nil || c.Status != checkpointComparisonStatusObservedReduction || c.CostShare == nil {
		t.Fatalf("comparison = %+v", c)
	}
	// Baseline units: 200k + 50k×2 + 750k×0.1 + 10k×5 = 425k → output 11.8%.
	// Current units: 150k + 25k×2 + 300k×0.1 + 25k×5 = 355k → output 35.2%.
	if c.CostShare.Output.Direction != checkpointDeltaDirectionUp || math.Abs(c.CostShare.Output.Baseline-0.1176) > 0.001 || math.Abs(c.CostShare.Output.Current-0.3521) > 0.001 {
		t.Errorf("output cost-share delta = %+v", c.CostShare.Output)
	}
	if c.CostShare.Output.ChangePercent == nil || c.CostShare.CacheRead.Direction != checkpointDeltaDirectionDown {
		t.Errorf("cost_share = %+v %+v", c.CostShare.Output, c.CostShare.CacheRead)
	}
	if !strings.Contains(c.Qualification, "output 12% → 35% (up 23 points)") {
		t.Errorf("qualification = %q", c.Qualification)
	}
}

func TestCheckpointTokensCmd_CompareUnavailableWithoutTokens(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	baselineID := id.MustCheckpointID("333ccc444ddd")
	currentID := id.MustCheckpointID("444ddd555eee")
	writeCommittedTokenCheckpoint(ctx, t, store, baselineID, "checkpoint-token-missing-baseline", nil)
	writeCommittedTokenCheckpoint(ctx, t, store, currentID, "checkpoint-token-current-with-data", &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1})

	out := runCheckpointTokensCmd(ctx, t, "444ddd", "--compare", "333ccc")
	assertContainsAll(t, out, "Comparison", "Baseline: 333ccc444ddd", "Comparison unavailable because token usage is missing for one checkpoint.")
	if strings.Contains(out, "Total tokens:") {
		t.Fatalf("expected unavailable comparison to omit metric deltas, got:\n%s", out)
	}
}

func TestCheckpointTokensCmd_RejectsSelfComparison(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID := id.MustCheckpointID("abc222abc222")
	writeCommittedTokenCheckpoint(ctx, t, store, cpID, "checkpoint-token-self-compare", &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1})

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"tokens", "abc222", "--compare", "abc222abc222"})
	err := cmd.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "cannot compare checkpoint abc222abc222 to itself") {
		t.Fatalf("expected self-comparison error, got: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no report output for self-comparison, got:\n%s", stdout.String())
	}
}

func TestCheckpointTokensCmd_UnknownCheckpointPrintsNoReport(t *testing.T) {
	runExplainAutoTestRepo(t)
	cmd := newCheckpointGroupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"tokens", "0000deadbeef"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
	if strings.Contains(stdout.String(), "Checkpoint tokens") {
		t.Fatalf("expected no report for an unknown checkpoint, got:\n%s", stdout.String())
	}
}

func TestBuildCheckpointShareDelta(t *testing.T) {
	t.Parallel()

	up := buildCheckpointShareDelta(0.36, 0.48)
	if up.Direction != checkpointDeltaDirectionUp || math.Abs(up.Change-0.12) > 1e-9 || up.ChangePercent == nil || math.Abs(*up.ChangePercent-33.333) > 0.01 {
		t.Errorf("up = %+v", up)
	}
	flat := buildCheckpointShareDelta(0.23, 0.234)
	if flat.Direction != checkpointDeltaDirectionUnchanged {
		t.Errorf("a sub-point drift should read unchanged, got %+v", flat)
	}
	fromZero := buildCheckpointShareDelta(0, 0.1)
	if fromZero.ChangePercent != nil || fromZero.Direction != checkpointDeltaDirectionUp {
		t.Errorf("fromZero = %+v", fromZero)
	}
}

func TestCheckpointCostMixShift(t *testing.T) {
	t.Parallel()

	d := buildCheckpointCostShareDelta(
		&tokenreport.CostShares{Input: 0.10, CacheWrite: 0.41, CacheRead: 0.13, Output: 0.36},
		&tokenreport.CostShares{Input: 0.08, CacheWrite: 0.30, CacheRead: 0.14, Output: 0.48},
	)
	got := checkpointCostMixShift(d)
	want := "Cost mix: cache write 41% → 30% (down 11 points); output 36% → 48% (up 12 points)."
	if got != want {
		t.Errorf("shift = %q, want %q", got, want)
	}
	if s := checkpointCostMixShift(buildCheckpointCostShareDelta(&tokenreport.CostShares{Output: 0.5}, &tokenreport.CostShares{Output: 0.52})); s != "" {
		t.Errorf("small moves should not be named, got %q", s)
	}
}

func TestBuildCheckpointMetricDeltaClampsChangeOverflow(t *testing.T) {
	t.Parallel()

	up := buildCheckpointMetricDelta(math.MinInt, math.MaxInt)
	if up.Change != math.MaxInt || up.Direction != checkpointDeltaDirectionUp {
		t.Fatalf("upward overflow = %+v", up)
	}
	down := buildCheckpointMetricDelta(math.MaxInt, math.MinInt)
	if down.Change != math.MinInt || down.Direction != checkpointDeltaDirectionDown {
		t.Fatalf("downward overflow = %+v", down)
	}
}

func TestSaturatingIntSubHandlesMinIntSubtrahend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    int
		want int
	}{
		{name: "clamps non-negative minuend", a: 0, want: math.MaxInt},
		{name: "keeps max exact result", a: -1, want: math.MaxInt},
		{name: "keeps representable result", a: -2, want: math.MaxInt - 1},
		{name: "keeps zero exact result", a: math.MinInt, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := saturatingIntSub(tt.a, math.MinInt); got != tt.want {
				t.Fatalf("saturatingIntSub(%d, minInt) = %d, want %d", tt.a, got, tt.want)
			}
		})
	}
}
