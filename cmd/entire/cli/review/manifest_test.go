package review

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
const manifestFindingsFlag = "--findings"
const manifestTestFinding = "finding"

func TestHydrateReviewSummaryTokensFromStates_PopulatesTokensFromSessionState(t *testing.T) {
	t.Parallel()
	started := time.Now().UTC().Truncate(time.Second)
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

	started := time.Now().UTC().Truncate(time.Second)
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
	started := time.Now().UTC().Truncate(time.Second)
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

	gotRun := reviewAgentRunTokenEnricherForRuns(repoRoot, "abc123", nil)(ctx, reviewtypes.AgentRun{
		Name:      manifestTestCodexAgent,
		StartedAt: started,
	})
	runTokens := gotRun.Tokens
	if runTokens.In != 12 || runTokens.Out != 5 {
		t.Fatalf("agent run tokens = {%d %d}, want {12 5}", runTokens.In, runTokens.Out)
	}
}

func TestWriteReviewCompletionFooter_PointsToFindings(t *testing.T) {
	manifest := LocalReviewManifest{
		Sources: []ManifestSource{{SessionID: "claude-session", Label: "Claude Code"}},
	}
	var b strings.Builder

	writeReviewCompletionFooter(&b, manifest)

	got := b.String()
	for _, want := range []string{"Review complete.", "entire review --findings 'claude-session'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--fix") {
		t.Fatalf("footer should not reference removed --fix:\n%s", got)
	}
}

func TestWriteReviewCompletionFooter_UsesTimestampWhenAvailable(t *testing.T) {
	manifest := LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources:   []ManifestSource{{SessionID: "claude-session", Label: "Claude Code"}},
	}
	var b strings.Builder

	writeReviewCompletionFooter(&b, manifest)

	got := b.String()
	if !strings.Contains(got, "entire review --findings '20260507T100000'") {
		t.Fatalf("footer missing timestamp view command:\n%s", got)
	}
	if strings.Contains(got, "entire review --findings 'claude-session'") {
		t.Fatalf("footer should not advertise session handle when timestamp is available:\n%s", got)
	}
}

func TestReviewFindingCommandsQuoteShellHandles(t *testing.T) {
	manifest := LocalReviewManifest{
		Sources: []ManifestSource{{
			SessionID: "sid; echo pwned",
			Label:     "Claude Code",
			Output:    manifestTestFinding,
		}},
	}
	var list strings.Builder
	var footer strings.Builder

	printReviewFindingsList(&list, []LocalReviewManifest{manifest})
	writeReviewCompletionFooter(&footer, manifest)

	want := "entire review --findings 'sid; echo pwned'"
	if !strings.Contains(list.String(), "view: "+want) {
		t.Fatalf("list output missing quoted view command:\n%s", list.String())
	}
	if !strings.Contains(footer.String(), want) {
		t.Fatalf("footer output missing quoted view command:\n%s", footer.String())
	}
}

func TestPrintReviewFindingsList_UsesTimestampForDuplicateSessionHandles(t *testing.T) {
	manifests := []LocalReviewManifest{
		{
			CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
			Sources: []ManifestSource{{
				SessionID: "claude-session",
				Label:     "Claude Code",
				Output:    "first finding",
			}},
		},
		{
			CreatedAt: time.Date(2026, 5, 7, 10, 5, 0, 0, time.UTC),
			Sources: []ManifestSource{{
				SessionID: "claude-session",
				Label:     "Claude Code",
				Output:    "second finding",
			}},
		},
	}
	var b strings.Builder

	printReviewFindingsList(&b, manifests)

	got := b.String()
	for _, want := range []string{
		"view: entire review --findings '20260507T100000'",
		"view: entire review --findings '20260507T100500'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("findings list missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "view: entire review --findings 'claude-session'") {
		t.Fatalf("findings list should not advertise ambiguous session handle:\n%s", got)
	}
}

