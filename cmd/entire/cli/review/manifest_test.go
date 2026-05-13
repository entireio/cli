package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const manifestTestCodexAgent = "codex"
const manifestTokenTestAgentName agenttypes.AgentName = "review-token-test"
const manifestTokenTestAgentType agenttypes.AgentType = "Review Token Test"

func TestHydrateReviewSummaryTokensFromStates_PopulatesTokensFromSessionState(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{Name: manifestTestCodexAgent, Status: reviewtypes.AgentStatusSucceeded},
		},
	}
	states := []*session.State{
		{
			SessionID:    "codex-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agent.AgentTypeCodex,
			TokenUsage: &agent.TokenUsage{
				InputTokens:         1000,
				CacheCreationTokens: 30,
				CacheReadTokens:     200,
				OutputTokens:        80,
				SubagentTokens: &agent.TokenUsage{
					InputTokens:  5,
					OutputTokens: 6,
				},
			},
		},
	}

	got := hydrateReviewSummaryTokensFromStates(context.Background(), "/repo", "abc123", summary, states, nil)
	tokens := got.AgentRuns[0].Tokens
	if tokens.In != 1235 || tokens.Out != 86 {
		t.Fatalf("tokens = {%d %d}, want {1235 86}", tokens.In, tokens.Out)
	}
}

func TestHydrateReviewSummaryTokensFromStates_FallsBackToTranscript(t *testing.T) {
	t.Parallel()
	lookup := func(agentType agenttypes.AgentType) (agent.Agent, error) {
		if agentType != manifestTokenTestAgentType {
			return nil, errors.New("unexpected agent type")
		}
		return manifestTokenTestAgent{}, nil
	}

	started := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	tmp := t.TempDir()
	transcriptPath := filepath.Join(tmp, "review.jsonl")
	transcript := "review transcript\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{Name: string(manifestTokenTestAgentName), Status: reviewtypes.AgentStatusSucceeded},
		},
	}
	states := []*session.State{
		{
			SessionID:      "review-token-session",
			Kind:           session.KindAgentReview,
			WorktreePath:   "/repo",
			BaseCommit:     "abc123",
			StartedAt:      started.Add(time.Second),
			AgentType:      manifestTokenTestAgentType,
			TranscriptPath: transcriptPath,
		},
	}

	got := hydrateReviewSummaryTokensFromStates(context.Background(), "/repo", "abc123", summary, states, lookup)
	tokens := got.AgentRuns[0].Tokens
	if tokens.In != 150 || tokens.Out != 50 {
		t.Fatalf("tokens = {%d %d}, want {150 50}", tokens.In, tokens.Out)
	}
	if slices.Contains(agent.List(), manifestTokenTestAgentName) {
		t.Fatalf("test agent %q leaked into global registry", manifestTokenTestAgentName)
	}
}

func TestReviewSummaryTokenEnricher_LoadsCurrentSessionState(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	started := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &session.State{
		SessionID:    "codex-session-token",
		Kind:         session.KindAgentReview,
		WorktreePath: repoRoot,
		BaseCommit:   "abc123",
		StartedAt:    started.Add(time.Second),
		AgentType:    agent.AgentTypeCodex,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  12,
			OutputTokens: 5,
		},
	}); err != nil {
		t.Fatalf("save session state: %v", err)
	}

	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{Name: manifestTestCodexAgent, Status: reviewtypes.AgentStatusSucceeded},
		},
	}
	got := reviewSummaryTokenEnricher(repoRoot, "abc123")(ctx, summary)
	tokens := got.AgentRuns[0].Tokens
	if tokens.In != 12 || tokens.Out != 5 {
		t.Fatalf("tokens = {%d %d}, want {12 5}", tokens.In, tokens.Out)
	}

	gotRun := reviewAgentRunTokenEnricher(repoRoot, "abc123")(ctx, reviewtypes.AgentRun{
		Name:      manifestTestCodexAgent,
		StartedAt: started,
	})
	runTokens := gotRun.Tokens
	if runTokens.In != 12 || runTokens.Out != 5 {
		t.Fatalf("agent run tokens = {%d %d}, want {12 5}", runTokens.In, runTokens.Out)
	}
}

