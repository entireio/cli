package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	git "github.com/go-git/go-git/v6"
)

const (
	fakeReplayAgent     = "fake-agent"
	replayFixtureFile   = "app.py"
	replayTargetContent = "def greet():\n    return 'hello'\n\n\ndef replay_helper():\n    return 'ok'\n"
)

func TestBuildReplaySpecFromCheckpoint(t *testing.T) {
	repoRoot, cpID, base, target := newReplayRepo(t)

	spec, err := buildReplaySpec(context.Background(), cpID)
	if err != nil {
		t.Fatalf("buildReplaySpec() error = %v", err)
	}

	if spec.CheckpointID != cpID {
		t.Fatalf("CheckpointID = %q, want %q", spec.CheckpointID, cpID)
	}
	if spec.BaseCommit != base {
		t.Fatalf("BaseCommit = %q, want %q", spec.BaseCommit, base)
	}
	if spec.TargetCommit != target {
		t.Fatalf("TargetCommit = %q, want %q", spec.TargetCommit, target)
	}
	if spec.Prompt != "Add the replay helper." {
		t.Fatalf("Prompt = %q", spec.Prompt)
	}
	if got := strings.Join(spec.FilesTouched, ","); got != replayFixtureFile {
		t.Fatalf("FilesTouched = %q", got)
	}
	if spec.OriginalAgent != string(agentpkg.AgentTypeClaudeCode) {
		t.Fatalf("OriginalAgent = %q", spec.OriginalAgent)
	}

	if content, err := os.ReadFile(filepath.Join(repoRoot, replayFixtureFile)); err != nil || !strings.Contains(string(content), "replay_helper") {
		t.Fatalf("fixture target file not written: %v", err)
	}
}

func TestBuildReplaySpecFallsBackToTranscriptPrompt(t *testing.T) {
	_, cpID, _, _ := newReplayRepoWithPrompts(t, nil, []byte(`{"type":"user","uuid":"u1","message":{"content":"Replay this transcript prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"Done"}]}}
`))

	spec, err := buildReplaySpec(context.Background(), cpID)
	if err != nil {
		t.Fatalf("buildReplaySpec() error = %v", err)
	}
	if spec.Prompt != "Replay this transcript prompt" {
		t.Fatalf("Prompt = %q", spec.Prompt)
	}
}

func TestBuildReplaySpecFallsBackToGitDiffFiles(t *testing.T) {
	_, cpID, _, _ := newReplayRepoWithOptions(t, replayRepoOptions{
		Prompts:      []string{"Add the replay helper."},
		Transcript:   []byte(`{"type":"user","uuid":"u1","message":{"content":"Add the replay helper."}}` + "\n"),
		FilesTouched: nil,
	})

	spec, err := buildReplaySpec(context.Background(), cpID)
	if err != nil {
		t.Fatalf("buildReplaySpec() error = %v", err)
	}
	if got := strings.Join(spec.FilesTouched, ","); got != replayFixtureFile {
		t.Fatalf("FilesTouched = %q, want git diff fallback %s", got, replayFixtureFile)
	}
}

func TestReplayCheckpointUsesIsolatedWorktreeAndSavesResult(t *testing.T) {
	repoRoot, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "fake replay completed"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{
		Agent:       fakeReplayAgent,
		TestCommand: "python3 -m py_compile " + replayFixtureFile,
	})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}

	if run.Status != replayStatusPassed {
		t.Fatalf("Status = %q, error = %s", run.Status, run.Error)
	}
	if run.WorktreePath != "" {
		t.Fatalf("WorktreePath should be empty when keep-worktree=false, got %q", run.WorktreePath)
	}
	if run.Metrics.FileRecall != 100 || run.Metrics.FilePrecision != 100 {
		t.Fatalf("metrics = %+v", run.Metrics)
	}
	if run.Test.Status != replayStatusPassed {
		t.Fatalf("test status = %q output=%s", run.Test.Status, run.Test.Output)
	}
	if run.ResultPath == "" {
		t.Fatal("ResultPath is empty")
	}
	if _, err := os.Stat(run.ResultPath); err != nil {
		t.Fatalf("saved result missing: %v", err)
	}

	mainContent, err := os.ReadFile(filepath.Join(repoRoot, replayFixtureFile))
	if err != nil {
		t.Fatalf("read main worktree: %v", err)
	}
	if !strings.Contains(string(mainContent), "replay_helper") {
		t.Fatalf("main worktree should remain at target commit content, got:\n%s", mainContent)
	}
}

