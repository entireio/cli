package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestPrepareLocal_RejectsServerMode(t *testing.T) {
	t.Parallel()

	_, err := PrepareLocal(context.Background(), Options{Mode: ModeServer})
	if err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("expected local-mode error, got %v", err)
	}
}

func TestPrepareLocal_RejectsInvalidSince(t *testing.T) {
	t.Parallel()

	_, err := PrepareLocal(context.Background(), Options{
		Mode:  ModeLocal,
		Since: "definitely-not-a-time",
	})
	if err == nil || !strings.Contains(err.Error(), "unparseable time") {
		t.Fatalf("expected invalid --since error, got %v", err)
	}
}

func TestPrepareLocal_RejectsInvalidUntil(t *testing.T) {
	t.Parallel()

	_, err := PrepareLocal(context.Background(), Options{
		Mode:  ModeLocal,
		Since: "2026-07-16T12:00:00Z",
		Until: "definitely-not-a-time",
	})
	if err == nil || !strings.Contains(err.Error(), "unparseable time") {
		t.Fatalf("expected invalid --until error, got %v", err)
	}
}

func TestPrepareLocal_RejectsReversedNormalizedWindow(t *testing.T) {
	t.Parallel()

	_, err := PrepareLocal(context.Background(), Options{
		Mode:  ModeLocal,
		Since: "2026-07-17T12:01:00Z",
		Until: "2026-07-17T12:00:00Z",
	})
	if err == nil || err.Error() != "--since must be before --until" {
		t.Fatalf("expected reversed-window error, got %v", err)
	}
}

func TestPrepareLocal_RejectsEqualNormalizedWindow(t *testing.T) {
	t.Parallel()

	_, err := PrepareLocal(context.Background(), Options{
		Mode:  ModeLocal,
		Since: "2026-07-17T12:00:00Z",
		Until: "2026-07-17T12:00:00Z",
	})
	if err == nil || err.Error() != "--since must be before --until" {
		t.Fatalf("expected equal-window error, got %v", err)
	}
}

func TestPrepareLocal_ValidWindowOutsideGitFailsRepoResolution(t *testing.T) {
	t.Chdir(t.TempDir())

	_, err := PrepareLocal(context.Background(), Options{
		Mode:  ModeLocal,
		Since: "2026-07-16T12:00:00Z",
		Until: "2026-07-17T12:00:00Z",
	})
	if err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Fatalf("expected repository-root error, got %v", err)
	}
}

func TestPrepareLocal_RunAutoPreparesDirectCall(t *testing.T) {
	t.Chdir(t.TempDir())

	_, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "2026-07-16T12:00:00Z",
		Until:         "2026-07-17T12:00:00Z",
		AllBranches:   true,
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Fatalf("expected direct Run to perform repository preflight, got %v", err)
	}
}

func TestRunLocal_UsesPreparedWindowAndRepoRoots(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "a.txt", "x")
	testutil.GitAdd(t, repoDir, "a.txt")
	testutil.GitCommit(t, repoDir, "initial")
	addOriginRemote(t, repoDir)

	preparedAt := time.Date(2026, 7, 17, 12, 34, 45, 0, time.UTC)
	oldNow := nowUTC
	nowUTC = func() time.Time { return preparedAt }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(repoDir)
	generator := stubGeneratedLocalDispatch()
	prepared, err := PrepareLocal(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "1h",
		AllBranches:   true,
		TextGenerator: generator,
		Model:         "prepared-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TextGenerator != generator || prepared.Model != "prepared-model" {
		t.Fatal("preflight must preserve injected generation options")
	}

	nowUTC = func() time.Time { return preparedAt.Add(24 * time.Hour) }
	t.Chdir(t.TempDir())
	got, err := Run(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}

	wantSince := time.Date(2026, 7, 17, 11, 34, 0, 0, time.UTC)
	wantUntil := time.Date(2026, 7, 17, 12, 35, 0, 0, time.UTC)
	if !got.Window.NormalizedSince.Equal(wantSince) || !got.Window.NormalizedUntil.Equal(wantUntil) {
		t.Fatalf("prepared window was recomputed: got [%s, %s), want [%s, %s)",
			got.Window.NormalizedSince, got.Window.NormalizedUntil, wantSince, wantUntil)
	}
	if got.GeneratedText != "generated dispatch" {
		t.Fatalf("unexpected generated text: %q", got.GeneratedText)
	}
}