func TestPrintReviewFindingsList_ListsSessionsWithoutLocalPath(t *testing.T) {
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

	printReviewFindingsList(&b, []LocalReviewManifest{
		manifest,
		{
			CreatedAt:       time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC),
			AggregateOutput: "aggregate-only finding",
		},
	})

	got := b.String()
	if strings.Contains(got, "/tmp/local-build/entire") {
		t.Fatalf("findings list should not print local binary path:\n%s", got)
	}
	if !strings.Contains(got, "claude-session") {
		t.Fatalf("findings list missing session handle:\n%s", got)
	}
	if !strings.Contains(got, "view: entire review --findings 'claude-session'") {
		t.Fatalf("findings list missing view command:\n%s", got)
	}
	if !strings.Contains(got, "view: entire review --findings '20260508T110000'") {
		t.Fatalf("findings list missing timestamp fallback handle:\n%s", got)
	}
}

func TestReviewFindingsCommand_ProfileFlagListsFindings(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "init")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	if err := writeLocalReviewManifest(context.Background(), LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources: []ManifestSource{{
			SessionID: "claude-session",
			Label:     "Claude Code",
			Output:    manifestTestFinding,
		}},
	}); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}

	cmd := NewCommand(Deps{})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{manifestFindingsFlag, "--profile", "general"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute review --findings --profile: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Review Findings", "view: entire review --findings 'claude-session'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("findings list missing %q:\n%s", want, got)
		}
	}
}

func TestReviewFindingsCommand_WithHandlePrintsFullDetail(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "init")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	if err := writeLocalReviewManifest(context.Background(), LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources: []ManifestSource{{
			SessionID: "claude-session",
			Label:     "Claude Code",
			Output:    "Full finding body that must not be hidden behind a picker.",
		}},
		AggregateOutput: "Aggregate detail for agents.",
	}); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}

	cmd := NewCommand(Deps{})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{manifestFindingsFlag, "claude-session"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute review --findings handle: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Review findings from",
		"Full finding body that must not be hidden behind a picker.",
		"Aggregate detail for agents.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail output missing %q:\n%s", want, got)
		}
	}
}

func TestReviewFindingsCommand_HandleWithNoManifestsFails(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "init")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	cmd := NewCommand(Deps{})
	var errOut strings.Builder
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{manifestFindingsFlag, "missing-session"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unknown findings handle to fail")
	}
	got := errOut.String()
	if !strings.Contains(got, `no local review findings match "missing-session"`) {
		t.Fatalf("no-manifest handle error mismatch:\n%s", got)
	}
}

func TestReviewFindingsCommand_UnknownHandleListsValidHandles(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "init")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	if err := writeLocalReviewManifest(context.Background(), LocalReviewManifest{
		CreatedAt: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		Sources: []ManifestSource{{
			SessionID: "claude-session; echo pwned",
			Label:     "Claude Code",
			Output:    manifestTestFinding,
		}},
	}); err != nil {
		t.Fatalf("writeLocalReviewManifest: %v", err)
	}

	cmd := NewCommand(Deps{})
	var errOut strings.Builder
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{manifestFindingsFlag, "missing-session"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unknown findings handle to fail")
	}
	got := errOut.String()
	for _, want := range []string{"no local review findings match", "view: entire review --findings 'claude-session; echo pwned'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unknown-handle error missing %q:\n%s", want, got)
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

func TestSavedAgentPick_UsesSavedWhenAvailable(t *testing.T) {
	choices := []AgentChoice{
		{Name: "claude-code", Label: "Claude Code"},
		{Name: manifestTestCodexAgent, Label: "Codex"},
	}

	got, ok := savedAgentPick(choices, manifestTestCodexAgent)

	if !ok {
		t.Fatal("expected saved agent match")
	}
	if got != manifestTestCodexAgent {
		t.Fatalf("saved pick = %q, want codex", got)
	}
}