func TestReplayCheckpointKeepWorktreePreservesPath(t *testing.T) {
	repoRoot, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, _ ReplayRunnerRequest) (ReplayRunnerResult, error) {
		return ReplayRunnerResult{Output: "no changes"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{
		Agent:        fakeReplayAgent,
		KeepWorktree: true,
	})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if run.WorktreePath == "" {
		t.Fatal("WorktreePath is empty")
	}
	if _, err := os.Stat(run.WorktreePath); err != nil {
		t.Fatalf("kept worktree missing: %v", err)
	}
	t.Cleanup(func() {
		if err := removeReplayWorktree(context.Background(), repoRoot, run.WorktreePath); err != nil {
			t.Errorf("remove replay worktree: %v", err)
		}
	})
}

func TestReplayCheckpointCapturesCommittedAgentResult(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		if _, err := replayGit(ctx, req.WorktreePath, "add", replayFixtureFile); err != nil {
			return ReplayRunnerResult{}, err
		}
		if _, err := replayGit(ctx, req.WorktreePath,
			"-c", "user.name=Replay Agent",
			"-c", "user.email=replay@example.com",
			"commit", "--no-gpg-sign", "-m", "agent replay result",
		); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "committed replay completed"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{Agent: fakeReplayAgent})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if got := strings.Join(run.ChangedFiles, ","); got != replayFixtureFile {
		t.Fatalf("ChangedFiles = %q", got)
	}
	if !strings.Contains(run.Diff, "replay_helper") {
		t.Fatalf("Diff does not include committed replay result:\n%s", run.Diff)
	}
	if run.Metrics.FileRecall != 100 || run.Metrics.FilePrecision != 100 {
		t.Fatalf("metrics = %+v", run.Metrics)
	}
}

func TestReplayCheckpointMetricsIgnoreTestArtifacts(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "fake replay completed"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{
		Agent:       fakeReplayAgent,
		TestCommand: "mkdir -p __pycache__ && printf artifact > __pycache__/artifact.pyc",
	})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if got := strings.Join(run.ChangedFiles, ","); got != replayFixtureFile {
		t.Fatalf("ChangedFiles = %q, want only replay output", got)
	}
	if run.Metrics.FileRecall != 100 || run.Metrics.FilePrecision != 100 {
		t.Fatalf("metrics include test artifacts: %+v", run.Metrics)
	}
}

func TestReplayCheckpointSkipsTestsWhenAgentFails(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte("def existing():\n    return 1\n"), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "fake replay failed"}, errors.New("agent failed")
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{
		Agent:       fakeReplayAgent,
		TestCommand: "mkdir -p __pycache__ && printf artifact > __pycache__/artifact.pyc",
	})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if run.Status != replayStatusFailed {
		t.Fatalf("Status = %q, want failed", run.Status)
	}
	if run.Test.Status != replayTestStatusSkipped {
		t.Fatalf("test status = %q, want skipped", run.Test.Status)
	}
	if slices.Contains(run.ChangedFiles, "__pycache__/artifact.pyc") {
		t.Fatalf("ChangedFiles include test artifact: %q", strings.Join(run.ChangedFiles, ","))
	}
	if !slices.Contains(run.Warnings, "test command skipped because replay agent failed") {
		t.Fatalf("warnings = %+v", run.Warnings)
	}
}