func TestRunLocal_RepreparesWhenPreflightInputsChange(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "a.txt", "x")
	testutil.GitAdd(t, repoDir, "a.txt")
	testutil.GitCommit(t, repoDir, "initial")
	addOriginRemote(t, repoDir)
	t.Chdir(repoDir)

	tests := []struct {
		name      string
		mutate    func(*Options)
		wantError string
	}{
		{
			name: "since",
			mutate: func(opts *Options) {
				opts.Since = "definitely-not-a-time"
			},
			wantError: "unparseable time",
		},
		{
			name: "until",
			mutate: func(opts *Options) {
				opts.Until = "definitely-not-a-time"
			},
			wantError: "unparseable time",
		},
		{
			name: "repo paths",
			mutate: func(opts *Options) {
				opts.RepoPaths = []string{filepath.Join(t.TempDir(), "missing")}
			},
			wantError: "resolve repo root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := PrepareLocal(context.Background(), Options{
				Mode:          ModeLocal,
				Since:         "7d",
				AllBranches:   true,
				TextGenerator: stubGeneratedLocalDispatch(),
			})
			if err != nil {
				t.Fatal(err)
			}

			tt.mutate(&prepared)
			_, err = Run(context.Background(), prepared)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Run() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestLocalMode_EnumeratesCheckpoints(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("expected 1 repo group, got %d", len(got.Repos))
	}
	if got.Repos[0].FullName != testRepoFullName {
		t.Fatalf("unexpected repo group: %+v", got.Repos[0])
	}
	if got.Repos[0].URL != testRepoURL {
		t.Fatalf("unexpected repo URL: %q", got.Repos[0].URL)
	}
	if got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("unexpected bullet: %+v", got.Repos[0].Sections[0].Bullets[0])
	}
	if len(got.CoveredRepos) != 1 || got.CoveredRepos[0] != testRepoFullName {
		t.Fatalf("unexpected covered repos: %v", got.CoveredRepos)
	}
}

func TestLocalMode_ExplicitRepoUsesTargetRepoCheckpointSettings(t *testing.T) {
	cwdDir := t.TempDir()
	targetDir := t.TempDir()

	testutil.InitRepo(t, cwdDir)
	if err := os.MkdirAll(filepath.Join(cwdDir, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cwdDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	testutil.InitRepo(t, targetDir)
	testutil.WriteFile(t, targetDir, "a.txt", "x")
	testutil.GitAdd(t, targetDir, "a.txt")
	testutil.GitCommit(t, targetDir, "initial")
	addOriginRemote(t, targetDir)

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, targetDir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(cwdDir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		RepoPaths:     []string{targetDir},
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("expected target repo checkpoint, got %+v", got.Repos)
	}
	if got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("unexpected bullet: %+v", got.Repos[0].Sections[0].Bullets[0])
	}
}

func TestLocalMode_ExplicitRepoUsesTargetRepoCheckpointRemotes(t *testing.T) {
	cwdDir := t.TempDir()
	targetDir := t.TempDir()

	testutil.InitRepo(t, cwdDir)
	addOriginRemote(t, cwdDir)

	testutil.InitRepo(t, targetDir)
	testutil.WriteFile(t, targetDir, "a.txt", "x")
	testutil.GitAdd(t, targetDir, "a.txt")
	testutil.GitCommit(t, targetDir, "initial")
	addOriginRemote(t, targetDir)
	testutil.AddRemote(t, targetDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, targetDir, "upstream")

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, targetDir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	localRef := "refs/heads/" + paths.MetadataBranchName
	cmd := exec.CommandContext(t.Context(), "git", "-C", targetDir, "rev-parse", localRef)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	testutil.GitUpdateRef(t, targetDir, "refs/remotes/upstream/"+paths.MetadataBranchName, strings.TrimSpace(string(out)))
	cmd = exec.CommandContext(t.Context(), "git", "-C", targetDir, "update-ref", "-d", localRef)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("delete local checkpoint ref: %v\n%s", err, out)
	}

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(cwdDir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		RepoPaths:     []string{targetDir},
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("expected checkpoint from target repo's upstream tracking ref, got %+v", got.Repos)
	}
}

func TestLocalMode_UsesUntilWindow(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	now := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    now,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return now }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Until:         now.Add(-time.Hour).Format(time.RFC3339),
		Branches:      []string{"main"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 0 {
		t.Fatalf("expected no repo groups, got %d", len(got.Repos))
	}
}

