package gitrepo

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// initEmptyIndexRepo builds a real git repository with three tracked files and
// one commit, using the real git binary under isolated config. Real git is the
// point of these tests: the failure being guarded against is a behaviour of
// git's own index reader, so nothing short of the binary reproduces it.
func initEmptyIndexRepo(t *testing.T) string {
	t.Helper()

	repoDir := t.TempDir()
	gitenv.Run(t, repoDir, "init", "-q", ".")
	gitenv.Run(t, repoDir, "config", "user.name", "Test User")
	gitenv.Run(t, repoDir, "config", "user.email", "test@example.com")
	gitenv.Run(t, repoDir, "config", "commit.gpgsign", "false")

	if err := os.MkdirAll(filepath.Join(repoDir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	for path, content := range map[string]string{
		"one.txt":       "one\n",
		"two.txt":       "two\n",
		"sub/three.txt": "three\n",
	} {
		if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(path)), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	gitenv.Run(t, repoDir, "add", "-A")
	gitenv.Run(t, repoDir, "commit", "-qm", "initial")
	return repoDir
}

func indexPathOf(repoDir string) string {
	return filepath.Join(repoDir, ".git", "index")
}

// The hazard probe: a real git hook cannot call into this package, so it
// re-executes the test binary, which runs the guard in-process against the
// index git handed the hook and prints the verdict.
const (
	hazardProbeEnabledEnv = "ENTIRE_TEST_HAZARD_PROBE"
	hazardProbeRepoEnv    = "ENTIRE_TEST_HAZARD_REPO"
	hazardProbeQuiet      = "HAZARD-PROBE-QUIET"
	hazardProbeNoIndexEnv = "HAZARD-PROBE-NO-GIT-INDEX-FILE"
)

// TestHazardProbeHelper is not a test. It is the body of the prepare-commit-msg
// hook that TestEmptyIndexCommitDestroysStagedWork installs: git runs the test
// binary, which lands here, evaluates the pre-commit guard at the one moment
// that answers the real question, and prints what a user would have been shown.
func TestHazardProbeHelper(t *testing.T) {
	if os.Getenv(hazardProbeEnabledEnv) == "" {
		t.Skip("hook body, not a test")
	}
	if os.Getenv("GIT_INDEX_FILE") == "" {
		t.Log(hazardProbeNoIndexEnv)
		return
	}
	hazard := DetectEmptyIndexHazard(t.Context(), os.Getenv(hazardProbeRepoEnv), os.Getenv("GIT_INDEX_FILE"))
	if hazard == nil {
		t.Log(hazardProbeQuiet)
		return
	}
	t.Log(hazard.Message())
}

func TestIndexRecordsNoEntries_PopulatedIndexIsNotEmpty(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)

	empty, err := IndexRecordsNoEntries(indexPathOf(repoDir))
	if err != nil {
		t.Fatalf("IndexRecordsNoEntries: %v", err)
	}
	if empty {
		t.Fatal("an index holding three staged entries was reported empty")
	}
}

func TestIndexRecordsNoEntries_EmptiedIndexIsEmpty(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	// The one way a user genuinely empties the index while keeping the files.
	gitenv.Run(t, repoDir, "rm", "-r", "--cached", "-q", ".")

	empty, err := IndexRecordsNoEntries(indexPathOf(repoDir))
	if err != nil {
		t.Fatalf("IndexRecordsNoEntries: %v", err)
	}
	if !empty {
		t.Fatal("an index git emptied with rm --cached was not reported empty")
	}
}

// TestIndexRecordsNoEntries_MissingIndexIsNotReportedEmpty pins the conflation
// this whole file exists to break. Git and go-git both answer "empty index" for
// a `.git/index` that is not there; this reader answers os.ErrNotExist, because
// "the file is gone" and "nothing is staged" are different facts and only one
// of them justifies committing an empty tree.
func TestIndexRecordsNoEntries_MissingIndexIsNotReportedEmpty(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	if err := os.Remove(indexPathOf(repoDir)); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	empty, err := IndexRecordsNoEntries(indexPathOf(repoDir))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist for a missing index, got %v", err)
	}
	if empty {
		t.Fatal("a missing index was reported as an empty index — the #2111 conflation")
	}
}