func TestSavedAgentPick_RejectsUnknownSavedAgent(t *testing.T) {
	choices := []AgentChoice{{Name: "claude-code", Label: "Claude Code"}}

	got, ok := savedAgentPick(choices, manifestTestCodexAgent)

	if ok {
		t.Fatalf("saved pick = %q, want no match", got)
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

func TestBuildLocalReviewManifestFromSummary_DisambiguatesSameAgentByModel(t *testing.T) {
	started := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:      "pi-sonnet",
				AgentName: "pi",
				Model:     "anthropic/claude-sonnet:high",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "Sonnet finding"}},
			},
			{
				Name:      "pi-opus",
				AgentName: "pi",
				Model:     "opus",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "Opus finding"}},
			},
		},
	}
	states := []*session.State{
		{
			SessionID:    "opus-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			ModelName:    "claude-opus-4-1",
		},
		{
			SessionID:    "sonnet-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second),
			ModelName:    "claude-sonnet-4-5",
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2: %#v", len(manifest.Sources), manifest.Sources)
	}
	if manifest.Sources[0].SessionID != "sonnet-session" || manifest.Sources[0].Label != "pi-sonnet" {
		t.Fatalf("sonnet source mismatch: %#v", manifest.Sources[0])
	}
	if manifest.Sources[1].SessionID != "opus-session" || manifest.Sources[1].Label != "pi-opus" {
		t.Fatalf("opus source mismatch: %#v", manifest.Sources[1])
	}
}

func TestWarnManifestNotWritten_PrintsReasonAndDiagnosticHints(t *testing.T) {
	var b strings.Builder

	warnManifestNotWritten(&b, "test reason text")

	got := b.String()
	for _, want := range []string{
		"Note: review skills ran but findings were not persisted.",
		"Reason: test reason text",
		"`entire review --findings` will not see this run.",
		"`ENTIRE_LOG_LEVEL=debug`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q:\n%s", want, got)
		}
	}
}

func TestWritePostReviewManifest_PersistsWithoutMatchingSessions(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: "sibling-session", Kind: session.KindAgentReview,
		WorktreePath: repoRoot + "-sibling", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	summary := reviewtypes.RunSummary{
		StartedAt: time.Now(),
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "claude-code", Status: reviewtypes.AgentStatusSucceeded},
		},
	}

	if err := writePostReviewManifest(context.Background(), &out, repoRoot, "abc123", summary, "verdict", false); err != nil {
		t.Fatal(err)
	}
	manifests, err := loadLocalReviewManifests(context.Background(), repoRoot)
	if err != nil || len(manifests) != 1 || manifests[0].AggregateOutput != "verdict" || len(manifests[0].Sources) != 1 || manifests[0].Sources[0].SessionID != "" {
		t.Fatalf("findings = %#v, err = %v", manifests, err)
	}
}

func TestWritePostReviewManifest_ReportsWriteFailure(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)
	if err := os.WriteFile(filepath.Join(repoRoot, ".git", "entire-review"), []byte("block directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := reviewtypes.RunSummary{StartedAt: time.Now(), AgentRuns: []reviewtypes.AgentRun{{Name: "claude-code"}}}
	var out strings.Builder
	if err := writePostReviewManifest(context.Background(), &out, repoRoot, "", summary, "", false); err == nil {
		t.Fatal("expected persistence error")
	}
	if !strings.Contains(out.String(), "findings were not persisted") {
		t.Fatalf("missing warning: %s", out.String())
	}
}

func TestReviewManifestsWithoutSessionsInSameSecond(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)
	started := time.Date(2026, 5, 7, 10, 0, 0, 1, time.UTC)
	for i, verdict := range []string{"first", "second"} {
		summary := reviewtypes.RunSummary{
			StartedAt: started.Add(time.Duration(i) * time.Nanosecond),
			AgentRuns: []reviewtypes.AgentRun{{Name: "pi", Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: verdict}}}},
		}
		if err := writePostReviewManifest(context.Background(), io.Discard, repoRoot, "", summary, "", false); err != nil {
			t.Fatal(err)
		}
	}
	manifests, err := loadLocalReviewManifests(context.Background(), repoRoot)
	if err != nil || len(manifests) != 2 {
		t.Fatalf("manifests = %#v, err = %v; want both reviews", manifests, err)
	}
	for _, manifest := range manifests {
		handle := reviewManifestCompletionHandle(manifest)
		found, err := findReviewManifestByHandle(manifests, handle)
		if err != nil || found.Sources[0].Output != manifest.Sources[0].Output {
			t.Fatalf("lookup %q = %#v, %v", handle, found, err)
		}
	}
}