func TestLocalMode_FallsBackToCommitSubjectWhenSummaryMissing(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	now := time.Now().UTC()
	cpID := testCheckpointID
	testutil.WriteFile(t, dir, "plans.md", "ship it")
	testutil.GitAdd(t, dir, "plans.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("ship the thing", mustCheckpointID(t, cpID)))
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           cpID,
		branch:       "main",
		createdAt:    now,
		filesTouched: []string{"plans.md"},
		outcome:      "",
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return now }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("expected one repo group, got %+v", got.Repos)
	}
	if got.Repos[0].Sections[0].Bullets[0].Text != "ship the thing" {
		t.Fatalf("unexpected bullet: %+v", got.Repos[0].Sections[0].Bullets[0])
	}
}

func TestLocalMode_GenerateProducesInlineText(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	createdAt := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	mock := &stubTextGenerator{text: "generated inline dispatch"}
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: mock,
		Model:         "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedText != "generated inline dispatch" {
		t.Fatalf("expected generated text, got %q", got.GeneratedText)
	}
	if mock.model != "test-model" {
		t.Fatalf("unexpected model: %q", mock.model)
	}
}

func TestLocalMode_FailsWhenGeneratedMarkdownIsEmpty(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	createdAt := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "main",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	_, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Branches:      []string{"main"},
		TextGenerator: &stubTextGenerator{text: "  \n\t "},
	})
	if err == nil {
		t.Fatal("expected error when local generation returns empty markdown")
	}
	if err.Error() != "dispatch generation returned no markdown" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLocalMode_ImplicitCurrentBranchUsesHEADReachability(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	cpID := testCheckpointID
	testutil.GitCheckoutNewBranch(t, dir, "entire-dispatch")
	testutil.WriteFile(t, dir, "plans.md", "dispatch plan")
	testutil.GitAdd(t, dir, "plans.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("plan commit", mustCheckpointID(t, cpID)))

	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	parsedID, err := checkpointid.NewCheckpointID(cpID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Write(context.Background(), checkpoint.Session{
		CheckpointID:     parsedID,
		SessionID:        "session-1",
		Strategy:         "manual-commit",
		Branch:           "entire-dispatch",
		Transcript:       redact.AlreadyRedacted([]byte("{\"type\":\"user\"}\n")),
		Prompts:          []string{"summarize recent work"},
		FilesTouched:     []string{"plans.md"},
		CheckpointsCount: 1,
		Agent:            agent.AgentTypeClaudeCode,
		Summary: &checkpoint.Summary{
			Outcome: testLocalFallbackText,
		},
		AuthorName:  "Test User",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	testutil.GitCheckoutNewBranch(t, dir, "entire-dispatch-codex")

	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Now().UTC() }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{"entire-dispatch-codex"},
		ImplicitCurrentBranch: true,
		TextGenerator:         stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("unexpected dispatch payload: %+v", got)
	}
}

func TestLocalMode_ExplicitBranchesRemainExact(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	cpID := testCheckpointID
	testutil.GitCheckoutNewBranch(t, dir, "entire-dispatch")
	testutil.WriteFile(t, dir, "plans.md", "dispatch plan")
	testutil.GitAdd(t, dir, "plans.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("plan commit", mustCheckpointID(t, cpID)))
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	parsedID, err := checkpointid.NewCheckpointID(cpID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Write(context.Background(), checkpoint.Session{
		CheckpointID:     parsedID,
		SessionID:        "session-1",
		Strategy:         "manual-commit",
		Branch:           "entire-dispatch",
		Transcript:       redact.AlreadyRedacted([]byte("{\"type\":\"user\"}\n")),
		Prompts:          []string{"summarize recent work"},
		FilesTouched:     []string{"plans.md"},
		CheckpointsCount: 1,
		Agent:            agent.AgentTypeClaudeCode,
		Summary: &checkpoint.Summary{
			Outcome: testLocalFallbackText,
		},
		AuthorName:  "Test User",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	testutil.GitCheckoutNewBranch(t, dir, "entire-dispatch-codex")

	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Now().UTC() }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		Branches:      []string{"entire-dispatch-codex"},
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 0 {
		t.Fatalf("expected 0 repo groups with explicit branch filter, got %d", len(got.Repos))
	}
}

func TestLocalMode_ImplicitCurrentBranchUsesCheckpointBranchWithoutTrailerReachability(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	testutil.GitCheckoutNewBranch(t, dir, "entire-dispatch-codex")

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "entire-dispatch-codex",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	t.Cleanup(func() {
		nowUTC = oldNow
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{"entire-dispatch-codex"},
		ImplicitCurrentBranch: true,
		TextGenerator:         stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("unexpected dispatch payload: %+v", got)
	}
}

func TestLocalMode_ImplicitCurrentBranchExcludesDefaultBranchHistory(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir)

	mainCheckpointID := "00aaaaaaaaaa"
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("main work", mustCheckpointID(t, mainCheckpointID)))

	mainCreatedAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           mainCheckpointID,
		branch:       "master",
		createdAt:    mainCreatedAt,
		filesTouched: []string{"a.txt"},
		outcome:      "pre-branch work on main",
	})

	testutil.GitCheckoutNewBranch(t, dir, "my-feature")
	testutil.WriteFile(t, dir, "feature.md", "ship it")
	testutil.GitAdd(t, dir, "feature.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("feature work", mustCheckpointID(t, testCheckpointID)))

	featureCreatedAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "my-feature",
		createdAt:    featureCreatedAt,
		filesTouched: []string{"feature.md"},
		outcome:      testLocalFallbackText,
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return featureCreatedAt.Add(time.Hour) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{"my-feature"},
		ImplicitCurrentBranch: true,
		TextGenerator:         stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("expected 1 repo group, got %d", len(got.Repos))
	}
	for _, section := range got.Repos[0].Sections {
		for _, bullet := range section.Bullets {
			if strings.Contains(bullet.Text, "pre-branch work on main") {
				t.Fatalf("default-branch checkpoint leaked into feature-branch dispatch: %+v", bullet)
			}
		}
	}
}

// TestLocalMode_ImplicitCurrentBranchOnDefaultBranchIncludesMergedWork is the
// regression test for ENT-1188: on the default branch there is no parent
// history to exclude, so work done on feature branches and merged into it
// (summary.Branch is the feature branch, but the checkpoint trailer is
// reachable from the default branch's HEAD) must appear in the dispatch.
// Before the fix, branchLocalRevRange returned <default>..HEAD, which is empty
// on an up-to-date default branch, so every such checkpoint was dropped and
// the dispatch came back empty.
func TestLocalMode_ImplicitCurrentBranchOnDefaultBranchIncludesMergedWork(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir)

	// A single commit on the default branch carrying a checkpoint trailer,
	// simulating merged feature-branch work now reachable from HEAD.
	testutil.WriteFile(t, dir, "feature.md", "ship it")
	testutil.GitAdd(t, dir, "feature.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("feature work", mustCheckpointID(t, testCheckpointID)))

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "my-feature",
		createdAt:    createdAt,
		filesTouched: []string{"feature.md"},
		outcome:      testLocalFallbackText,
	})

	// Resolve the actual default branch name (go-git's PlainInit default) so
	// the test does not hard-code master vs main.
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	defaultBranch := head.Name().Short()

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(time.Hour) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{defaultBranch},
		ImplicitCurrentBranch: true,
		TextGenerator:         stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || len(got.Repos[0].Sections) == 0 || len(got.Repos[0].Sections[0].Bullets) == 0 ||
		got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("merged feature-branch work missing from default-branch dispatch: %+v", got)
	}
}