func TestIndexRecordsNoEntries_RejectsNonIndexFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "not-an-index")
	if err := os.WriteFile(path, []byte("this is not a git index at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := IndexRecordsNoEntries(path); !errors.Is(err, ErrNotAnIndexFile) {
		t.Fatalf("want ErrNotAnIndexFile, got %v", err)
	}

	if _, err := IndexRecordsNoEntries(dir); !errors.Is(err, ErrNotAnIndexFile) {
		t.Fatalf("want ErrNotAnIndexFile for a directory, got %v", err)
	}

	short := filepath.Join(dir, "truncated")
	if err := os.WriteFile(short, []byte("DIRC"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := IndexRecordsNoEntries(short); !errors.Is(err, ErrNotAnIndexFile) {
		t.Fatalf("want ErrNotAnIndexFile for a truncated header, got %v", err)
	}
}

// TestIndexRecordsNoEntries_SplitIndexIsNotEmpty covers the one shape where a
// zero entry count does not mean an empty index: a split index keeps its
// entries in .git/sharedindex.<hash>.
func TestIndexRecordsNoEntries_SplitIndexIsNotEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "index")

	header := make([]byte, 0, 20)
	header = append(header, []byte("DIRC")...)
	header = binary.BigEndian.AppendUint32(header, 2) // version
	header = binary.BigEndian.AppendUint32(header, 0) // entry count
	header = append(header, []byte("link")...)        // split-index extension
	header = append(header, 0, 0, 0, 0)               // extension length
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	empty, err := IndexRecordsNoEntries(path)
	if err != nil {
		t.Fatalf("IndexRecordsNoEntries: %v", err)
	}
	if empty {
		t.Fatal("a split index was reported empty; its entries live in the shared index")
	}
}

// TestEmptyIndexCommitDestroysStagedWork is the bug, end to end, with the real
// git binary: staged work is lost, every tracked file is deleted by the commit,
// git exits 0 and says nothing — and the guard sees all of it, both before the
// commit exists and after.
//
// Removing `.git/index` outright is a deterministic stand-in for the race in
// #2111: on a virtiofs / gRPC-FUSE bind mount a concurrent rename over the
// index makes a reader observe ENOENT for a file that exists. Git's reaction to
// ENOENT is the failure, and it is identical either way — an empty in-core
// index — so this reproduces the consequence without needing the filesystem.
func TestEmptyIndexCommitDestroysStagedWork(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)

	const userWork = "WORK THE USER MUST NOT LOSE\n"
	if err := os.WriteFile(filepath.Join(repoDir, "one.txt"), []byte("one\n"+userWork), 0o644); err != nil {
		t.Fatalf("write one.txt: %v", err)
	}
	gitenv.Run(t, repoDir, "add", "one.txt")
	if staged := gitenv.Run(t, repoDir, "diff", "--cached", "--name-only"); !strings.Contains(staged, "one.txt") {
		t.Fatalf("one.txt was not staged, got %q", staged)
	}

	// Run the pre-commit guard from a real prepare-commit-msg hook, at the
	// moment git runs it, over the index git handed the hook. Evaluating it
	// after the commit would be a different question: by then HEAD is the
	// empty-tree commit and there is nothing left to compare against.
	probeOutput := filepath.Join(t.TempDir(), "probe-output")
	hookPath := filepath.Join(repoDir, ".git", "hooks", "prepare-commit-msg")
	hook := "#!/bin/sh\n" +
		hazardProbeEnabledEnv + "=1 " + hazardProbeRepoEnv + "=" + repoDir + " " +
		os.Args[0] + " -test.run='^TestHazardProbeHelper$' -test.v > " + probeOutput + " 2>&1\n" +
		"exit 0\n"
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	// The window: git is about to read an index that is not there.
	if err := os.Remove(indexPathOf(repoDir)); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	// Real git, real commit. It succeeds, which is the whole problem.
	gitenv.Run(t, repoDir, "commit", "-qm", "record the user's work")

	// The damage, stated three ways.
	if tracked := strings.TrimSpace(gitenv.Run(t, repoDir, "ls-tree", "-r", "--name-only", "HEAD")); tracked != "" {
		t.Fatalf("expected the commit to record no files at all, got:\n%s", tracked)
	}
	if shown := gitenv.Run(t, repoDir, "show", "--stat", "--oneline", "HEAD"); !strings.Contains(shown, "3 files changed") {
		t.Fatalf("expected the commit to delete all three tracked files, got:\n%s", shown)
	}
	onDisk, err := os.ReadFile(filepath.Join(repoDir, "one.txt"))
	if err != nil || !strings.Contains(string(onDisk), userWork) {
		t.Fatalf("the user's work should still be on disk: %v / %q", err, onDisk)
	}

	// What the guard said, in the hook, before the commit existed.
	probe, err := os.ReadFile(probeOutput)
	if err != nil {
		t.Fatalf("the prepare-commit-msg hook did not run: %v", err)
	}
	if strings.Contains(string(probe), hazardProbeNoIndexEnv) {
		t.Fatalf("git did not export GIT_INDEX_FILE to the commit hook:\n%s", probe)
	}
	if strings.Contains(string(probe), hazardProbeQuiet) {
		t.Fatalf("pre-commit guard missed an index git read as empty while tracked files were present:\n%s", probe)
	}
	if !strings.Contains(string(probe), "is about to record an EMPTY tree") {
		t.Fatalf("pre-commit guard did not warn:\n%s", probe)
	}
	for _, want := range []string{"one.txt", "git reset --mixed HEAD~1"} {
		if !strings.Contains(string(probe), want) {
			t.Fatalf("pre-commit warning does not mention %q:\n%s", want, probe)
		}
	}
	t.Logf("pre-commit guard, from inside the real hook:\n%s", probe)

	repo, err := OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	after := DetectEmptyTreeCommitHazard(t.Context(), repoDir, commit)
	if after == nil {
		t.Fatal("post-commit guard missed a commit whose tree is empty while its parent's files are on disk")
	}
	for _, want := range []string{"EMPTY tree", "git reset --mixed HEAD~1", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(after.Message(), want) {
			t.Fatalf("warning does not mention %q:\n%s", want, after.Message())
		}
	}

	// The recovery the warning prescribes has to actually work.
	gitenv.Run(t, repoDir, "reset", "--mixed", "HEAD~1")
	restored := gitenv.Run(t, repoDir, "ls-tree", "-r", "--name-only", "HEAD")
	for _, path := range []string{"one.txt", "two.txt", "sub/three.txt"} {
		if !strings.Contains(restored, path) {
			t.Fatalf("git reset --mixed HEAD~1 did not restore %s:\n%s", path, restored)
		}
	}
	recoveredWork, err := os.ReadFile(filepath.Join(repoDir, "one.txt"))
	if err != nil || !strings.Contains(string(recoveredWork), userWork) {
		t.Fatalf("the user's work did not survive recovery: %v / %q", err, recoveredWork)
	}
}