func TestReplayEvalRunRanksAndPersistsResults(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "fake replay completed"}, nil
	})
	defer restore()

	eval, err := runReplayEval(context.Background(), replayEvalOptions{
		Checkpoints: []string{cpID},
		Agents:      []string{fakeReplayAgent},
	})
	if err != nil {
		t.Fatalf("runReplayEval() error = %v", err)
	}
	if len(eval.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(eval.Runs))
	}
	if eval.Runs[0].Status != replayStatusPassed {
		t.Fatalf("run status = %q", eval.Runs[0].Status)
	}
	if eval.ResultPath == "" {
		t.Fatal("ResultPath is empty")
	}
	if len(eval.Summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(eval.Summaries))
	}
	if summary := eval.Summaries[0]; summary.Agent != fakeReplayAgent || summary.PassRate != 100 || summary.AvgFileRecall != 100 {
		t.Fatalf("summary = %+v", summary)
	}

	loaded, err := readReplayEval(context.Background(), eval.ID)
	if err != nil {
		t.Fatalf("readReplayEval() error = %v", err)
	}
	if loaded.ID != eval.ID || len(loaded.Runs) != 1 {
		t.Fatalf("loaded eval = %+v", loaded)
	}
}

func TestReplayReportReadsSavedRun(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "fake replay completed"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{Agent: fakeReplayAgent})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	loaded, err := readReplayRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("readReplayRun() error = %v", err)
	}
	if loaded.ID != run.ID || loaded.Spec.CheckpointID != cpID {
		t.Fatalf("loaded run = %+v", loaded)
	}
}

func TestReplayEvalSkipsUnsupportedAgent(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)

	eval, err := runReplayEval(context.Background(), replayEvalOptions{
		Checkpoints: []string{cpID},
		Agents:      []string{"unsupported-agent"},
	})
	if err != nil {
		t.Fatalf("runReplayEval() error = %v", err)
	}
	if len(eval.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(eval.Runs))
	}
	if eval.Runs[0].Status != replayStatusSkipped {
		t.Fatalf("status = %q, want skipped", eval.Runs[0].Status)
	}
	if eval.Runs[0].Test.Status != replayTestStatusSkipped {
		t.Fatalf("test status = %q, want skipped", eval.Runs[0].Test.Status)
	}
}

func TestReplayMetricsFlagsExtraAndRiskyFiles(t *testing.T) {
	metrics := replayMetrics(context.Background(), "", "", ReplaySpec{FilesTouched: []string{replayFixtureFile}}, []string{replayFixtureFile, "auth/config.yaml", "db/schema.sql"})

	if metrics.FileRecall != 100 {
		t.Fatalf("FileRecall = %d", metrics.FileRecall)
	}
	if metrics.FilePrecision != 33 {
		t.Fatalf("FilePrecision = %d", metrics.FilePrecision)
	}
	if got := strings.Join(metrics.ExtraFiles, ","); got != "auth/config.yaml,db/schema.sql" {
		t.Fatalf("ExtraFiles = %q", got)
	}
	if got := strings.Join(metrics.RiskyFiles, ","); got != "auth/config.yaml,db/schema.sql" {
		t.Fatalf("RiskyFiles = %q", got)
	}
	if !metrics.MissingTests {
		t.Fatal("MissingTests = false, want true")
	}
	if metrics.RiskScore == 0 {
		t.Fatal("RiskScore should be non-zero")
	}
}

func TestReplayEvalAgentSummariesRankAgents(t *testing.T) {
	summaries := summarizeReplayEvalAgents([]ReplayRun{
		{
			Agent:      "slow-risky",
			Status:     replayStatusPassed,
			DurationMS: 2000,
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, RiskScore: 3, SemanticAvailable: true, SemanticSimilarity: 50},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
		{
			Agent:      "fast-clean",
			Status:     replayStatusPassed,
			DurationMS: 1000,
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, RiskScore: 0, SemanticAvailable: true, SemanticSimilarity: 80},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1},
		},
		{
			Agent:  "unsupported",
			Status: replayStatusSkipped,
		},
	})

	if len(summaries) != 3 {
		t.Fatalf("summaries = %d, want 3", len(summaries))
	}
	if summaries[0].Agent != "fast-clean" {
		t.Fatalf("top summary = %+v", summaries[0])
	}
	if summaries[0].InputTokens != 5 || summaries[0].OutputTokens != 1 {
		t.Fatalf("token totals = %+v", summaries[0])
	}
	if summaries[2].Agent != "unsupported" || summaries[2].Skipped != 1 {
		t.Fatalf("unsupported summary = %+v", summaries[2])
	}
}