// TestLocalMode_ImplicitCurrentBranchFeatureBranchNoOwnCommitsIncludesMergedWork
// is the regression test for ENT-1201: dispatching from a feature-branch
// worktree that has no commits of its own (a fresh worktree branched off the
// up-to-date default branch, or a branch whose work has already merged into it)
// must still surface the default branch's in-window merged work reachable from
// HEAD. The bug: branchLocalRevRange returned <default>..HEAD for any
// non-default branch, which is EMPTY here, so the reachable-trailer set was
// empty and every merged checkpoint was dropped — the dispatch came back empty
// even though HEAD clearly carried the work. ENT-1188 fixed this only for the
// literal default branch (matched by name); this covers the general
// empty-range case, which the common worktree workflow hits constantly.
func TestLocalMode_ImplicitCurrentBranchFeatureBranchNoOwnCommitsIncludesMergedWork(t *testing.T) {
	dir := t.TempDir()
	stubGeneratedLocalDispatch(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "seed.txt", "seed")
	testutil.GitAdd(t, dir, "seed.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	// Merged feature-branch work now sitting ON the default branch, carrying a
	// checkpoint trailer and reachable from HEAD. The checkpoint's own branch is
	// the (merged) feature branch, not the default branch.
	testutil.WriteFile(t, dir, "feature.md", "ship it")
	testutil.GitAdd(t, dir, "feature.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint("feature work", mustCheckpointID(t, testCheckpointID)))

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "already-merged-feature",
		createdAt:    createdAt,
		filesTouched: []string{"feature.md"},
		outcome:      testLocalFallbackText,
	})

	// A fresh worktree branch off the default branch's tip: it shares HEAD with
	// the default branch and has no commits of its own, so <default>..HEAD is
	// empty. This is the standard `marvin wt new` / git-worktree setup.
	testutil.GitCheckoutNewBranch(t, dir, "my-worktree-branch")

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(time.Hour) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{"my-worktree-branch"},
		ImplicitCurrentBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || len(got.Repos[0].Sections) == 0 || len(got.Repos[0].Sections[0].Bullets) == 0 ||
		got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("merged work missing from feature-branch-with-no-own-commits dispatch: %+v", got)
	}
}