func TestDetectEmptyIndexHazard_QuietOnAHealthyCommit(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "two.txt"), []byte("two\nmore\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitenv.Run(t, repoDir, "add", "two.txt")

	if hazard := DetectEmptyIndexHazard(t.Context(), repoDir, indexPathOf(repoDir)); hazard != nil {
		t.Fatalf("a normal staged commit was flagged:\n%s", hazard.Message())
	}
}

func TestDetectEmptyIndexHazard_QuietWithoutAnIndexPath(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	if hazard := DetectEmptyIndexHazard(t.Context(), repoDir, ""); hazard != nil {
		t.Fatal("guard fired with no index path to read")
	}
	if hazard := DetectEmptyIndexHazard(t.Context(), "", indexPathOf(repoDir)); hazard != nil {
		t.Fatal("guard fired with no worktree root to check against")
	}
}

// TestDetectEmptyIndexHazard_QuietWhenTheFilesAreGoneToo is the intent case:
// someone who deleted every tracked file is committing exactly what they meant
// to, and must not be warned.
func TestDetectEmptyIndexHazard_QuietWhenTheFilesAreGoneToo(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	gitenv.Run(t, repoDir, "rm", "-r", "-q", ".")

	empty, err := IndexRecordsNoEntries(indexPathOf(repoDir))
	if err != nil || !empty {
		t.Fatalf("git rm -r . should leave an empty index: %v / %v", empty, err)
	}
	if hazard := DetectEmptyIndexHazard(t.Context(), repoDir, indexPathOf(repoDir)); hazard != nil {
		t.Fatalf("a deliberate delete-everything commit was flagged:\n%s", hazard.Message())
	}
}

func TestDetectEmptyTreeCommitHazard_QuietCases(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	repo, err := OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	rootCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}

	if hazard := DetectEmptyTreeCommitHazard(t.Context(), repoDir, nil); hazard != nil {
		t.Fatal("guard fired on a nil commit")
	}
	if hazard := DetectEmptyTreeCommitHazard(t.Context(), repoDir, rootCommit); hazard != nil {
		t.Fatalf("guard fired on an ordinary commit:\n%s", hazard.Message())
	}

	// A root commit that records nothing removes nothing.
	emptyRoot := t.TempDir()
	gitenv.Run(t, emptyRoot, "init", "-q", ".")
	gitenv.Run(t, emptyRoot, "config", "user.name", "Test User")
	gitenv.Run(t, emptyRoot, "config", "user.email", "test@example.com")
	gitenv.Run(t, emptyRoot, "config", "commit.gpgsign", "false")
	gitenv.Run(t, emptyRoot, "commit", "-q", "--allow-empty", "-m", "root")

	emptyRepo, err := OpenPath(emptyRoot)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer emptyRepo.Close()
	emptyHead, err := emptyRepo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	emptyCommit, err := emptyRepo.CommitObject(emptyHead.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	if hazard := DetectEmptyTreeCommitHazard(t.Context(), emptyRoot, emptyCommit); hazard != nil {
		t.Fatalf("guard fired on an empty root commit:\n%s", hazard.Message())
	}
}

// TestDetectEmptyTreeCommitHazard_QuietWhenTheFilesAreGoneToo is the
// post-commit counterpart of the intent case above.
func TestDetectEmptyTreeCommitHazard_QuietWhenTheFilesAreGoneToo(t *testing.T) {
	t.Parallel()

	repoDir := initEmptyIndexRepo(t)
	gitenv.Run(t, repoDir, "rm", "-r", "-q", ".")
	gitenv.Run(t, repoDir, "commit", "-qm", "remove everything, on purpose")

	repo, err := OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}

	if hazard := DetectEmptyTreeCommitHazard(t.Context(), repoDir, commit); hazard != nil {
		t.Fatalf("a deliberate delete-everything commit was flagged:\n%s", hazard.Message())
	}
}