func TestExtractReplayTokenUsage(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"assistant","usage":{"input_tokens":999,"output_tokens":999}}`,
		`{"type":"result","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}}`,
		`{"type":"turn.completed","usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":6}}`,
	}, "\n")
	usage := extractReplayTokenUsage(output)
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 20 || usage.CacheReadTokens != 5 || usage.OutputTokens != 6 || usage.APICallCount != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestCommitReplayResultForSemanticCleanupPreservesWorkingTree(t *testing.T) {
	repoRoot, _, base, _ := newReplayRepo(t)
	worktree, err := createReplayWorktree(context.Background(), repoRoot, base)
	if err != nil {
		t.Fatalf("createReplayWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		if err := removeReplayWorktree(context.Background(), repoRoot, worktree); err != nil {
			t.Errorf("remove replay worktree: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(worktree, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
		t.Fatalf("write replay content: %v", err)
	}

	replayHead, cleanup, err := commitReplayResultForSemantic(context.Background(), worktree)
	if err != nil {
		t.Fatalf("commitReplayResultForSemantic() error = %v", err)
	}
	if replayHead == base {
		t.Fatal("semantic commit did not advance HEAD")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("semantic cleanup: %v", err)
	}
	head := replayGitForTest(t, worktree, "rev-parse", "HEAD")
	if head != base {
		t.Fatalf("HEAD after cleanup = %s, want %s", head, base)
	}
	diff := replayGitForTest(t, worktree, "diff", "--", replayFixtureFile)
	if !strings.Contains(diff, "replay_helper") {
		t.Fatalf("working tree diff lost replay changes:\n%s", diff)
	}
}

func TestReplayJSONIsStable(t *testing.T) {
	run := ReplayRun{
		ID:     "abc123def456",
		Status: replayStatusPassed,
		Spec: ReplaySpec{
			CheckpointID: "a1b2c3d4e5f6",
			Prompt:       "Do work",
			BaseCommit:   "base",
			TargetCommit: "target",
		},
		Metrics: ReplayMetrics{FileRecall: 100, FilePrecision: 100},
	}
	var out bytes.Buffer
	if err := writeReplayJSON(&out, run); err != nil {
		t.Fatalf("writeReplayJSON() error = %v", err)
	}
	var decoded ReplayRun
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if decoded.ID != run.ID || decoded.Spec.CheckpointID != run.Spec.CheckpointID {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestReplayAgentEnvDisablesGitHooks(t *testing.T) {
	env := replayAgentEnv([]string{
		"PATH=/usr/bin",
		"GIT_DIR=/tmp/git",
		"GIT_CONFIG_COUNT=99",
		"GIT_CONFIG_KEY_0=user.name",
		"GIT_CONFIG_VALUE_0=Bad",
	})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, absent := range []string{"\nGIT_DIR=", "\nGIT_CONFIG_COUNT=99", "\nGIT_CONFIG_KEY_0=user.name", "\nGIT_CONFIG_VALUE_0=Bad"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("env still contains %q:\n%s", absent, joined)
		}
	}
	for _, present := range []string{"ENTIRE_REPLAY=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null"} {
		if !strings.Contains(joined, "\n"+present+"\n") {
			t.Fatalf("env missing %q:\n%s", present, joined)
		}
	}
}

func TestRootCommandHasReplayAndEval(t *testing.T) {
	root := NewRootCmd()
	replayCmd, _, err := root.Find([]string{"replay", "checkpoint"})
	if err != nil {
		t.Fatalf("find replay checkpoint: %v", err)
	}
	if replayCmd.Name() != "checkpoint" {
		t.Fatalf("replay command = %q", replayCmd.Name())
	}
	reportCmd, _, err := root.Find([]string{"replay", "report"})
	if err != nil {
		t.Fatalf("find replay report: %v", err)
	}
	if reportCmd.Name() != "report" {
		t.Fatalf("replay report command = %q", reportCmd.Name())
	}
	evalCmd, _, err := root.Find([]string{"eval", "run"})
	if err != nil {
		t.Fatalf("find eval run: %v", err)
	}
	if evalCmd.Name() != "run" {
		t.Fatalf("eval command = %q", evalCmd.Name())
	}
}

func newReplayRepo(t *testing.T) (repoRoot, cpID, base, target string) {
	t.Helper()
	return newReplayRepoWithPrompts(t, []string{"Add the replay helper."}, []byte(`{"type":"user","uuid":"u1","message":{"content":"Add the replay helper."}}
`))
}

func newReplayRepoWithPrompts(t *testing.T, prompts []string, transcript []byte) (repoRoot, cpID, base, target string) {
	t.Helper()
	return newReplayRepoWithOptions(t, replayRepoOptions{
		Prompts:      prompts,
		Transcript:   transcript,
		FilesTouched: []string{replayFixtureFile},
	})
}

type replayRepoOptions struct {
	Prompts      []string
	Transcript   []byte
	FilesTouched []string
}

func newReplayRepoWithOptions(t *testing.T, opts replayRepoOptions) (repoRoot, cpID, base, target string) {
	t.Helper()
	repoRoot = t.TempDir()
	testutil.InitRepo(t, repoRoot)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(session.ClearGitCommonDirCache)

	testutil.WriteFile(t, repoRoot, ".gitignore", "__pycache__/\n")
	testutil.WriteFile(t, repoRoot, replayFixtureFile, "def greet():\n    return 'hello'\n")
	testutil.GitAdd(t, repoRoot, ".gitignore", replayFixtureFile)
	testutil.GitCommit(t, repoRoot, "initial app")
	base = replayGitForTest(t, repoRoot, "rev-parse", "HEAD")

	cpID = "a1b2c3d4e5f6"
	testutil.WriteFile(t, repoRoot, replayFixtureFile, "def greet():\n    return 'hello'\n\n\ndef replay_helper():\n    return 'ok'\n")
	testutil.GitAdd(t, repoRoot, replayFixtureFile)
	testutil.GitCommit(t, repoRoot, trailers.FormatCheckpoint("add replay helper", checkpointid.MustCheckpointID(cpID)))
	target = replayGitForTest(t, repoRoot, "rev-parse", "HEAD")

	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	if err := checkpoint.NewGitStore(repo).WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID:     checkpointid.MustCheckpointID(cpID),
		SessionID:        "session-replay-12345678",
		Strategy:         "manual-commit",
		Branch:           "master",
		Transcript:       redact.AlreadyRedacted(opts.Transcript),
		Prompts:          opts.Prompts,
		FilesTouched:     opts.FilesTouched,
		CheckpointsCount: 1,
		Agent:            agentpkg.AgentTypeClaudeCode,
		Model:            "claude-test-model",
	}); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	return repoRoot, cpID, base, target
}

func replayGitForTest(t *testing.T, repoRoot string, args ...string) string {
	t.Helper()
	out, err := replayGit(context.Background(), repoRoot, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func stubReplayRunner(fn func(context.Context, ReplayRunnerRequest) (ReplayRunnerResult, error)) func() {
	previous := replayRunnerFor
	replayRunnerFor = func(agentName string) *replayRunnerFunc {
		if agentName == fakeReplayAgent {
			return &replayRunnerFunc{name: fakeReplayAgent, fn: fn}
		}
		return nil
	}
	return func() { replayRunnerFor = previous }
}