// TestLocalMode_IncludesReachableCheckpointMissingFromLocalStore is the other
// half of the ENT-1188 fix: dispatch --local must summarize work reachable from
// HEAD by commit trailer even when the checkpoint itself is absent from the
// local checkout (the common case — checkpoints are pushed to the remote from
// other worktrees and never fetched into this checkout). The commit subject is
// always available from git log, so the bullet falls back to it without any
// (slow) per-checkpoint network fetch. Before the fix, enumeration relied on
// store.List, which only sees local checkpoints, so this work was invisible.
func TestLocalMode_IncludesReachableCheckpointMissingFromLocalStore(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir)

	// A commit on the default branch carrying a checkpoint trailer, but the
	// checkpoint is intentionally NOT seeded into the local store.
	const subject = "landed remote work"
	testutil.WriteFile(t, dir, "feature.md", "ship it")
	testutil.GitAdd(t, dir, "feature.md")
	commitWithMessage(t, dir, trailers.FormatCheckpoint(subject, mustCheckpointID(t, testCheckpointID)))

	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	defaultBranch := head.Name().Short()

	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:                  ModeLocal,
		Since:                 "7d",
		Branches:              []string{defaultBranch},
		ImplicitCurrentBranch: true,
		TextGenerator:         stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || len(got.Repos[0].Sections) == 0 || len(got.Repos[0].Sections[0].Bullets) == 0 {
		t.Fatalf("reachable-but-unfetched checkpoint missing from dispatch: %+v", got)
	}
	if got.Repos[0].Sections[0].Bullets[0].Text != subject {
		t.Fatalf("expected bullet from commit subject %q, got %q", subject, got.Repos[0].Sections[0].Bullets[0].Text)
	}
}

func TestLocalMode_AllBranchesRestrictsToLocalBranches(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	testutil.GitCheckoutNewBranch(t, dir, "feature-a")

	createdAt := time.Now().UTC()
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           testCheckpointID,
		branch:       "feature-a",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      testLocalFallbackText,
	})
	seedCommittedCheckpoint(t, dir, seededCheckpoint{
		id:           "00ccccccccdd",
		branch:       "deleted-branch",
		createdAt:    createdAt,
		filesTouched: []string{"a.txt"},
		outcome:      "should not appear — branch no longer exists locally",
	})

	oldNow := nowUTC
	nowUTC = func() time.Time { return createdAt.Add(time.Hour) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:          ModeLocal,
		Since:         "7d",
		AllBranches:   true,
		TextGenerator: stubGeneratedLocalDispatch(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("expected 1 repo group, got %d", len(got.Repos))
	}
	for _, section := range got.Repos[0].Sections {
		for _, bullet := range section.Bullets {
			if strings.Contains(bullet.Text, "should not appear") {
				t.Fatalf("checkpoint for non-local branch leaked into --all-branches dispatch: %+v", bullet)
			}
		}
	}
}