// Findings from finished workers must persist even if the run context is
// cancelled during finalize (summary not cancelled).
func TestWritePostReviewManifest_SurvivesCancelledRunContext(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out strings.Builder
	summary := reviewtypes.RunSummary{
		StartedAt: time.Now(),
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "claude-code", Status: reviewtypes.AgentStatusSucceeded},
		},
	}

	if err := writePostReviewManifest(ctx, &out, repoRoot, "abc123", summary, "", false); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, "context canceled") {
		t.Fatalf("persistence used the cancelled run context; findings would be lost:\n%s", got)
	}
	if !strings.Contains(got, "Review complete.") {
		t.Fatalf("expected completion footer; got:\n%s", got)
	}
}

func TestBuildLocalReviewManifestFromSummary_PartialMatch_NoWarning(t *testing.T) {
	t.Parallel()
	started := time.Now()
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "claude-code", Status: reviewtypes.AgentStatusSucceeded},
			{Name: "codex", Status: reviewtypes.AgentStatusSucceeded},
		},
	}
	// Only one tagged state with the right AgentType for claude-code. codex
	// has no matching tagged state; its output is still saved without an ID.
	states := []*session.State{
		{
			SessionID:    "claude-session",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
		},
	}
	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")
	if len(manifest.Sources) != 2 || manifest.Sources[1].SessionID != "" {
		t.Fatalf("expected both sources without misattribution; got %#v", manifest.Sources)
	}
	if manifest.Sources[0].SessionID != "claude-session" {
		t.Errorf("expected the claude-code source to be matched; got %+v", manifest.Sources[0])
	}
}

type manifestTokenTestAgent struct{}

func (manifestTokenTestAgent) Name() agenttypes.AgentName { return manifestTokenTestAgentName }
func (manifestTokenTestAgent) Type() agenttypes.AgentType { return manifestTokenTestAgentType }
func (manifestTokenTestAgent) Description() string        { return "review token test agent" }
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

func TestReviewRunModelMatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		got  string
		ok   bool
	}{
		{"exact", "gpt-4", "gpt-4", true},
		{"empty want matches anything", "", "claude-sonnet-4-5", true},
		{"empty got matches anything", "sonnet", "", true},
		{"alias matches resolved", "sonnet", "claude-sonnet-4-20250514", true},
		{"family matches resolved", "claude-sonnet", "claude-sonnet-4-5", true},
		{"provider prefix and thinking suffix stripped", "anthropic/claude-sonnet:high", "claude-sonnet-4-5", true},
		{"separator-insensitive", "claude_sonnet", "claude-sonnet-4-5", true},
		{"numeric version suffix matches", "gpt-4o", "gpt-4o-2024-08-06", true},
		{"minor version suffix matches", "claude-sonnet-4", "claude-sonnet-4-5", true},
		// Partial component must not match.
		{"gpt-4 must NOT match gpt-4o-mini", "gpt-4", "gpt-4o-mini", false},
		// Variant suffix (a word, not a version) must not match.
		{"gpt-4o must NOT match gpt-4o-mini", "gpt-4o", "gpt-4o-mini", false},
		{"gpt-4 must NOT match gpt-4-turbo", "gpt-4", "gpt-4-turbo", false},
		// Bare version fragments must not match a model that merely ends in them.
		{"version fragment does not match", "4-5", "claude-sonnet-4-5", false},
		{"different families do not match", "gpt-4o-mini", "claude-sonnet-4-5", false},
		{"opus does not match sonnet", "opus", "claude-sonnet-4-5", false},
		// Identical ids match regardless of component count (via the want==got
		// short-circuit), but two distinct equal-length ids must not.
		{"identical multi-component ids match", "claude-sonnet-4-5", "claude-sonnet-4-5", true},
		{"identical two-component ids match", "claude-sonnet", "claude-sonnet", true},
		{"slash provider prefix stripped then identical", "anthropic/claude-sonnet", "claude-sonnet", true},
		{"family matches across a provider component at offset", "claude-sonnet", "anthropic-claude-sonnet-4-5", true},
		{"match where the next component is the last element", "sonnet-4", "claude-sonnet-4-5", true},
		// Suffix-only spans are intentionally rejected (no version boundary to
		// confirm the same model); this is what also keeps bare fragments and
		// variant words from matching. Realistic configured models still match via
		// the trailing version (see the alias/family cases above).
		{"family+version suffix is intentionally not matched", "sonnet-4", "claude-sonnet-4", false},
		{"variant-word suffix must not match", "mini", "gpt-4o-mini", false},
		{"bare version suffix must not match", "4-5", "claude-sonnet-4", false},
		{"thinking-suffix-only difference matches", "claude-sonnet:high", "claude-sonnet:low", true},
		{"equal-length different family does not match", "claude-sonnet", "claude-opus", false},
		{"equal-length different version does not match", "claude-sonnet-4", "claude-sonnet-5", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewRunModelMatches(c.want, c.got); got != c.ok {
				t.Errorf("reviewRunModelMatches(%q, %q) = %v, want %v", c.want, c.got, got, c.ok)
			}
		})
	}
}

