package agentcheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
)

var testCheckpointID = id.MustCheckpointID("abc123def456")

type fakeReader struct {
	summary  *checkpoint.CheckpointSummary
	sessions []*checkpoint.SessionContent
	readErr  error
}

func (f fakeReader) Read(context.Context, id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	return f.summary, f.readErr
}

func (f fakeReader) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	return nil, nil
}

func (f fakeReader) ReadSessionContent(_ context.Context, _ id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error) {
	if sessionIndex < 0 || sessionIndex >= len(f.sessions) {
		return nil, checkpoint.ErrCheckpointNotFound
	}
	return f.sessions[sessionIndex], nil
}

func (f fakeReader) ReadSessionMetadata(_ context.Context, cpID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, error) {
	content, err := f.ReadSessionContent(context.Background(), cpID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return &content.Metadata, nil
}

func (f fakeReader) ReadSessionPrompts(_ context.Context, cpID id.CheckpointID, sessionIndex int) (string, error) {
	content, err := f.ReadSessionContent(context.Background(), cpID, sessionIndex)
	if err != nil {
		return "", err
	}
	return content.Prompts, nil
}

func (f fakeReader) ReadSessionMetadataAndPrompts(_ context.Context, cpID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, string, error) {
	content, err := f.ReadSessionContent(context.Background(), cpID, sessionIndex)
	if err != nil {
		return nil, "", err
	}
	return &content.Metadata, content.Prompts, nil
}

type fakeGraph struct {
	ctx GraphContext
	err error
}

func (f fakeGraph) CollectGraphEvidence(context.Context, GraphRequest) (GraphContext, error) {
	return f.ctx, f.err
}

func TestBuildValidCheckpointProducesContext(t *testing.T) {
	repoRoot, repo := repoWithCheckpointCommit(t, testCheckpointID)
	ctx, err := Builder{
		Reader:   testReader(),
		Repo:     repo,
		RepoRoot: repoRoot,
		Graph:    fakeGraph{ctx: GraphContext{Available: true, Evidence: []GraphEvidence{{Kind: "definition", Paths: []string{"feature.go"}}}}},
	}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if ctx.CheckpointID != testCheckpointID {
		t.Fatalf("CheckpointID = %s, want %s", ctx.CheckpointID, testCheckpointID)
	}
	if len(ctx.Sessions) != 2 {
		t.Fatalf("len(Sessions) = %d, want 2", len(ctx.Sessions))
	}
	if !ctx.Graph.Available {
		t.Fatal("Graph.Available = false, want true")
	}
}

func TestBuildPreservesSessionInformation(t *testing.T) {
	ctx, err := Builder{Reader: testReader()}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	first := ctx.Sessions[0]
	if first.SessionID != "session-1" || first.AgentType != "Codex" || first.Model != "gpt-test" {
		t.Fatalf("session fields = %+v", first)
	}
	if first.TokenUsage == nil || first.TokenUsage.InputTokens != 11 {
		t.Fatalf("TokenUsage = %+v, want input 11", first.TokenUsage)
	}
}

func TestBuildPreservesOriginalDeveloperPrompt(t *testing.T) {
	ctx, err := Builder{Reader: testReader()}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := "Implement Google OAuth login.\nDo NOT modify the database schema."
	if ctx.DeveloperPrompt != want {
		t.Fatalf("DeveloperPrompt = %q, want %q", ctx.DeveloperPrompt, want)
	}
	if len(ctx.ScopedPrompts) != 3 {
		t.Fatalf("len(ScopedPrompts) = %d, want 3", len(ctx.ScopedPrompts))
	}
}

func TestBuildPreservesChangedFiles(t *testing.T) {
	ctx, err := Builder{Reader: testReader()}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := strings.Join(ctx.FilesTouched, ","); got != "feature.go,feature_test.go" {
		t.Fatalf("FilesTouched = %q", got)
	}
}

func TestBuildAssociatesGitEvidenceWithCheckpoint(t *testing.T) {
	repoRoot, repo := repoWithCheckpointCommit(t, testCheckpointID)
	ctx, err := Builder{Reader: testReader(), Repo: repo, RepoRoot: repoRoot}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(ctx.Git.AssociatedCommits) != 1 {
		t.Fatalf("AssociatedCommits = %+v", ctx.Git.AssociatedCommits)
	}
	if ctx.Git.AssociatedCommits[0].Source != "commit_trailer" {
		t.Fatalf("Source = %q", ctx.Git.AssociatedCommits[0].Source)
	}
	if !strings.Contains(ctx.Git.Diff, "feature.go") {
		t.Fatalf("GitDiff missing feature.go:\n%s", ctx.Git.Diff)
	}
	if len(ctx.ChangedFiles) != 1 || ctx.ChangedFiles[0].Path != "feature.go" {
		t.Fatalf("ChangedFiles = %+v", ctx.ChangedFiles)
	}
}

func TestBuildGraphUnavailableDoesNotBreakContext(t *testing.T) {
	ctx, err := Builder{Reader: testReader()}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if ctx.Graph.Available {
		t.Fatal("Graph.Available = true, want false")
	}
	if ctx.Graph.UnavailableReason == "" {
		t.Fatal("Graph.UnavailableReason is empty")
	}
}

func TestBuildMissingCheckpointReturnsClearError(t *testing.T) {
	_, err := Builder{Reader: fakeReader{summary: nil}}.Build(context.Background(), testCheckpointID)
	if err == nil {
		t.Fatal("Build() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "read checkpoint") {
		t.Fatalf("error = %v, want read checkpoint context", err)
	}
}

func TestBuildDoesNotFabricateUnavailableData(t *testing.T) {
	reader := testReader()
	reader.sessions[0].Transcript = nil
	reader.sessions[1].Transcript = nil
	ctx, err := Builder{Reader: reader}.Build(context.Background(), testCheckpointID)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if ctx.Transcript.Available {
		t.Fatal("Transcript.Available = true, want false")
	}
	if len(ctx.Git.AssociatedCommits) != 0 || ctx.Git.Diff != "" {
		t.Fatalf("git evidence fabricated: %+v", ctx.Git)
	}
	if len(ctx.TaskRecords) != 0 {
		t.Fatalf("TaskRecords = %+v, want none", ctx.TaskRecords)
	}
}

func TestBuildFromRepositoryUsesCheckpointFacade(t *testing.T) {
	repoRoot, repo := repoWithCheckpointCommit(t, testCheckpointID)
	repoCtx := settings.WithWorktreeRoot(context.Background(), repoRoot)
	stores, err := checkpoint.Open(repoCtx, repo, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = stores.Persistent.Write(repoCtx, checkpoint.Session(checkpoint.WriteOptions{
		CheckpointID:     testCheckpointID,
		SessionID:        "session-real",
		CreatedAt:        time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		Strategy:         "manual-commit",
		Branch:           "main",
		CheckpointsCount: 1,
		FilesTouched:     []string{"feature.go"},
		Transcript:       redact.AlreadyRedacted([]byte(`{"type":"message"}` + "\n")),
		Prompts:          []string{"Preserve this exact prompt."},
		Agent:            types.AgentType("Codex"),
		Model:            "gpt-test",
	}))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	ctx, err := BuildFromRepository(context.Background(), testCheckpointID, RepositoryBuildOptions{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("BuildFromRepository() error = %v", err)
	}
	if ctx.DeveloperPrompt != "Preserve this exact prompt." {
		t.Fatalf("DeveloperPrompt = %q", ctx.DeveloperPrompt)
	}
	if len(ctx.Sessions) != 1 || ctx.Sessions[0].SessionID != "session-real" {
		t.Fatalf("Sessions = %+v", ctx.Sessions)
	}
	runtime.GC()
	debug.FreeOSMemory()
}

func testReader() fakeReader {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	return fakeReader{
		summary: &checkpoint.CheckpointSummary{
			CheckpointID:     testCheckpointID,
			Strategy:         "manual-commit",
			Branch:           "main",
			CheckpointsCount: 2,
			FilesTouched:     []string{"feature_test.go", "feature.go"},
			Sessions:         []checkpoint.SessionFilePaths{{}, {}},
			TokenUsage:       &types.TokenUsage{InputTokens: 21, OutputTokens: 13},
		},
		sessions: []*checkpoint.SessionContent{
			{
				Metadata: checkpoint.Metadata{
					CheckpointID:     testCheckpointID,
					SessionID:        "session-1",
					CreatedAt:        now,
					Strategy:         "manual-commit",
					Branch:           "main",
					CheckpointsCount: 1,
					SaveStepCount:    1,
					FilesTouched:     []string{"feature.go"},
					Agent:            types.AgentType("Codex"),
					Model:            "gpt-test",
					TokenUsage:       &types.TokenUsage{InputTokens: 11, OutputTokens: 7},
				},
				Transcript: []byte(`{"type":"response_item"}`),
				Prompts:    "Implement Google OAuth login.\nDo NOT modify the database schema." + checkpoint.PromptSeparator + "Keep it minimal.",
			},
			{
				Metadata: checkpoint.Metadata{
					CheckpointID: testCheckpointID,
					SessionID:    "session-2",
					CreatedAt:    now.Add(time.Minute),
					Strategy:     "manual-commit",
					FilesTouched: []string{"feature_test.go"},
					Agent:        types.AgentType("Codex"),
					Model:        "gpt-test",
				},
				Transcript: []byte(`{"type":"response_item"}`),
				Prompts:    "Add focused tests.",
			},
		},
	}
}

func repoWithCheckpointCommit(t *testing.T, cpID id.CheckpointID) (string, *git.Repository) {
	t.Helper()
	dir, err := os.MkdirTemp("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "feature.go")
	runGit(t, dir, "commit", "-m", "add feature\n\nEntire-Checkpoint: "+cpID.String())

	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath() error = %v", err)
	}
	t.Cleanup(func() {
		_ = repo.Close()
		runtime.GC()
		debug.FreeOSMemory()
		_ = os.RemoveAll(dir)
	})
	return dir, repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}
