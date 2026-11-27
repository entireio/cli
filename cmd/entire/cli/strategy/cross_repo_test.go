package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/go-git/go-git/v6/plumbing"
)

// crossRepoAncestry returns this process's ancestry, skipping where proclive
// cannot introspect processes.
func crossRepoAncestry(t *testing.T, minDepth int) []proclive.Identity {
	t.Helper()
	ancestry, ok := proclive.CurrentAncestry()
	if !ok || len(ancestry.Chain()) < minDepth {
		t.Skip("process ancestry is not available on this platform")
	}
	return ancestry.Chain()
}

func TestCrossRepoPointer_NearestOwnerInAncestryWins(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chain := crossRepoAncestry(t, 2)
	ctx := context.Background()

	if p := findCrossRepoPointer(ctx); p != nil {
		t.Fatalf("no pointer written, found %+v", p)
	}
	for _, p := range []crossRepoPointer{
		{SessionID: "outer", Owner: chain[1], Worktree: "/c"},
		{SessionID: "inner", Owner: chain[0], Worktree: "/c"},
	} {
		if err := writeCrossRepoPointer(p); err != nil {
			t.Fatal(err)
		}
	}
	p := findCrossRepoPointer(ctx)
	if p == nil || p.SessionID != "inner" {
		t.Fatalf("got %+v, want the nearest owner's session", p)
	}
}

func TestCrossRepoPointer_IgnoresAPointerFiledUnderAnotherOwner(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chain := crossRepoAncestry(t, 1)
	// Filed under the ancestor's name, but naming a different process: a
	// pointer counts only for the owner it records.
	forged := crossRepoPointer{SessionID: "forged", Owner: proclive.Identity{PID: chain[0].PID, Start: "other", Host: chain[0].Host}}
	if err := writeCrossRepoPointerAs(crossRepoPointerName(chain[0]), forged); err != nil {
		t.Fatal(err)
	}
	if p := findCrossRepoPointer(context.Background()); p != nil {
		t.Fatalf("got %+v, want nothing", p)
	}
}