func TestDefaultBranchRef_RejectsStaleRefNotAncestorOfHEAD(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "master commit")

	// Create an orphan branch so master and develop share no history; master
	// must not be accepted as a default-branch base for HEAD on develop.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("checkout", "--orphan", "develop")
	run("reset", "--hard")
	testutil.WriteFile(t, dir, "b.txt", "y")
	run("add", "b.txt")
	run("commit", "--no-gpg-sign", "-m", "develop root commit")

	got := defaultBranchRef(context.Background(), dir)
	if got != "" {
		t.Fatalf("expected defaultBranchRef to reject non-ancestor master, got %q", got)
	}
}

func TestDefaultBranchRef_RejectsStaleOriginHEADNotAncestorOfHEAD(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "master commit")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	// Fake a remote so refs/remotes/origin/master can exist.
	run("update-ref", "refs/remotes/origin/master", "HEAD")
	run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")

	// Move HEAD to a disjoint orphan branch. origin/HEAD still points at
	// origin/master, which is no longer an ancestor of HEAD — the fast path
	// must reject it instead of trusting the stale symbolic-ref target.
	run("checkout", "--orphan", "develop")
	run("reset", "--hard")
	testutil.WriteFile(t, dir, "b.txt", "y")
	run("add", "b.txt")
	run("commit", "--no-gpg-sign", "-m", "develop root commit")

	got := defaultBranchRef(context.Background(), dir)
	if got != "" {
		t.Fatalf("expected defaultBranchRef to reject stale origin/HEAD, got %q", got)
	}
}

func TestDefaultBranchRef_AcceptsAncestor(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "master commit")

	testutil.GitCheckoutNewBranch(t, dir, "feature")
	testutil.WriteFile(t, dir, "b.txt", "y")
	testutil.GitAdd(t, dir, "b.txt")
	testutil.GitCommit(t, dir, "feature commit")

	got := defaultBranchRef(context.Background(), dir)
	if got != "master" {
		t.Fatalf("expected defaultBranchRef to accept master as ancestor of feature, got %q", got)
	}
}

func TestReachableCheckpointIDsInRange_LimitsLogToWindowAndCheckpointTrailers(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "git-args.txt")
	gitPath := filepath.Join(tmpDir, "git")

	// The shim only captures args from `git log` invocations; other
	// subcommands (merge-base, symbolic-ref, rev-parse) are rejected so this
	// test cannot accidentally collect args from an unrelated git call if
	// future callers reorder resolution around the log.
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"log\" ]; then\n" +
		"  printf '%s\\n' \"$@\" > \"$TEST_GIT_ARGS_FILE\"\n" +
		"  printf '2026-04-02T10:00:00Z\\000subject\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000\\000'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_GIT_ARGS_FILE", argsFile)

	since := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
	until := time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC)
	reachable, err := reachableCheckpointIDsInRange(context.Background(), "/tmp/repo", "origin/main..HEAD", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reachable[testCheckpointID]; !ok {
		t.Fatalf("expected checkpoint %s to be reachable, got %v", testCheckpointID, reachable)
	}

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--grep") || !strings.Contains(args, "Entire-Checkpoint:") {
		t.Fatalf("expected git log to filter checkpoint trailers, got args %q", args)
	}
	if !strings.Contains(args, "--since=2026-04-01T12:30:00Z") {
		t.Fatalf("expected git log to bound history by since window, got args %q", args)
	}
	if !strings.Contains(args, "--until=2026-05-01T12:30:00Z") {
		t.Fatalf("expected git log to bound history by until window, got args %q", args)
	}
	if !strings.Contains(args, "origin/main..HEAD") {
		t.Fatalf("expected git log to use the supplied rev range, got args %q", args)
	}
}

// TestReachableCheckpointIDsInRange_KeepsInWindowTimeDespiteLaterCommit is the
// regression test for the bugbot finding: a checkpoint referenced by both an
// in-window commit and a later out-of-window commit must record its in-window
// time so the caller's [since, until) check does not drop it.
func TestReachableCheckpointIDsInRange_KeepsInWindowTimeDespiteLaterCommit(t *testing.T) {
	tmpDir := t.TempDir()
	gitPath := filepath.Join(tmpDir, "git")

	// git log emits newest-first: the out-of-window commit (after until) comes
	// before the in-window commit, both referencing the same checkpoint. Only
	// the in-window one should be recorded.
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"log\" ]; then\n" +
		"  printf '2026-06-15T10:00:00Z\\000later subject\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000\\000'\n" +
		"  printf '2026-04-10T10:00:00Z\\000in-window subject\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000\\000'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	reachable, err := reachableCheckpointIDsInRange(context.Background(), "/tmp/repo", "HEAD", since, until)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reachable[testCheckpointID]
	if !ok {
		t.Fatalf("expected checkpoint %s to be reachable via its in-window commit, got %v", testCheckpointID, reachable)
	}
	want := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("expected in-window commit time %s, got %s (later out-of-window commit leaked)", want, got)
	}
}

