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
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestLocalMode_EnumeratesCheckpoints(t *testing.T) {
	dir := t.TempDir()
	stubGeneratedLocalDispatch(t)
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
		Mode:     ModeLocal,
		Since:    "7d",
		Branches: []string{"main"},
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
	if got.Repos[0].Sections[0].Bullets[0].Text != testLocalFallbackText {
		t.Fatalf("unexpected bullet: %+v", got.Repos[0].Sections[0].Bullets[0])
	}
	if len(got.CoveredRepos) != 1 || got.CoveredRepos[0] != testRepoFullName {
		t.Fatalf("unexpected covered repos: %v", got.CoveredRepos)
	}
}

func TestLocalMode_UsesUntilWindow(t *testing.T) {
	dir := t.TempDir()
	stubGeneratedLocalDispatch(t)
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
		Mode:     ModeLocal,
		Since:    "7d",
		Until:    now.Add(-time.Hour).Format(time.RFC3339),
		Branches: []string{"main"},
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
	stubGeneratedLocalDispatch(t)
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
		Mode:     ModeLocal,
		Since:    "7d",
		Branches: []string{"main"},
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
	oldFactory := dispatchTextGeneratorFactory
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	mock := &stubTextGenerator{text: "generated inline dispatch"}
	dispatchTextGeneratorFactory = func() (dispatchTextGenerator, error) {
		return mock, nil
	}
	t.Cleanup(func() {
		nowUTC = oldNow
		dispatchTextGeneratorFactory = oldFactory
	})

	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:     ModeLocal,
		Since:    "7d",
		Branches: []string{"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedText != "generated inline dispatch" {
		t.Fatalf("expected generated text, got %q", got.GeneratedText)
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
	oldFactory := dispatchTextGeneratorFactory
	nowUTC = func() time.Time { return createdAt.Add(2 * time.Hour) }
	dispatchTextGeneratorFactory = func() (dispatchTextGenerator, error) {
		return &stubTextGenerator{text: "  \n\t "}, nil
	}
	t.Cleanup(func() {
		nowUTC = oldNow
		dispatchTextGeneratorFactory = oldFactory
	})

	t.Chdir(dir)

	_, err := Run(context.Background(), Options{
		Mode:     ModeLocal,
		Since:    "7d",
		Branches: []string{"main"},
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
	stubGeneratedLocalDispatch(t)
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
	store := checkpoint.NewGitStore(repo)
	parsedID, err := checkpointid.NewCheckpointID(cpID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
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
	stubGeneratedLocalDispatch(t)
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
	store := checkpoint.NewGitStore(repo)
	parsedID, err := checkpointid.NewCheckpointID(cpID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
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
		Mode:     ModeLocal,
		Since:    "7d",
		Branches: []string{"entire-dispatch-codex"},
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
	stubGeneratedLocalDispatch(t)
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
	stubGeneratedLocalDispatch(t)
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

func TestLocalMode_AllBranchesRestrictsToLocalBranches(t *testing.T) {
	dir := t.TempDir()
	stubGeneratedLocalDispatch(t)
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
		Mode:        ModeLocal,
		Since:       "7d",
		AllBranches: true,
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
		"  printf 'subject\\n\\nEntire-Checkpoint: " + testCheckpointID + "\\000'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_GIT_ARGS_FILE", argsFile)

	since := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
	reachable, err := reachableCheckpointIDsInRange(context.Background(), "/tmp/repo", "origin/main..HEAD", since)
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
	if !strings.Contains(args, "origin/main..HEAD") {
		t.Fatalf("expected git log to use the supplied rev range, got args %q", args)
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

func stubGeneratedLocalDispatch(t *testing.T) {
	t.Helper()

	oldFactory := dispatchTextGeneratorFactory
	dispatchTextGeneratorFactory = func() (dispatchTextGenerator, error) {
		return &stubTextGenerator{text: "generated dispatch"}, nil
	}
	t.Cleanup(func() {
		dispatchTextGeneratorFactory = oldFactory
	})
}

func seedCommittedCheckpoint(t *testing.T, repoDir string, cp seededCheckpoint) {
	t.Helper()

	repo, err := git.PlainOpenWithOptions(repoDir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		t.Fatal(err)
	}

	store := checkpoint.NewGitStore(repo)
	cpID, err := checkpointid.NewCheckpointID(cp.id)
	if err != nil {
		t.Fatal(err)
	}

	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
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