func TestCrossRepoPointer_GCDropsExitedOwners(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	self, ok := proclive.IdentityOf(os.Getpid())
	if !ok {
		t.Skip("process identity is not available on this platform")
	}
	dead := self
	dead.Start = "exited-long-ago"
	for _, p := range []crossRepoPointer{{SessionID: "live", Owner: self}, {SessionID: "dead", Owner: dead}} {
		if err := writeCrossRepoPointer(p); err != nil {
			t.Fatal(err)
		}
	}
	gcCrossRepoPointers()

	root, err := openCrossRepoPointers(false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.Lstat(crossRepoPointerName(self)); err != nil {
		t.Errorf("live owner's pointer removed: %v", err)
	}
	if _, err := root.Lstat(crossRepoPointerName(dead)); !os.IsNotExist(err) {
		t.Errorf("exited owner's pointer kept: %v", err)
	}
}

func TestClearCrossRepoPointers_RemovesTheDirectory(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	if err := writeCrossRepoPointer(crossRepoPointer{SessionID: "s", Owner: proclive.Identity{PID: 1, Start: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := ClearCrossRepoPointers(); err != nil {
		t.Fatal(err)
	}
	if _, err := openCrossRepoPointers(false); !os.IsNotExist(err) {
		t.Fatalf("pointer directory still there: %v", err)
	}
	if err := ClearCrossRepoPointers(); err != nil {
		t.Fatalf("clearing nothing: %v", err)
	}
}

func TestCrossRepoInstalls_RecordedOnceEach(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	for _, repo := range []string{"/d1", "/d2", "/d1"} {
		if err := recordCrossRepoInstall(repo); err != nil {
			t.Fatal(err)
		}
	}
	got := CrossRepoInstalls()
	if len(got) != 2 || got[0] != "/d1" || got[1] != "/d2" {
		t.Fatalf("got %v", got)
	}
}

func TestLoadCrossRepoHome_SameRepositoryIsNotCrossRepo(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	home, err := loadCrossRepoHome(context.Background(), "s1", dir)
	if !errors.Is(err, errNotCrossRepo) || home != nil {
		t.Fatalf("got %v, %v; want nothing for this repository's own session", home, err)
	}
}

func TestCrossRepoEditedOverlap(t *testing.T) {
	t.Parallel()
	committed := map[string]struct{}{"a.go": {}, "b.go": {}}
	if !editedOverlap(committed, []string{"x.go", "b.go"}) {
		t.Error("overlap missed")
	}
	if editedOverlap(committed, []string{"x.go"}) || editedOverlap(committed, nil) {
		t.Error("overlap without a shared path")
	}
}

// plainRepo is a repository as `git init` makes it: a main worktree with git's
// sample hooks and no core.hooksPath.
func plainRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, dir, "init", "-q")
	return dir
}

func TestCrossRepoInstallBlocker_MainWorktreeWithoutHooksPath(t *testing.T) {
	for name, sub := range map[string]string{"root": "", "subdirectory": "pkg"} {
		t.Run(name, func(t *testing.T) {
			dir := plainRepo(t)
			t.Setenv("HOME", filepath.Dir(dir))
			cwd := filepath.Join(dir, sub)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			ClearHooksDirCache()
			t.Cleanup(ClearHooksDirCache)
			if reason := crossRepoInstallBlocker(context.Background()); reason != "" {
				t.Fatalf("an ordinary repository blocked the install: %q", reason)
			}
		})
	}
}

func TestCrossRepoInstallBlocker_SharedHooksPathBlocks(t *testing.T) {
	dir := plainRepo(t)
	t.Setenv("HOME", filepath.Dir(dir))
	testutil.RunGit(t, dir, "config", "core.hooksPath", t.TempDir())
	t.Chdir(dir)
	ClearHooksDirCache()
	t.Cleanup(ClearHooksDirCache)
	if reason := crossRepoInstallBlocker(context.Background()); reason == "" {
		t.Fatal("a hooks directory outside the repository did not block the install")
	}
}

func TestCrossRepoInstallOwner_LinkedWorktreeNamesTheRepository(t *testing.T) {
	dir := plainRepo(t)
	testutil.RunGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-wt")
	testutil.RunGit(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
	t.Chdir(wt)
	got, err := crossRepoInstallOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("got %s, want the repository %s", got, dir)
	}
}

func TestCrossRepoStatus_RecentSessionsShownAfterTheAgentExits(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	dir := plainRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	for _, id := range []string{"s1", "s2", "s1"} {
		recordCrossRepoLastLink(ctx, crossRepoLastLink{SessionID: id, HomeRepo: "home"})
	}
	inbound, _ := CrossRepoStatus(ctx)
	if len(inbound) != 2 || inbound[0].SessionID != "s1" || inbound[1].SessionID != "s2" {
		t.Fatalf("got %+v, want s1 then s2", inbound)
	}
	if inbound[0].Live {
		t.Error("a session with no pointer reported live")
	}
}

func TestPublicRepoKey_NeverALocalPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private-name")
	testutil.InitRepo(t, dir)
	if got := publicRepoKey(ctx, dir); got != "private-name" {
		t.Errorf("no origin: got %q, want the directory name", got)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v\n%s", err, out)
	}
	if got := publicRepoKey(ctx, dir); strings.HasPrefix(got, "/") || !strings.Contains(got, "acme/widgets") {
		t.Errorf("with origin: got %q, want the origin key", got)
	}
	if got := publicRepoKey(ctx, ""); got != "" {
		t.Errorf("no worktree: got %q", got)
	}
}

func TestLinkCrossRepoCommits_ConcurrentSweepsLinkOnce(t *testing.T) {
	ctx := context.Background()
	home, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{home, other} {
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "seed.txt", "seed")
		testutil.GitAdd(t, dir, "seed.txt")
		testutil.GitCommit(t, dir, "seed")
	}
	store, err := session.NewStateStoreForWorktree(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, &session.State{SessionID: "s1", WorktreePath: home, BaseCommit: "x", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Minute)
	testutil.WriteFile(t, other, "x.go", "package x")
	testutil.GitAdd(t, other, "x.go")
	testutil.GitCommit(t, other, "agent work")
	t.Chdir(other)

	const sweeps = 4
	counts := make(chan int, sweeps)
	var wg sync.WaitGroup
	for range sweeps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := (&ManualCommitStrategy{}).LinkCrossRepoCommits(ctx, "s1", home, since, []string{"x.go"}, nil)
			if err != nil {
				t.Error(err)
			}
			counts <- n
		}()
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("concurrent sweeps linked %d commits, want 1", total)
	}
	journal, err := os.ReadFile(filepath.Join(home, ".git", crossRepoLinksName("s1")))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(journal), "\n"); lines != 1 {
		t.Fatalf("home journal has %d links, want 1", lines)
	}
}

// linkSweepFixture makes a home repository with session s1 and another
// repository with commits agent commits on top of a seed, and chdirs into it.
func linkSweepFixture(t *testing.T, commits int) (home string, since time.Time) {
	t.Helper()
	ctx := context.Background()
	home, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{home, other} {
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "seed.txt", "seed")
		testutil.GitAdd(t, dir, "seed.txt")
		testutil.GitCommit(t, dir, "seed")
	}
	store, err := session.NewStateStoreForWorktree(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, &session.State{SessionID: "s1", WorktreePath: home, BaseCommit: "x", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	since = time.Now().Add(-time.Minute)
	for i := range commits {
		testutil.WriteFile(t, other, "x.go", fmt.Sprintf("package x // %d", i))
		testutil.GitAdd(t, other, "x.go")
		testutil.GitCommit(t, other, fmt.Sprintf("agent work %d", i))
	}
	t.Chdir(other)
	return home, since
}

func homeJournalLines(t *testing.T, home string) int {
	t.Helper()
	journal, err := os.ReadFile(filepath.Join(home, ".git", crossRepoLinksName("s1")))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(journal), "\n")
}