// TestSortCandidatesByRecency covers the trail-review finding: because the
// trailer fallback pass ranges over a map, candidates must be sorted before
// returning so dispatch output is stable across runs. Newest first, ties broken
// by checkpoint ID.
func TestSortCandidatesByRecency(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	candidates := []candidate{
		{CheckpointID: "ccc", CreatedAt: base},
		{CheckpointID: "aaa", CreatedAt: base.Add(2 * time.Hour)},
		{CheckpointID: "bbb", CreatedAt: base}, // same time as ccc → tiebreak by ID
		{CheckpointID: "zzz", CreatedAt: base.Add(time.Hour)},
	}
	sortCandidatesByRecency(candidates)

	got := make([]string, len(candidates))
	for i, c := range candidates {
		got[i] = c.CheckpointID
	}
	want := []string{"aaa", "zzz", "bbb", "ccc"} // newest first; bbb < ccc for the tie
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortCandidatesByRecency order = %v, want %v", got, want)
		}
	}
}

func TestLoadCommitSubjectsByCheckpoint_UsesSingleWindowedLogScan(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "git-args.txt")
	gitPath := filepath.Join(tmpDir, "git")

	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"log\" ]; then\n" +
		"  printf '%s\\n' \"$@\" > \"$TEST_GIT_ARGS_FILE\"\n" +
		"  printf 'latest subject\\000latest body\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000\\000older subject\\000older body\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000\\000'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_GIT_ARGS_FILE", argsFile)

	since := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
	subjects, err := loadCommitSubjectsByCheckpoint(context.Background(), "/tmp/repo", since)
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects[testCheckpointID]; got != "latest subject" {
		t.Fatalf("expected newest subject to win, got %q", got)
	}

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--since=2026-04-01T12:30:00Z") {
		t.Fatalf("expected git log to bound history by since window, got args %q", args)
	}
	if !strings.Contains(args, "--grep") || !strings.Contains(args, "Entire-Checkpoint:") {
		t.Fatalf("expected git log to filter checkpoint trailers, got args %q", args)
	}
	if strings.Contains(args, testCheckpointID) {
		t.Fatalf("did not expect one git log invocation per checkpoint, got args %q", args)
	}
}

func mustCheckpointID(t *testing.T, value string) checkpointid.CheckpointID {
	t.Helper()

	cpID, err := checkpointid.NewCheckpointID(value)
	if err != nil {
		t.Fatal(err)
	}
	return cpID
}

func commitWithMessage(t *testing.T, repoDir, message string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

type seededCheckpoint struct {
	id           string
	branch       string
	createdAt    time.Time
	filesTouched []string
	outcome      string
}

func stubGeneratedLocalDispatch() TextGenerator {
	return &stubTextGenerator{text: "generated dispatch"}
}

func seedCommittedCheckpoint(t *testing.T, repoDir string, cp seededCheckpoint) {
	t.Helper()

	repo, err := git.PlainOpenWithOptions(repoDir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	cpID, err := checkpointid.NewCheckpointID(cp.id)
	if err != nil {
		t.Fatal(err)
	}

	err = store.Write(context.Background(), checkpoint.Session{
		CheckpointID:     cpID,
		SessionID:        "session-1",
		Strategy:         "manual-commit",
		Branch:           cp.branch,
		Transcript:       redact.AlreadyRedacted([]byte("{\"type\":\"user\"}\n")),
		Prompts:          []string{"summarize recent work"},
		FilesTouched:     cp.filesTouched,
		CheckpointsCount: 1,
		Agent:            agent.AgentTypeClaudeCode,
		Summary: &checkpoint.Summary{
			Outcome: cp.outcome,
		},
		AuthorName:  "Test User",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func addOriginRemote(t *testing.T, repoDir string) {
	t.Helper()

	repo, err := git.PlainOpenWithOptions(repoDir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{testRepoRemoteURL},
	})
	if err != nil {
		t.Fatal(err)
	}
}