func TestModelComponentsMatchLastComponentBoundary(t *testing.T) {
	t.Parallel()
	long := []string{"anthropic", "claude", "sonnet", "4"}

	if !modelComponentsMatch([]string{"claude", "sonnet"}, long) {
		t.Fatal("span followed by long's last component should match when that component is numeric")
	}
	if modelComponentsMatch([]string{"sonnet", "4"}, long) {
		t.Fatal("suffix span should not match because it has no following boundary component")
	}
	if modelComponentsMatch([]string{"claude", "sonnet"}, []string{"anthropic", "claude", "sonnet", "mini"}) {
		t.Fatal("last-component boundary should not match when the following component is non-numeric")
	}
}

// TestBuildLocalReviewManifestFromSummary_DisambiguatesSameModelDifferentThinking
// pins the used-session tracking: two reviewers on the same agent whose models
// normalize identically (claude-sonnet:high / :low -> claude-sonnet), with
// sessions that start in the same second, must still link to distinct sessions
// rather than both grabbing the most recent match.
func TestComponentsEqualAtBoundsChecks(t *testing.T) {
	t.Parallel()
	long := []string{"claude", "sonnet"}
	short := []string{"sonnet", "4"}

	if componentsEqualAt(long, short, -1) {
		t.Fatal("negative offset should not match")
	}
	if componentsEqualAt(long, short, 1) {
		t.Fatal("span that overruns long should not match")
	}
	if !componentsEqualAt(long, []string{"sonnet"}, 1) {
		t.Fatal("in-bounds span should match")
	}
}

func TestHydrateReviewAgentRunTokensFromStatesWithUsedDisambiguatesDuplicateSlots(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	run := reviewtypes.AgentRun{
		Name:      "claude-code",
		AgentName: "claude-code",
		Model:     "opus",
		StartedAt: started,
	}
	states := []*session.State{
		{
			SessionID:    "sess-older",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-opus-4-1",
			TokenUsage:   &agent.TokenUsage{InputTokens: 20, OutputTokens: 2},
		},
		{
			SessionID:    "sess-newer",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-opus-4-1",
			TokenUsage:   &agent.TokenUsage{InputTokens: 10, OutputTokens: 1},
		},
	}

	freshA := hydrateReviewAgentRunTokensFromStatesWithUsed(context.Background(), "/repo", "abc123", run, states, nil, map[string]bool{})
	freshB := hydrateReviewAgentRunTokensFromStatesWithUsed(context.Background(), "/repo", "abc123", run, states, nil, map[string]bool{})
	if freshA.Tokens.In != 10 || freshB.Tokens.In != 10 {
		t.Fatalf("fresh-map setup changed: tokens = %d/%d, want both newest session token count 10", freshA.Tokens.In, freshB.Tokens.In)
	}

	used := map[string]bool{}
	first := hydrateReviewAgentRunTokensFromStatesWithUsed(context.Background(), "/repo", "abc123", run, states, nil, used)
	second := hydrateReviewAgentRunTokensFromStatesWithUsed(context.Background(), "/repo", "abc123", run, states, nil, used)
	if first.Tokens.In != 10 || first.Tokens.Out != 1 {
		t.Fatalf("first tokens = %+v, want newer session tokens 10/1", first.Tokens)
	}
	if second.Tokens.In != 20 || second.Tokens.Out != 2 {
		t.Fatalf("second tokens = %+v, want older distinct session tokens 20/2", second.Tokens)
	}
	if !used["sess-newer"] || !used["sess-older"] || len(used) != 2 {
		t.Fatalf("used sessions = %#v, want both sessions claimed", used)
	}
}