func TestLocalReviewManifest_ResolveByAnySessionID(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	manifest := LocalReviewManifest{
		Version:      1,
		WorktreePath: repoRoot,
		CreatedAt:    time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		StartingSHA:  "abc123",
		Sources: []ManifestSource{
			{
				SessionID: "claude-session",
				Agent:     "claude-code",
				Label:     "Claude Code",
				Output:    "H1. Claude finding",
			},
			{
				SessionID: "codex-session",
				Agent:     manifestTestCodexAgent,
				Label:     "Codex",
				Output:    "M1. Codex finding",
			},
		},
		AggregateOutput: "Combined summary",
	}

	if err := writeLocalReviewManifest(context.Background(), manifest); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}

	got, matched, err := resolveLocalReviewManifestBySessionID(context.Background(), repoRoot, "codex-session")
	if err != nil {
		t.Fatalf("resolveLocalReviewManifestBySessionID: %v", err)
	}
	if matched.SessionID != "codex-session" {
		t.Fatalf("matched session = %q, want codex-session", matched.SessionID)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(got.Sources))
	}
	if got.AggregateOutput != "Combined summary" {
		t.Fatalf("aggregate output = %q", got.AggregateOutput)
	}
}

func TestLocalReviewManifest_PrefixMatchWithinSameManifestDoesNotAmbiguate(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	manifest := LocalReviewManifest{
		Version:      1,
		WorktreePath: repoRoot,
		CreatedAt:    time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		StartingSHA:  "abc123",
		Sources: []ManifestSource{
			{
				SessionID: "review-session-claude",
				Agent:     "claude-code",
				Label:     "Claude Code",
				Output:    "H1. Claude finding",
			},
			{
				SessionID: "review-session-codex",
				Agent:     manifestTestCodexAgent,
				Label:     "Codex",
				Output:    "M1. Codex finding",
			},
		},
	}

	if err := writeLocalReviewManifest(context.Background(), manifest); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}

	got, _, err := resolveLocalReviewManifestBySessionID(context.Background(), repoRoot, "review-session")
	if err != nil {
		t.Fatalf("resolveLocalReviewManifestBySessionID: %v", err)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(got.Sources))
	}
}

func TestComposeReviewFixPrompt_UsesSelectedSources(t *testing.T) {
	manifest := LocalReviewManifest{
		WorktreePath: "/repo",
		Sources: []ManifestSource{
			{
				SessionID: "claude-session",
				Agent:     "claude-code",
				Label:     "Claude Code",
				Output:    "H1. Claude finding",
			},
			{
				SessionID: "codex-session",
				Agent:     manifestTestCodexAgent,
				Label:     "Codex",
				Output:    "M1. Codex finding",
			},
		},
		AggregateOutput: "Aggregate finding",
	}

	prompt := composeReviewFixPrompt(manifest, []reviewFixSource{
		{Kind: reviewFixSourceAgent, Label: "Codex", Output: "M1. Codex finding"},
		{Kind: reviewFixSourceAggregate, Label: "Aggregate summary", Output: "Aggregate finding"},
	})

	for _, want := range []string{
		"Fix only the selected review findings.",
		"Codex",
		"M1. Codex finding",
		"Aggregate summary",
		"Aggregate finding",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "H1. Claude finding") {
		t.Fatalf("prompt should not include unselected Claude output:\n%s", prompt)
	}
}

func TestWriteReviewCompletionFooter_PrintsExactFixCommands(t *testing.T) {
	manifest := LocalReviewManifest{
		Sources: []ManifestSource{{SessionID: "claude-session", Label: "Claude Code"}},
	}
	var b strings.Builder

	writeReviewCompletionFooter(&b, manifest)

	got := b.String()
	for _, want := range []string{
		"Review complete.",
		"entire review --fix claude-session --all",
		"entire review --fix claude-session",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer missing %q:\n%s", want, got)
		}
	}
}

func TestPrintReviewFindingsList_PrintsProductionCommandName(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"/tmp/local-build/entire"}

	manifest := LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources: []ManifestSource{{
			SessionID: "claude-session",
			Label:     "Claude Code",
			Output:    "H1. finding",
		}},
	}
	var b strings.Builder

	printReviewFindingsList(&b, []LocalReviewManifest{manifest})

	got := b.String()
	if strings.Contains(got, "/tmp/local-build/entire") {
		t.Fatalf("findings list should not print local binary path:\n%s", got)
	}
	if !strings.Contains(got, "entire review --fix claude-session --all") {
		t.Fatalf("findings list missing production command:\n%s", got)
	}
}