// A run stopped after its first marker (the foreground budget kills it) must
// leave that commit recorded, so the retry does not link it a second time.
func TestLinkCrossRepoCommits_StoppedRunRetryLinksEachCommitOnce(t *testing.T) {
	home, since := linkSweepFixture(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	crossRepoMarkerWritten = cancel
	t.Cleanup(func() { crossRepoMarkerWritten = nil })
	n, err := (&ManualCommitStrategy{}).LinkCrossRepoCommits(ctx, "s1", home, since, []string{"x.go"}, nil)
	if n != 1 || err == nil {
		t.Fatalf("stopped run linked %d (err %v), want 1 and an error", n, err)
	}
	crossRepoMarkerWritten = nil
	n, err = (&ManualCommitStrategy{}).LinkCrossRepoCommits(context.Background(), "s1", home, since, []string{"x.go"}, nil)
	if err != nil || n != 1 {
		t.Fatalf("retry linked %d (err %v), want the remaining 1", n, err)
	}
	if lines := homeJournalLines(t, home); lines != 2 {
		t.Fatalf("home journal has %d links, want 2", lines)
	}
}

// An orphan worktree reports HEAD as the zero hash; it must not make rev-list
// fail and the sweep link nothing.
func TestLinkCrossRepoCommits_OrphanWorktreeDoesNotStopTheSweep(t *testing.T) {
	home, since := linkSweepFixture(t, 1)
	other, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, other, "worktree", "add", "-q", "--orphan", "-b", "empty", filepath.Join(t.TempDir(), "orphan"))
	if heads := worktreeHeads(context.Background(), other); slices.Contains(heads, plumbing.ZeroHash.String()) {
		t.Fatalf("heads include the zero hash: %v", heads)
	}
	n, err := (&ManualCommitStrategy{}).LinkCrossRepoCommits(context.Background(), "s1", home, since, []string{"x.go"}, nil)
	if err != nil || n != 1 {
		t.Fatalf("linked %d (err %v), want 1", n, err)
	}
}

// withLinkedRepos must not advance the cursor itself: a checkpoint write that
// fails after it would otherwise drop those links from every checkpoint's
// per-checkpoint delta.
func TestWithLinkedRepos_LeavesTheCursorToTheCaller(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	state := &SessionState{SessionID: "s1", CrossRepoLinksCondensed: 1}
	home := &crossRepoHome{state: state, commonDir: filepath.Join(dir, ".git")}
	for _, sha := range []string{"aaa", "bbb", "ccc"} {
		if err := appendCrossRepoLink(home, cpkg.LinkedRepoCommit{RepoKey: "r", SHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	var opts cpkg.WriteOptions
	next := withLinkedRepos(context.Background(), &opts, state)
	if next != 3 || state.CrossRepoLinksCondensed != 1 {
		t.Fatalf("next %d, state cursor %d; want 3 and the cursor untouched", next, state.CrossRepoLinksCondensed)
	}
	if len(opts.LinkedRepos) != 2 || opts.LinkedRepos[0].SHA != "bbb" || len(opts.SessionLinkedRepos) != 3 {
		t.Fatalf("delta %+v, all %+v", opts.LinkedRepos, opts.SessionLinkedRepos)
	}
}

// A lazy install never goes outside the home directory unasked; the
// developer lifts that floor per repository with `entire track allow <path>`
// or by enabling it there.
func TestCrossRepoInstallBlocker_HomeFloor(t *testing.T) {
	for name, tc := range map[string]struct {
		userSettings func(dir string) string
		local        string
		blocked      bool
	}{
		"outside home":       {userSettings: func(string) string { return `{"global_enable": true}` }, blocked: true},
		"allowed by path":    {userSettings: func(dir string) string { return `{"global_enable": true, "track": {"repos": ["` + dir + `"]}}` }},
		"enabled there":      {userSettings: func(string) string { return `{"global_enable": true}` }, local: `{"enabled": true}`},
		"denied, not lifted": {userSettings: func(dir string) string { return `{"global_enable": true, "track": {"deny": ["` + dir + `"]}}` }, blocked: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := plainRepo(t)
			t.Setenv("HOME", t.TempDir())
			configDir := t.TempDir()
			t.Setenv(userdirs.EnvConfigDir, configDir)
			if err := os.WriteFile(filepath.Join(configDir, usersettings.FileName), []byte(tc.userSettings(dir)), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.local != "" {
				testutil.WriteFile(t, dir, ".gitignore", ".entire/settings.local.json\n")
				testutil.WriteFile(t, dir, ".entire/settings.local.json", tc.local)
			}
			t.Chdir(dir)
			ClearHooksDirCache()
			t.Cleanup(ClearHooksDirCache)
			settings.ClearTrackCache()
			t.Cleanup(settings.ClearTrackCache)
			reason := crossRepoInstallBlocker(context.Background())
			if tc.blocked != strings.Contains(reason, "outside the home directory") {
				t.Fatalf("reason %q, want blocked by the home floor: %v", reason, tc.blocked)
			}
		})
	}
}