func TestHydrateReviewAgentRunTokensFromStatesWithPlanClaimsExplicitBeforeDefault(t *testing.T) {
	t.Parallel()
	const (
		sessDefault = "sess-default"
		sessOpus    = "sess-opus"
	)
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	defaultRun := reviewtypes.AgentRun{
		Name:      "claude-code",
		AgentName: "claude-code",
		StartedAt: started,
	}
	opusRun := reviewtypes.AgentRun{
		Name:      "claude-code",
		AgentName: "claude-code",
		Model:     "opus",
		StartedAt: started,
	}
	planned := []reviewtypes.AgentRun{defaultRun, opusRun}
	states := []*session.State{
		{
			SessionID:    sessDefault,
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
			TokenUsage:   &agent.TokenUsage{InputTokens: 20, OutputTokens: 2},
		},
		{
			SessionID:    sessOpus,
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second), // newer: a naive default match would grab this
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-opus-4-1",
			TokenUsage:   &agent.TokenUsage{InputTokens: 10, OutputTokens: 1},
		},
	}

	claimed := make([]bool, len(planned))
	defaultFirst, ok, sessionID := hydrateReviewAgentRunTokensFromStatesWithPlan(context.Background(), "/repo", "abc123", defaultRun, states, nil, planned, started, claimed)
	if !ok {
		t.Fatal("default run did not claim a planned slot")
	}
	if sessionID != sessDefault || defaultFirst.Tokens.In != 20 || defaultFirst.Tokens.Out != 2 {
		t.Fatalf("default run matched session %q tokens %+v, want %s tokens 20/2", sessionID, defaultFirst.Tokens, sessDefault)
	}

	opusSecond, ok, sessionID := hydrateReviewAgentRunTokensFromStatesWithPlan(context.Background(), "/repo", "abc123", opusRun, states, nil, planned, started, claimed)
	if !ok {
		t.Fatal("opus run did not claim a planned slot")
	}
	if sessionID != sessOpus || opusSecond.Tokens.In != 10 || opusSecond.Tokens.Out != 1 {
		t.Fatalf("opus run matched session %q tokens %+v, want %s tokens 10/1", sessionID, opusSecond.Tokens, sessOpus)
	}
}

func TestBuildLocalReviewManifestFromSummary_DisambiguatesSameModelDifferentThinking(t *testing.T) {
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "claude-sonnet:high",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "high finding"}},
			},
			{
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "claude-sonnet:low",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "low finding"}},
			},
		},
	}
	// Both sessions resolve to the same model and start in the same second, so
	// only used-session tracking can keep the two workers on distinct sessions.
	sameStart := started.Add(time.Second)
	states := []*session.State{
		{
			SessionID:    "sess-1",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    sameStart,
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
		},
		{
			SessionID:    "sess-2",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    sameStart,
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2 (each reviewer linked to a session)", len(manifest.Sources))
	}
	a, b := manifest.Sources[0].SessionID, manifest.Sources[1].SessionID
	if a == b {
		t.Fatalf("both reviewers linked to the same session %q; used-session tracking must keep them distinct", a)
	}
	valid := map[string]bool{"sess-1": true, "sess-2": true}
	if !valid[a] || !valid[b] {
		t.Errorf("sessions = {%q, %q}, want the two distinct sessions sess-1 and sess-2", a, b)
	}
}

// TestBuildLocalReviewManifestFromSummary_ExplicitModelClaimedBeforeDefault
// pins the two-pass matching: a default-model reviewer (empty model, which
// matches any recorded model) must not grab an explicit-model reviewer's
// session, even when it appears first and the explicit session is more recent.
func TestBuildLocalReviewManifestFromSummary_ExplicitModelClaimedBeforeDefault(t *testing.T) {
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{ // default-model reviewer, listed first
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "default finding"}},
			},
			{ // explicit opus reviewer
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "opus",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "opus finding"}},
			},
		},
	}
	states := []*session.State{
		{
			SessionID:    "sess-default",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(1 * time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
		},
		{
			SessionID:    "sess-opus",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second), // more recent: a naive default match would grab this
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-opus-4-1",
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(manifest.Sources))
	}
	// Sources keep original run order: [default, opus].
	if manifest.Sources[0].SessionID != "sess-default" {
		t.Errorf("default reviewer linked to %q, want sess-default", manifest.Sources[0].SessionID)
	}
	if manifest.Sources[1].SessionID != "sess-opus" {
		t.Errorf("opus reviewer linked to %q, want sess-opus", manifest.Sources[1].SessionID)
	}
}