func TestReviewFixSourcesForManifest_AddsAggregateFallbackForMultipleAgents(t *testing.T) {
	manifest := LocalReviewManifest{
		Sources: []ManifestSource{
			{
				SessionID: "claude-session",
				Agent:     "claude-code",
				Label:     "Claude Code",
				Output:    "H1. Claude finding",
			},
			{
				SessionID: "codex-session",
				Agent:     manifestTestCodexAgent,
				Label:     "Codex",
				Output:    "M1. Codex finding",
			},
		},
	}

	sources := reviewFixSourcesForManifest(manifest)

	if len(sources) != 3 {
		t.Fatalf("sources = %d, want 3: %#v", len(sources), sources)
	}
	aggregate := sources[2]
	if aggregate.Kind != reviewFixSourceAggregate {
		t.Fatalf("aggregate kind = %q, want %q", aggregate.Kind, reviewFixSourceAggregate)
	}
	if aggregate.Label != "Aggregate findings" {
		t.Fatalf("aggregate label = %q", aggregate.Label)
	}
	for _, want := range []string{"Claude Code findings", "H1. Claude finding", "Codex findings", "M1. Codex finding"} {
		if !strings.Contains(aggregate.Output, want) {
			t.Fatalf("aggregate output missing %q:\n%s", want, aggregate.Output)
		}
	}
}

func TestReviewPickerHeight_ShowsAllSmallOptionSets(t *testing.T) {
	for _, optionCount := range []int{1, 2, 3, 4} {
		if got := reviewPickerHeight(optionCount); got < optionCount+2 {
			t.Fatalf("height for %d options = %d, want at least %d", optionCount, got, optionCount+2)
		}
	}
}

func TestReviewFixSourcePickerTitle_IncludesSessionHandle(t *testing.T) {
	manifest := LocalReviewManifest{
		Sources: []ManifestSource{{SessionID: "073be48b-2a68-473e-b783-9fa7b78a85aa"}},
	}

	got := reviewFixSourcePickerTitle(manifest)

	if !strings.Contains(got, "073be48b-2a68-473e-b783-9fa7b78a85aa") {
		t.Fatalf("title = %q, want session id", got)
	}
}

func TestReviewFixAgentFromSelectedSources_UsesSingleAgentSource(t *testing.T) {
	got, ok := reviewFixAgentFromSelectedSources([]reviewFixSource{
		{Kind: reviewFixSourceAgent, Agent: manifestTestCodexAgent, Label: "Codex findings"},
	})

	if !ok {
		t.Fatal("expected single-source agent inference")
	}
	if got != manifestTestCodexAgent {
		t.Fatalf("agent = %q, want codex", got)
	}
}

func TestReviewFixAgentFromSelectedSources_DoesNotInferForAggregateOrMultiple(t *testing.T) {
	tests := []struct {
		name    string
		sources []reviewFixSource
	}{
		{
			name: "aggregate",
			sources: []reviewFixSource{
				{Kind: reviewFixSourceAggregate, Label: "Aggregate summary"},
			},
		},
		{
			name: "multiple agents",
			sources: []reviewFixSource{
				{Kind: reviewFixSourceAgent, Agent: "claude-code"},
				{Kind: reviewFixSourceAgent, Agent: manifestTestCodexAgent},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reviewFixAgentFromSelectedSources(tc.sources)
			if ok {
				t.Fatalf("agent = %q, want no inference", got)
			}
		})
	}
}

func TestSavedReviewFixAgentPick_UsesSavedWhenAvailable(t *testing.T) {
	choices := []AgentChoice{
		{Name: "claude-code", Label: "Claude Code"},
		{Name: manifestTestCodexAgent, Label: "Codex"},
	}

	got, ok := savedReviewFixAgentPick(choices, manifestTestCodexAgent)

	if !ok {
		t.Fatal("expected saved agent match")
	}
	if got != manifestTestCodexAgent {
		t.Fatalf("saved pick = %q, want codex", got)
	}
}

func TestSavedReviewFixAgentPick_RejectsUnknownSavedAgent(t *testing.T) {
	choices := []AgentChoice{{Name: "claude-code", Label: "Claude Code"}}

	got, ok := savedReviewFixAgentPick(choices, manifestTestCodexAgent)

	if ok {
		t.Fatalf("saved pick = %q, want no match", got)
	}
}

func TestPickReviewFixAgentPreference_PreservesCurrentWhenNoChoices(t *testing.T) {
	t.Parallel()

	got, err := pickReviewFixAgentPreference(context.Background(), nil, manifestTestCodexAgent)
	if err != nil {
		t.Fatalf("pickReviewFixAgentPreference: %v", err)
	}
	if got != manifestTestCodexAgent {
		t.Fatalf("fix agent = %q, want codex", got)
	}
}

