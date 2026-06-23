package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/redact"
)

func TestAddTokensProfileTokenSignalsSubagentHeavyAvoidsOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	signals := map[string]*tokensProfileSignal{}
	addTokensProfileTokenSignals(signals, id.MustCheckpointID("999aaa000001"), &sessionTokensUsage{
		Total:         maxInt,
		SubagentTotal: maxInt,
	}, 1)

	if signals["subagent-heavy"] == nil {
		t.Fatalf("expected subagent-heavy signal, got %+v", signals)
	}
}

func TestAddTokensProfileTokenSignalsCacheReplayUsesTopLevelTokenTotal(t *testing.T) {
	t.Parallel()

	signals := map[string]*tokensProfileSignal{}
	addTokensProfileTokenSignals(signals, id.MustCheckpointID("999aaa000002"), &sessionTokensUsage{
		Total:         10000,
		Input:         100,
		CacheRead:     800,
		CacheWrite:    50,
		Output:        50,
		APICalls:      20,
		SubagentTotal: 9000,
	}, 1)

	if signals["context-replay-hotspot"] == nil {
		t.Fatalf("expected context-replay-hotspot signal, got %+v", signals)
	}
}

func TestTokensProfileCmd_TextOutputAggregatesCommittedCheckpoints(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000001", "profile-cache-hotspot", &agent.TokenUsage{
		InputTokens:         100,
		CacheCreationTokens: 100,
		CacheReadTokens:     800,
		APICallCount:        5,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000002", "profile-api-heavy", &agent.TokenUsage{
		InputTokens:  400,
		OutputTokens: 100,
		APICallCount: 25,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000003", "profile-subagent-heavy", &agent.TokenUsage{
		InputTokens:  500,
		OutputTokens: 500,
		APICallCount: 3,
		SubagentTokens: &agent.TokenUsage{
			InputTokens: 1_000,
		},
	})
	writeProfileTokenCheckpoint(ctx, t, store, "100aaa000004", "profile-missing", nil)

	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"profile"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Token profile",
		"Checkpoints analyzed: 4",
		"With token data:      3",
		"Missing token data:   1",
		"Checkpoint-observed token usage",
		"Total:  3.5k tokens",
		"Cache read: 800",
		"API calls: 33",
		"Repeated signals",
		"Cache/context replay hotspot: 1 checkpoint",
		"API call amplification: 1 checkpoint",
		"Subagent-heavy sessions: 1 checkpoint",
		"Missing token data: 1 checkpoint",
		"Recommendations",
		"Use `entire search` for prior decisions/checkpoints before broad re-investigation.",
		"Token totals are summed from analyzed checkpoints and may include overlapping checkpoint history",
		"Tool-level search/read spend is not captured yet",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}

	tokenUsageIndex := strings.Index(out, "Checkpoint-observed token usage")
	recommendationsIndex := strings.Index(out, "Recommendations")
	if tokenUsageIndex == -1 || recommendationsIndex == -1 {
		t.Fatalf("expected token usage and recommendations sections, got:\n%s", out)
	}
	if tokenUsageIndex > recommendationsIndex {
		t.Fatalf("expected token usage before recommendations, got:\n%s", out)
	}
}

func TestTokensProfileCmd_JSONOutput(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	writeProfileTokenCheckpoint(ctx, t, store, "200bbb000001", "profile-json-cache", &agent.TokenUsage{
		InputTokens:     100,
		CacheReadTokens: 900,
		APICallCount:    2,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "200bbb000002", "profile-json-api", &agent.TokenUsage{
		InputTokens:  200,
		OutputTokens: 100,
		APICallCount: 22,
	})

	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"profile", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result tokensProfileReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("expected valid JSON object, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if raw["usage_scope"] != "checkpoint_observed" {
		t.Fatalf("usage_scope = %v, want checkpoint_observed", raw["usage_scope"])
	}
	if result.CheckpointsAnalyzed != 2 {
		t.Fatalf("checkpoints_analyzed = %d, want 2", result.CheckpointsAnalyzed)
	}
	if result.CheckpointsWithTokenData != 2 {
		t.Fatalf("checkpoints_with_token_data = %d, want 2", result.CheckpointsWithTokenData)
	}
	if result.Tokens == nil || result.Tokens.Total != 1300 {
		t.Fatalf("unexpected token total: %+v", result.Tokens)
	}
	if got := signalCount(result.Signals, "context-replay-hotspot"); got != 1 {
		t.Fatalf("context-replay-hotspot signal count = %d, want 1", got)
	}
	if got := signalCount(result.Signals, "api-call-amplification"); got != 1 {
		t.Fatalf("api-call-amplification signal count = %d, want 1", got)
	}
	if len(result.Recommendations) == 0 {
		t.Fatalf("expected recommendations, got none")
	}
}

func TestTokensProfileCmd_JSONOutputReportsAPICallOnlyCheckpoints(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	writeProfileTokenCheckpoint(ctx, t, store, "250bbb000001", "profile-json-api-only", &agent.TokenUsage{
		APICallCount: 25,
	})

	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"profile", "--json"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result tokensProfileReport
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected valid JSON, got parse error: %v\noutput: %s", err, stdout.String())
	}
	if result.CheckpointsWithTokenData != 1 {
		t.Fatalf("checkpoints_with_token_data = %d, want 1", result.CheckpointsWithTokenData)
	}
	if result.MissingTokenData != 0 {
		t.Fatalf("missing_token_data = %d, want 0", result.MissingTokenData)
	}
	if result.Tokens == nil || result.Tokens.Total != 0 || result.Tokens.APICalls != 25 {
		t.Fatalf("unexpected token usage: %+v", result.Tokens)
	}
	if got := signalCount(result.Signals, "api-call-amplification"); got != 1 {
		t.Fatalf("api-call-amplification signal count = %d, want 1", got)
	}
}

func TestTokensProfileCmd_LimitScopesAnalyzedCheckpoints(t *testing.T) {
	repo, _ := runExplainAutoTestRepo(t)
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())

	writeProfileTokenCheckpoint(ctx, t, store, "300ccc000001", "profile-limit-one", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 100,
		APICallCount: 1,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "300ccc000002", "profile-limit-two", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 100,
		APICallCount: 1,
	})
	writeProfileTokenCheckpoint(ctx, t, store, "300ccc000003", "profile-limit-three", &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 100,
		APICallCount: 1,
	})

	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"profile", "--limit", "2"})

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Checkpoints available: 3",
		"Checkpoints analyzed: 2",
		"Total:  400 tokens",
		"Limited to latest 2 of 3 committed checkpoints",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
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

	cmd := newTokensGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"profile"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	checks := []string{
		"Token profile",
		"Checkpoints analyzed: 0",
		"Token data: unavailable",
		"No committed checkpoints found.",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("expected %q in output, got:\n%s", check, out)
		}
	}
}

func signalCount(signals []tokensProfileSignal, id string) int {
	for _, signal := range signals {
		if signal.ID == id {
			return signal.Count
		}
	}
	return 0
}

func writeProfileTokenCheckpoint(ctx context.Context, t *testing.T, store *checkpoint.GitStore, checkpointID string, sessionID string, usage *agent.TokenUsage) {
	t.Helper()

	if err := store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: id.MustCheckpointID(checkpointID),
		SessionID:    sessionID,
		Strategy:     strategy.StrategyNameManualCommit,
		Branch:       "tokens-profile",
		Agent:        testAgentClaude,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"profile"}]}}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		TokenUsage:   usage,
	}); err != nil {
		t.Fatalf("WriteCommitted(%s) error = %v", checkpointID, err)
	}
}