// TestBuildLocalReviewManifestFromSummary_ExplicitModelWithoutMatchingSession
// verifies that an explicit-model reviewer with no matching session is left
// unlinked (not force-attributed to the default-model session), and that the
// matched slice stays index-aligned so the default reviewer still links.
func TestBuildLocalReviewManifestFromSummary_ExplicitModelWithoutMatchingSession(t *testing.T) {
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	summary := reviewtypes.RunSummary{
		StartedAt: started,
		AgentRuns: []reviewtypes.AgentRun{
			{ // explicit opus reviewer, but only a sonnet session exists
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "opus",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "opus finding"}},
			},
			{ // default reviewer
				Name:      "claude-code",
				AgentName: "claude-code",
				Model:     "",
				Status:    reviewtypes.AgentStatusSucceeded,
				Buffer:    []reviewtypes.Event{reviewtypes.AssistantText{Text: "default finding"}},
			},
		},
	}
	states := []*session.State{
		{
			SessionID:    "sess-default",
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")

	if len(manifest.Sources) != 2 || manifest.Sources[0].SessionID != "" || manifest.Sources[1].SessionID != "sess-default" || manifest.Sources[1].Output != "default finding" {
		t.Fatalf("sources = %#v, want unmatched opus and matched default", manifest.Sources)
	}
}

// TestBuildLocalReviewManifestFromSummary_ExplicitEmptyModelIsDefault proves
// that a JSON value of "model": "" is indistinguishable from an omitted model
// once decoded into AgentRun.Model, and is therefore treated as a default-model
// reviewer (not as an explicit-model reviewer) by matchSessionsToRuns.
func TestBuildLocalReviewManifestFromSummary_ExplicitEmptyModelIsDefault(t *testing.T) {
	const (
		sessDefault = "sess-default"
		sessOpus    = "sess-opus"
	)
	started := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	type encodedRun struct {
		Name      string                  `json:"name"`
		AgentName string                  `json:"agent_name"`
		Model     string                  `json:"model"`
		Status    reviewtypes.AgentStatus `json:"status"`
	}
	var encoded []encodedRun
	if err := json.Unmarshal([]byte(`[
		{"name":"claude-code","agent_name":"claude-code","model":"","status":1},
		{"name":"claude-code","agent_name":"claude-code","model":"opus","status":1}
	]`), &encoded); err != nil {
		t.Fatalf("unmarshal runs: %v", err)
	}
	runs := make([]reviewtypes.AgentRun, len(encoded))
	for i, run := range encoded {
		runs[i] = reviewtypes.AgentRun{
			Name:      run.Name,
			AgentName: run.AgentName,
			Model:     run.Model,
			Status:    run.Status,
		}
	}
	if runs[0].Model != "" {
		t.Fatalf("explicit empty JSON model decoded as %q, want empty string", runs[0].Model)
	}
	summary := reviewtypes.RunSummary{StartedAt: started, AgentRuns: runs}
	states := []*session.State{
		{
			SessionID:    sessDefault,
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(time.Second),
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-sonnet-4-5",
		},
		{
			SessionID:    sessOpus,
			Kind:         session.KindAgentReview,
			WorktreePath: "/repo",
			BaseCommit:   "abc123",
			StartedAt:    started.Add(2 * time.Second), // more recent, so a single-pass default match would steal it
			AgentType:    agenttypes.AgentType("Claude Code"),
			ModelName:    "claude-opus-4-1",
		},
	}

	manifest := buildLocalReviewManifestFromSummary("/repo", "abc123", summary, states, "")

	if len(manifest.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(manifest.Sources))
	}
	if manifest.Sources[0].SessionID != sessDefault {
		t.Errorf("explicit-empty/default reviewer linked to %q, want %s", manifest.Sources[0].SessionID, sessDefault)
	}
	if manifest.Sources[1].SessionID != sessOpus {
		t.Errorf("opus reviewer linked to %q, want %s", manifest.Sources[1].SessionID, sessOpus)
	}
}