func TestBuildLocalReviewManifestFromSummary_GroupsAgentSessionsAndAggregate(t *testing.T) {
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:   "claude-code",
				Status: reviewtypes.AgentStatusSucceeded,
				Buffer: []reviewtypes.Event{
					reviewtypes.AssistantText{Text: "Claude finding"},
				},
			},
			{
				Name:   manifestTestCodexAgent,
				Status: reviewtypes.AgentStatusSucceeded,
				Buffer: []reviewtypes.Event{
					reviewtypes.AssistantText{Text: "Codex finding"},
				},
			},
		},
	}
	states := []*session.State{
		{
			SessionID:    "claude-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
		},
		{
			SessionID:    "codex-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second),
			AgentType:    agenttypes.AgentType("Codex"),
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "Aggregate finding")

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(manifest.Sources))
	}
	if manifest.Sources[0].SessionID != "claude-session" || manifest.Sources[0].Output != "Claude finding" {
		t.Fatalf("claude source mismatch: %#v", manifest.Sources[0])
	}
	if manifest.Sources[1].SessionID != "codex-session" || manifest.Sources[1].Output != "Codex finding" {
		t.Fatalf("codex source mismatch: %#v", manifest.Sources[1])
	}
	if manifest.AggregateOutput != "Aggregate finding" {
		t.Fatalf("AggregateOutput = %q", manifest.AggregateOutput)
	}
}

func TestWarnManifestNotWritten_PrintsReasonAndDiagnosticHints(t *testing.T) {
	var b strings.Builder

	warnManifestNotWritten(&b, "test reason text")

	got := b.String()
	for _, want := range []string{
		"Note: review skills ran but findings were not persisted.",
		"Reason: test reason text",
		"`entire review --findings` and `entire review --fix` will not see this run.",
		"`ENTIRE_LOG_LEVEL=debug`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q:\n%s", want, got)
		}
	}
}

func TestWritePostReviewManifest_WarnsWhenNoMatchingSessions(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	var out strings.Builder
	summary := reviewtypes.RunSummary{
		StartedAt: time.Now(),
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "claude-code", Status: reviewtypes.AgentStatusSucceeded},
		},
	}

	// SHA is irrelevant: matcher never runs since no session states exist.
	writePostReviewManifest(context.Background(), &out, repoRoot, "abc123", summary, "")

	got := out.String()
	if !strings.Contains(got, "Note: review skills ran but findings were not persisted.") {
		t.Fatalf("expected warning to fire when no sessions match; got:\n%s", got)
	}
	if !strings.Contains(got, "env-var handshake did not reach the hook") {
		t.Fatalf("expected handshake-failure reason; got:\n%s", got)
	}
	if strings.Contains(got, "Review complete.") {
		t.Fatalf("happy-path footer must not print when manifest is empty; got:\n%s", got)
	}
}

type manifestTokenTestAgent struct{}

func (manifestTokenTestAgent) Name() agenttypes.AgentName { return manifestTokenTestAgentName }
func (manifestTokenTestAgent) Type() agenttypes.AgentType { return manifestTokenTestAgentType }
func (manifestTokenTestAgent) Description() string        { return "review token test agent" }
func (manifestTokenTestAgent) IsPreview() bool            { return false }
func (manifestTokenTestAgent) DetectPresence(context.Context) (bool, error) {
	return false, nil
}
func (manifestTokenTestAgent) ProtectedDirs() []string { return nil }
func (manifestTokenTestAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	return os.ReadFile(sessionRef)
}
func (manifestTokenTestAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}
func (manifestTokenTestAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	return chunks[0], nil
}
func (manifestTokenTestAgent) GetSessionID(*agent.HookInput) string { return "" }
func (manifestTokenTestAgent) GetSessionDir(string) (string, error) { return "", nil }
func (manifestTokenTestAgent) ResolveSessionFile(_, _ string) string {
	return ""
}
func (manifestTokenTestAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return &agent.AgentSession{}, nil
}
func (manifestTokenTestAgent) WriteSession(context.Context, *agent.AgentSession) error {
	return nil
}
func (manifestTokenTestAgent) FormatResumeCommand(string) string { return "" }
func (manifestTokenTestAgent) CalculateTokenUsage(content []byte, _ int) (*agent.TokenUsage, error) {
	if len(content) == 0 {
		return nil, errors.New("empty transcript")
	}
	return &agent.TokenUsage{
		InputTokens:     100,
		CacheReadTokens: 50,
		OutputTokens:    50,
	}, nil
}
