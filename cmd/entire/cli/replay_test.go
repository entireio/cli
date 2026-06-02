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
	"time"

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
	if run.SchemaVersion != replaySchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", run.SchemaVersion, replaySchemaVersion)
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

func TestReplayCheckpointTruncatesLargeDiff(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(_ context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		largeContent := "def greet():\n    return 'hello'\n\n" + strings.Repeat("# replay filler line\n", 40000)
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(largeContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		return ReplayRunnerResult{Output: "large replay completed"}, nil
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{Agent: fakeReplayAgent})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if !run.DiffTruncated {
		t.Fatal("DiffTruncated = false, want true")
	}
	if len(run.Diff) > replayResultDiffLimit+len("\n...[diff truncated]") {
		t.Fatalf("diff length = %d, want capped", len(run.Diff))
	}
	if !strings.Contains(run.Diff, "...[diff truncated]") {
		t.Fatalf("diff missing truncation marker")
	}

	loaded, err := readReplayRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("readReplayRun() error = %v", err)
	}
	if !loaded.DiffTruncated {
		t.Fatal("loaded DiffTruncated = false, want true")
	}
}

func TestReplayCheckpointCapturesDiffAfterAgentTimeout(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restore := stubReplayRunner(func(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, replayFixtureFile), []byte(replayTargetContent), 0o644); err != nil {
			return ReplayRunnerResult{}, err
		}
		<-ctx.Done()
		return ReplayRunnerResult{Output: "agent timed out after writing files"}, ctx.Err()
	})
	defer restore()

	run, err := runReplayCheckpoint(context.Background(), cpID, replayCheckpointOptions{
		Agent:   fakeReplayAgent,
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runReplayCheckpoint() error = %v", err)
	}
	if run.Status != replayStatusFailed {
		t.Fatalf("Status = %q, want failed", run.Status)
	}
	if got := strings.Join(run.ChangedFiles, ","); got != replayFixtureFile {
		t.Fatalf("ChangedFiles = %q, want replay output after timeout", got)
	}
	if !strings.Contains(run.Diff, "replay_helper") {
		t.Fatalf("Diff missing timed-out replay changes:\n%s", run.Diff)
	}
	if run.Metrics.FileRecall != 100 || run.Metrics.FilePrecision != 100 {
		t.Fatalf("metrics = %+v", run.Metrics)
	}
	if len(run.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want no diff-inspection warning", run.Warnings)
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
	if eval.SchemaVersion != replaySchemaVersion || eval.Runs[0].SchemaVersion != replaySchemaVersion {
		t.Fatalf("schema versions = eval %d run %d, want %d", eval.SchemaVersion, eval.Runs[0].SchemaVersion, replaySchemaVersion)
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
	if loaded.ID != eval.ID || len(loaded.Runs) != 1 || loaded.SchemaVersion != replaySchemaVersion || loaded.Runs[0].SchemaVersion != replaySchemaVersion {
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
	if loaded.ID != run.ID || loaded.Spec.CheckpointID != cpID || loaded.SchemaVersion != replaySchemaVersion {
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

func TestReplayCheckpointMissingAgentCommandFailsEarly(t *testing.T) {
	restoreRunner := stubReplayRunner(func(_ context.Context, _ ReplayRunnerRequest) (ReplayRunnerResult, error) {
		t.Fatal("runner should not execute when command is missing")
		return ReplayRunnerResult{}, nil
	})
	defer restoreRunner()
	restoreCommand := replayCommandForAgent
	replayCommandForAgent = func(string) string {
		return filepath.Join(t.TempDir(), "missing-agent-command")
	}
	defer func() { replayCommandForAgent = restoreCommand }()

	_, err := runReplayCheckpoint(context.Background(), "does-not-need-a-real-checkpoint", replayCheckpointOptions{Agent: fakeReplayAgent})
	if err == nil {
		t.Fatal("runReplayCheckpoint() error = nil, want missing command error")
	}
	if !strings.Contains(err.Error(), "requires") || !strings.Contains(err.Error(), "missing-agent-command") {
		t.Fatalf("error = %v", err)
	}
}

func TestReplayEvalSkipsMissingAgentCommand(t *testing.T) {
	_, cpID, _, _ := newReplayRepo(t)
	restoreRunner := stubReplayRunner(func(_ context.Context, _ ReplayRunnerRequest) (ReplayRunnerResult, error) {
		t.Fatal("runner should not execute when command is missing")
		return ReplayRunnerResult{}, nil
	})
	defer restoreRunner()
	restoreCommand := replayCommandForAgent
	replayCommandForAgent = func(string) string {
		return filepath.Join(t.TempDir(), "missing-agent-command")
	}
	defer func() { replayCommandForAgent = restoreCommand }()

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
	run := eval.Runs[0]
	if run.Status != replayStatusSkipped || run.Test.Status != replayTestStatusSkipped {
		t.Fatalf("run = %+v, want skipped run and skipped test", run)
	}
	if !strings.Contains(run.Error, "requires") || !strings.Contains(run.Error, "missing-agent-command") {
		t.Fatalf("error = %q", run.Error)
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

func TestReplayMetricsBroadSourceFilesNeedTests(t *testing.T) {
	for _, file := range []string{
		"cmd/main.go",
		"src/App.tsx",
		"src/Auth.java",
		"Sources/AuthService.swift",
		"lib/token.rb",
		"src/parser.rs",
		"database/schema.sql",
		"proto/service.proto",
		"infra/main.tf",
		"scripts/deploy.sh",
		"src/lib.cpp",
		"src/claims.cs",
		"lib/module.ex",
		"src/query.scala",
		"src/plugin.php",
	} {
		if !sourceChangedWithoutTests([]string{file}) {
			t.Fatalf("sourceChangedWithoutTests(%q) = false, want true", file)
		}
	}
	if sourceChangedWithoutTests([]string{"src/Auth.java", "src/AuthTest.java"}) {
		t.Fatal("sourceChangedWithoutTests() = true when test file changed too")
	}
	if !sourceChangedWithoutTests([]string{"src/contest.go"}) {
		t.Fatal("sourceChangedWithoutTests() = false for non-test source file containing test")
	}
	if !sourceChangedWithoutTests([]string{"src/specimen.py"}) {
		t.Fatal("sourceChangedWithoutTests() = false for non-test source file containing spec")
	}
}

func TestReplayTestFileDetectionUsesConventions(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/auth_test.go", true},
		{"src/test_auth.py", true},
		{"src/auth.test.ts", true},
		{"src/auth.spec.tsx", true},
		{"src/AuthTest.java", true},
		{"src/AuthSpec.swift", true},
		{"src/__tests__/auth.ts", true},
		{"tests/auth.rs", true},
		{"src/contest.go", false},
		{"src/specimen.py", false},
		{"src/latest.ts", false},
		{"src/testimony.rb", false},
	}

	for _, tt := range tests {
		if got := isReplayTestFile(tt.path); got != tt.want {
			t.Fatalf("isReplayTestFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestReplayRiskFlagsInfrastructureAndSecurityFiles(t *testing.T) {
	files := []string{
		".github/workflows/deploy.yml",
		".env",
		"infra/main.tf",
		"security/policy.yaml",
		"docs/readme.md",
	}
	got := strings.Join(riskyReplayFiles(files), ",")
	want := ".env,.github/workflows/deploy.yml,infra/main.tf,security/policy.yaml"
	if got != want {
		t.Fatalf("riskyReplayFiles() = %q, want %q", got, want)
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

func TestReplayEvalAgentSummariesUseTokenTieBreaker(t *testing.T) {
	summaries := summarizeReplayEvalAgents([]ReplayRun{
		{
			Agent:      "expensive",
			Status:     replayStatusPassed,
			DurationMS: 1000,
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, SemanticAvailable: true, SemanticSimilarity: 90},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 100, OutputTokens: 20},
		},
		{
			Agent:      "cheap",
			Status:     replayStatusPassed,
			DurationMS: 1000,
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, SemanticAvailable: true, SemanticSimilarity: 90},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 10, OutputTokens: 2},
		},
	})

	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2", len(summaries))
	}
	if summaries[0].Agent != "cheap" {
		t.Fatalf("top summary = %+v, want cheap token tie-breaker", summaries[0])
	}
}

func TestSortReplayRunsUsesSemanticAndTokenTieBreakers(t *testing.T) {
	runs := []ReplayRun{
		{
			ID:         "expensive",
			Agent:      "expensive",
			Status:     replayStatusPassed,
			Test:       ReplayTestRun{Status: replayStatusPassed},
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, SemanticAvailable: true, SemanticSimilarity: 95},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 100, OutputTokens: 10},
			DurationMS: 1000,
		},
		{
			ID:         "better-semantic",
			Agent:      "better-semantic",
			Status:     replayStatusPassed,
			Test:       ReplayTestRun{Status: replayStatusPassed},
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, SemanticAvailable: true, SemanticSimilarity: 99},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 1000, OutputTokens: 100},
			DurationMS: 2000,
		},
		{
			ID:         "cheap",
			Agent:      "cheap",
			Status:     replayStatusPassed,
			Test:       ReplayTestRun{Status: replayStatusPassed},
			Metrics:    ReplayMetrics{FileRecall: 100, FilePrecision: 100, SemanticAvailable: true, SemanticSimilarity: 95},
			TokenUsage: &agentpkg.TokenUsage{InputTokens: 10, OutputTokens: 1},
			DurationMS: 1000,
		},
	}

	sortReplayRuns(runs)

	if runs[0].ID != "better-semantic" {
		t.Fatalf("first run = %+v, want better semantic match first", runs[0])
	}
	if runs[1].ID != "cheap" {
		t.Fatalf("second run = %+v, want cheaper token tie-breaker", runs[1])
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
