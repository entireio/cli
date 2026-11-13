package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"

	"github.com/stretchr/testify/require"
)

// trackedCaseRepo returns a repository that tracks README.md and src/App.go,
// and its worktree root.
func trackedCaseRepo(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	// Close the shared root opened on dir before it is removed: Windows
	// cannot remove a directory while a handle to it is open. t.Cleanup runs
	// in reverse order, so this runs before the removal TempDir registered.
	// Forgetting only dir keeps it safe for parallel tests.
	t.Cleanup(func() { osroot.Forget(dir) })
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.WriteFile(t, dir, "src/App.go", "package src\n")
	testutil.GitAdd(t, dir, "README.md", "src/App.go")
	testutil.GitCommit(t, dir, "Add files")
	worktree, err := worktreedir.OpenAt(dir)
	require.NoError(t, err)
	return dir, worktree
}

// TestTrackedSpellings_CaseInsensitiveFilesystemWindows checks the rewrite
// where it applies, on a case-insensitive filesystem: CI's Windows job runs
// it by its name. On a case-sensitive one it checks that nothing changes.
func TestTrackedSpellings_CaseInsensitiveFilesystemWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	files := []string{"readme.md", "SRC/app.go", "README.md", "new.txt"}
	got := TrackedSpellings(context.Background(), dir, files)
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.Equal(t, []string{"README.md", "src/App.go", "new.txt"}, got,
			"spelled as git spells them, the duplicate dropped; an untracked name is kept")
	} else {
		require.Equal(t, files, got, "readme.md and SRC/app.go are other files here")
	}
}

// ignoresCaseIn stands in for a worktree where the directory it names finds
// its entries ignoring case and the others respect case.
type ignoresCaseIn string

func (dir ignoresCaseIn) IgnoresCase(d string) bool {
	return d == string(dir)
}

func (dir ignoresCaseIn) SameEntry(file, spelling string) bool {
	return path.Dir(file) == string(dir) && path.Dir(spelling) == string(dir) && strings.EqualFold(file, spelling)
}

// TestAnyDirectoryIgnoresCase checks that each directory on a path counts,
// not only the one holding the file: SRC/App.go in a root that ignores case
// opens src/App.go even when src/ respects case, as Windows allows.
func TestAnyDirectoryIgnoresCase(t *testing.T) {
	t.Parallel()
	require.True(t, anyDirectoryIgnoresCase(ignoresCaseIn("."), []string{"SRC/App.go"}), "the root ignores case")
	require.True(t, anyDirectoryIgnoresCase(ignoresCaseIn("src"), []string{"README.md", "src/a/b.go"}), "an ancestor of b.go ignores case")
	require.False(t, anyDirectoryIgnoresCase(ignoresCaseIn("src"), []string{"README.md", "docs/a.md"}), "no directory on these paths ignores case")
	require.False(t, anyDirectoryIgnoresCase(ignoresCaseIn("."), nil))
}

// TestTrackedSpelling covers the rule itself, independent of the filesystem.
// A hard link stands in for a case-insensitive filesystem, where both
// spellings open one file, and ignoresCaseIn for a directory that finds its
// entries ignoring case (worktreedir's tests cover the probe itself).
func TestTrackedSpelling(t *testing.T) {
	t.Parallel()
	dir, worktree := trackedCaseRepo(t)
	cases := worktreedir.NewCaseProbe(dir)
	if cases.IgnoresCase(".") {
		t.Skip("needs a case-sensitive filesystem to tell the cases apart")
	}
	tracked := []string{"README.md"}

	require.Equal(t, "README.md", trackedSpelling(worktree, cases, dir, "README.md", tracked, false), "already git's spelling")
	require.Equal(t, "notes.txt", trackedSpelling(worktree, cases, dir, "notes.txt", nil, false), "not tracked in any case")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked, false), "opens no file")

	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked, false), "another file")

	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Link(filepath.Join(dir, "README.md"), filepath.Join(dir, "readme.md")))
	require.Equal(t, "README.md", trackedSpelling(worktree, ignoresCaseIn("."), dir, "readme.md", tracked, false),
		"opens the same file where the root ignores case")
	require.Equal(t, "readme.md", trackedSpelling(worktree, ignoresCaseIn("."), dir, "readme.md", tracked, true),
		"reported deleted yet still opening a file, as a case-only rename's deletion does: it keeps its spelling")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked, false),
		"where the root respects case, a hard link is another entry, which git tracks apart")

	// A deletion has nothing left to open, so the directory decides. The
	// root respects case: readme.md was another file.
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Remove(filepath.Join(dir, "README.md")))
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked, false), "the root respects case")

	// Where src/ finds its entries ignoring case, src/app.go names git's
	// deleted src/App.go.
	require.NoError(t, os.Remove(filepath.Join(dir, "src", "App.go")))
	app := []string{"src/App.go"}
	require.Equal(t, "src/app.go", trackedSpelling(worktree, cases, dir, "src/app.go", app, false), "src/ respects case here")
	folding := ignoresCaseIn("src")
	require.Equal(t, "src/App.go", trackedSpelling(worktree, folding, dir, "src/app.go", app, false), "deleted under git's spelling")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, folding, dir, "src/app.go", []string{"src/App.go", "src/APP.go"}, false),
		"ambiguous: the index has two spellings")
	require.Equal(t, "Src/app.go", trackedSpelling(worktree, folding, dir, "Src/app.go", app, false), "the root respects case")
	testutil.WriteFile(t, dir, "src/App.go", "back\n")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, folding, dir, "src/app.go", app, false),
		"git's spelling still opens a file, so src/app.go is another one")
}

// TestFilterTrackableFiles_KeepsGitsSpellingWindows checks the transcript
// route a commit hook takes before the first turn end: a path respelled as
// git spells it is kept in that spelling, not dropped for differing from the
// transcript's. CI's Windows job runs it by its name.
func TestFilterTrackableFiles_KeepsGitsSpellingWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	got := filterTrackableFiles(context.Background(), dir, []string{"readme.md", "notes.txt"})
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.Equal(t, []string{"README.md", "notes.txt"}, got)
	} else {
		require.Equal(t, []string{"readme.md", "notes.txt"}, got, "readme.md is another file here")
	}
}

// TestFilterTrackableFiles_SpellsAsTheCommitIndexWindows covers a commit git
// prepares in a temporary index: README.md is staged as renamed to
// readme.md, and a path-limited commit of README.md keeps README.md in the
// index the hook sees. The transcript's README.md must stay as that index,
// and so the commit's staged paths, spell it, not take the worktree index's
// readme.md. CI's Windows job runs it by its name. On a case-sensitive
// filesystem nothing is respelled.
func TestFilterTrackableFiles_SpellsAsTheCommitIndexWindows(t *testing.T) {
	// Not parallel: t.Chdir and t.Setenv.
	dir, _ := trackedCaseRepo(t)
	// The worktree root git reports can be another spelling of dir (a short
	// TEMP path on Windows), which opens a second shared root on it.
	t.Cleanup(osroot.ResetShared)
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	require.NoError(t, err)
	commitIndex := filepath.Join(t.TempDir(), "next-index.lock")
	require.NoError(t, os.WriteFile(commitIndex, index, 0o644))
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	t.Chdir(dir)
	ctx := context.Background()
	root, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	t.Setenv("GIT_INDEX_FILE", commitIndex)

	require.Equal(t, []string{"README.md"}, filterTrackableFiles(ctx, root, []string{"README.md"}))
}

// TestFilterTrackableChanges_KeepsACaseRenamesDeletionWindows covers a
// staged case-only rename of README.md to readme.md. git status reports
// README.md deleted, and on a case-insensitive filesystem README.md still
// opens readme.md. The deletion is of HEAD's README.md, the spelling under
// which a session recorded editing it, so it keeps that spelling. CI's
// Windows job runs it by its name.
func TestFilterTrackableChanges_KeepsACaseRenamesDeletionWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	modified, added, deleted := FilterTrackableChanges(context.Background(), dir, nil, []string{"readme.md"}, []string{"README.md"})
	require.Empty(t, modified)
	require.Equal(t, []string{"readme.md"}, added)
	require.Equal(t, []string{"README.md"}, deleted)
}

// TestSessionHasNewContentFromLiveTranscript_SpellsAsTheCommitWindows covers
// a session, before its first Stop, whose tool hook recorded readme.md: after
// a staged rename of README.md to readme.md, the worktree's index spells it
// so. A path-limited commit of README.md commits README.md, which names the
// same file where the filesystem ignores case, so the session's edit is in
// it. CI's Windows job runs it by its name.
func TestSessionHasNewContentFromLiveTranscript_SpellsAsTheCommitWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID: "spelled-as-commit", WorktreePath: dir,
		AgentType: agent.AgentTypeAntigravity, FilesTouched: []string{"readme.md"},
	}
	got, err := s.sessionHasNewContentFromLiveTranscript(context.Background(), state, fileSet([]string{"README.md"}))
	require.NoError(t, err)
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.True(t, got, "README.md and readme.md name one file here")
	} else {
		require.False(t, got, "README.md is another file here")
	}
}

// TestSessionHasNewContentFromLiveTranscript_SpellsExtractedPathsAsTheCommitWindows
// is TestSessionHasNewContentFromLiveTranscript_SpellsAsTheCommitWindows for
// a session whose edit only its transcript names. Extraction spells the path
// as the worktree's index does, readme.md, and in PostCommit that is not the
// temporary index the commit of README.md was prepared in. CI's Windows job
// runs it by its name.
func TestSessionHasNewContentFromLiveTranscript_SpellsExtractedPathsAsTheCommitWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	written, err := json.Marshal(filepath.Join(dir, "readme.md"))
	require.NoError(t, err)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"human","message":{"content":"edit the readme"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":`+string(written)+`,"content":"edited\n"}}]}}
`), 0o644))
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID: "extracted-as-commit", WorktreePath: dir, Phase: session.PhaseIdle,
		AgentType: agent.AgentTypeClaudeCode, TranscriptPath: transcript,
	}
	got, err := s.sessionHasNewContentFromLiveTranscript(context.Background(), state, fileSet([]string{"README.md"}))
	require.NoError(t, err)
	require.Equal(t, worktreedir.NewCaseProbe(dir).IgnoresCase("."), got,
		"README.md and readme.md name one file where the filesystem ignores case")
}

// TestOverlapSpelledAsCommittedWindows is the post-commit side of
// TestSessionHasNewContentFromLiveTranscript_SpellsAsTheCommitWindows: the
// overlap gate must see the session's readme.md in the commit of README.md
// too, with its recorded hash, or the trailer PrepareCommitMsg added would
// have no checkpoint. CI's Windows job runs it by its name.
func TestOverlapSpelledAsCommittedWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	recorded := []string{"readme.md", "src/App.go"}
	recordedHashes := map[string]string{"readme.md": "abc"}
	files, hashes := overlapSpelledAsCommitted(dir, recorded, recordedHashes, fileSet([]string{"README.md"}))
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.Equal(t, []string{"README.md", "src/App.go"}, files)
		require.Equal(t, "abc", hashes["README.md"], "the hash recorded under the session's spelling")
	} else {
		require.Equal(t, recorded, files, "README.md is another file here")
		require.NotContains(t, hashes, "README.md")
	}
	require.Equal(t, []string{"readme.md", "src/App.go"}, recorded, "the recorded files, which carry-forward uses, are unchanged")
	require.Equal(t, map[string]string{"readme.md": "abc"}, recordedHashes, "and so are their hashes")
}

// TestOverlapChecksSpellAsTheCommitWindows: a turn end recorded the
// session's file as the index spelled it then, readme.md, and a case-only
// rename staged since has the commit spell it README.md. PrepareCommitMsg's
// check of a turn-recorded session against the staged files, and
// PostCommit's count of the sessions that claim the commit, must find the
// session's file in it, as the overlap gate does. CI's Windows job runs it by
// its name.
func TestOverlapChecksSpellAsTheCommitWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	state := &SessionState{
		SessionID: "spelled-claims", WorktreePath: dir, Phase: session.PhaseIdle,
		FilesTouched: []string{"readme.md"}, TouchedFileHashes: map[string]string{"readme.md": "abc"},
	}
	ignoresCase := worktreedir.NewCaseProbe(dir).IgnoresCase(".")
	require.Equal(t, ignoresCase, stagedOverlap(context.Background(), repo, state, []string{"README.md"}),
		"the staged README.md is the session's readme.md where both open one file")
	claimants := 0
	if ignoresCase {
		claimants = 1
	}
	s := &ManualCommitStrategy{}
	require.Equal(t, claimants, s.collectCommittedFileClaims(context.Background(), []*SessionState{state}, fileSet([]string{"README.md"})))
}

// TestCarryForward_KeepsAStagedCaseRenameWindows: after a staged rename of
// README.md to readme.md that the session recorded, a commit of README.md
// takes the edit and leaves the rename staged. The overlap gate counts the
// session's readme.md in that commit, but carry-forward must keep it: the
// next commit, of the rename, is still the session's. CI's Windows job runs
// it by its name. Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_KeepsAStagedCaseRenameWindows(t *testing.T) {
	dir := setupGitRepo(t)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-11-11-staged-case-rename"
	ctx := context.Background()
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "Add README.md")

	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	testutil.WriteFile(t, dir, "readme.md", "readme, edited\n")
	saveTestStep(t, s, dir, sid, "readme.md")
	commitPathsWithTrailer(t, dir, "c3c3c3c3c3c3", "README.md")
	require.Equal(t, "R  README.md -> readme.md", strings.TrimSpace(testutil.RunGit(t, dir, "status", "--porcelain", "--", "README.md", "readme.md")),
		"fixture: the commit took the edit and left the rename staged")
	require.NoError(t, s.PostCommit(ctx))

	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Contains(t, testutil.RunGit(t, dir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName), "c3c3c3c3c3",
		"the commit of README.md condensed the session")
	require.Equal(t, []string{"README.md"}, checkpointFilesTouched(t, "c3c3c3c3c3c3"),
		"the checkpoint lists the edit as the commit spells it")
	require.Contains(t, state.FilesTouched, "readme.md", "the staged rename is still the session's to commit")

	testutil.RunGit(t, dir, "commit", "-q", "-m", "commit the rename")
	require.True(t, filesOverlapWithContent(ctx, state.TouchedFileHashes, headCommit(t, dir), state.FilesTouched),
		"the commit of the rename should link the session")
}

// TestCarryForward_DropsACommittedCaseRenameWindows: the session recorded its
// edit of README.md, and the user commits it with a case-only rename to
// readme.md. The commit removes README.md, which still opens readme.md where
// the filesystem ignores case: carry-forward must take the edit as the
// committed readme.md, not keep the removed name pending for good. CI's
// Windows job runs it by its name. Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_DropsACommittedCaseRenameWindows(t *testing.T) {
	dir := setupGitRepo(t)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-11-11-committed-case-rename"
	ctx := context.Background()
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "Add README.md")

	testutil.WriteFile(t, dir, "README.md", "readme, edited\n")
	saveTestStep(t, s, dir, sid, "README.md")
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	testutil.GitAdd(t, dir, "readme.md")
	commitPathsWithTrailer(t, dir, "d4d4d4d4d4d4")
	require.NoError(t, s.PostCommit(ctx))

	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Contains(t, testutil.RunGit(t, dir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName), "d4d4d4d4d4",
		"the commit of the rename condensed the session")
	require.Empty(t, state.FilesTouched, "the edit and the rename are committed")
	require.False(t, state.HasPendingFiles())
}

// TestCarryForward_DropsACaseRenamedStagedAdditionWindows: the session
// created NEW.md, and the user stages it, renames it to new.md and commits.
// The commit adds new.md and never had NEW.md, which still opens new.md where
// the filesystem ignores case: carry-forward must take the session's file as
// the committed new.md, not keep NEW.md pending for good. CI's Windows job
// runs it by its name. Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_DropsACaseRenamedStagedAdditionWindows(t *testing.T) {
	dir := setupGitRepo(t)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-11-11-case-renamed-addition"
	ctx := context.Background()

	testutil.WriteFile(t, dir, "NEW.md", "new\n")
	saveTestStep(t, s, dir, sid, "NEW.md")
	testutil.GitAdd(t, dir, "NEW.md")
	testutil.RunGit(t, dir, "mv", "-f", "NEW.md", "new.md")
	commitPathsWithTrailer(t, dir, "f6f6f6f6f6f6")
	require.Equal(t, "new.md", strings.TrimSpace(testutil.RunGit(t, dir, "ls-tree", "--name-only", "HEAD", "--", "new.md", "NEW.md")),
		"fixture: the commit added the file as new.md only")
	require.NoError(t, s.PostCommit(ctx))

	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Contains(t, testutil.RunGit(t, dir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName), "f6f6f6f6f6",
		"the commit of new.md condensed the session")
	require.Empty(t, state.FilesTouched, "the session's file is committed as new.md")
	require.False(t, state.HasPendingFiles())
}

// TestCarryForward_KeepsAPendingDeletionOfACaseRenamedAdditionWindows: as
// TestCarryForward_DropsACaseRenamedStagedAdditionWindows, with NEW.md
// recorded without a hash, as a per-tool hook records it, and the file
// removed from the worktree before the commit. The commit adds new.md, whose
// deletion is still pending, so that file stays the session's. Uses t.Chdir —
// do NOT add t.Parallel().
func TestCarryForward_KeepsAPendingDeletionOfACaseRenamedAdditionWindows(t *testing.T) {
	dir := setupGitRepo(t)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-11-11-deleted-case-renamed-addition"
	ctx := context.Background()

	testutil.WriteFile(t, dir, "NEW.md", "new\n")
	saveTestStep(t, s, dir, sid, "NEW.md")
	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	delete(state.TouchedFileHashes, "NEW.md") // as a per-tool hook records it
	require.NoError(t, s.saveSessionState(ctx, state))
	testutil.GitAdd(t, dir, "NEW.md")
	testutil.RunGit(t, dir, "mv", "-f", "NEW.md", "new.md")
	require.NoError(t, os.Remove(filepath.Join(dir, "new.md")))
	commitPathsWithTrailer(t, dir, "a7a7a7a7a7a7")
	require.NoError(t, s.PostCommit(ctx))

	state, err = s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, []string{"new.md"}, state.FilesTouched, "the deletion of the committed new.md is pending")
}

// TestCheckpointTouchesAny_MatchesACaseRenameWindows: after a commit of a
// case-only rename of README.md to readme.md, the checkpoint lists the
// README.md the session recorded, while a task that ran during the commit
// names the readme.md git now tracks. Both open one file where the
// filesystem ignores case, so the task's checkpoint is that one; a deleted
// file matches once both spellings are gone. CI's Windows job runs it by its
// name.
func TestCheckpointTouchesAny_MatchesACaseRenameWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { osroot.Forget(dir) })
	testutil.WriteFile(t, dir, "readme.md", "readme\n")
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("README.md and readme.md are two files on a case-sensitive filesystem")
	}
	require.True(t, checkpointTouchesAny(dir, []string{"README.md"}, []string{"readme.md"}), "both spellings open one file")
	require.True(t, checkpointTouchesAny(dir, []string{"GONE.md"}, []string{"gone.md"}), "both spellings are gone")
	require.False(t, checkpointTouchesAny(dir, []string{"other.md"}, []string{"readme.md"}), "another file")
}

// TestRespelled_DropsConflictingHashes: a session recorded readme.md with an
// older hash and README.md with a newer one, and both now spell one file.
// Neither hash can stand for it: kept, the older one could find older content
// committed and drop the newer edit. A hash moved without a conflict, or
// recorded the same under both spellings, stays.
func TestRespelled_DropsConflictingHashes(t *testing.T) {
	t.Parallel()
	files := []string{"readme.md", "README.md", "src/app.go", "NOTES.md", "notes.md"}
	spelled := []string{"readme.md", "readme.md", "src/app.go", "notes.md", "notes.md"}
	hashes := map[string]string{"readme.md": "older", "README.md": "newer", "src/app.go": "app", "NOTES.md": "same", "notes.md": "same"}
	gotFiles, gotHashes := respelled(files, spelled, hashes)
	require.Equal(t, []string{"readme.md", "src/app.go", "notes.md"}, gotFiles)
	require.NotContains(t, gotHashes, "readme.md", "two hashes recorded for one file")
	require.Equal(t, "app", gotHashes["src/app.go"])
	require.Equal(t, "same", gotHashes["notes.md"], "one hash under both spellings")
	require.Equal(t, "older", hashes["readme.md"], "the recorded hashes are unchanged")

	_, moved := respelled([]string{"README.md"}, []string{"readme.md"}, map[string]string{"README.md": "abc"})
	require.Equal(t, "abc", moved["readme.md"], "a hash moves to the new spelling")

	// A per-tool hook recorded readme.md with no hash: README.md's older hash
	// cannot stand for the newer edit either.
	_, unhashed := respelled([]string{"README.md", "readme.md"}, []string{"readme.md", "readme.md"}, map[string]string{"README.md": "older"})
	require.NotContains(t, unhashed, "readme.md", "one spelling recorded no hash")
}

// TestCarryForward_KeepsAPendingDeletionAfterACaseRenameWindows: a per-tool
// hook recorded the session's edit of README.md with no hash. The user
// stages a case-only rename to readme.md, removes the file and commits the
// rename. Neither spelling opens a file now, but the commit tracks readme.md
// and its deletion is pending: carry-forward must keep the session's record
// as readme.md, as it keeps any file recorded without a hash while the
// worktree differs from the commit, not drop README.md as gone from both.
// CI's Windows job runs it by its name. Uses t.Chdir — do NOT add
// t.Parallel().
func TestCarryForward_KeepsAPendingDeletionAfterACaseRenameWindows(t *testing.T) {
	dir := setupGitRepo(t)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-11-11-pending-deletion-after-case-rename"
	ctx := context.Background()
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "Add README.md")

	testutil.WriteFile(t, dir, "README.md", "readme, edited\n")
	saveTestStep(t, s, dir, sid, "README.md")
	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	delete(state.TouchedFileHashes, "README.md") // as a per-tool hook records it
	require.NoError(t, s.saveSessionState(ctx, state))
	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	testutil.GitAdd(t, dir, "readme.md")
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	commitPathsWithTrailer(t, dir, "e5e5e5e5e5e5")
	require.NoError(t, s.PostCommit(ctx))

	state, err = s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, []string{"readme.md"}, state.FilesTouched, "the deletion of the committed readme.md is pending")
}

// checkpointFilesTouched returns the files_touched the checkpoint
// checkpointID recorded for its first session.
func checkpointFilesTouched(t *testing.T, checkpointID string) []string {
	t.Helper()
	ctx := t.Context()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()
	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(ctx, id.MustCheckpointID(checkpointID), 0)
	require.NoError(t, err)
	return content.Metadata.FilesTouched
}

// On a case-sensitive filesystem FilterTrackableChanges keeps both
// spellings: they are two files.
func TestFilterTrackableChanges_KeepsSpellingsOnCaseSensitiveFilesystem(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("TestTrackedSpellings_CaseInsensitiveFilesystemWindows covers a case-insensitive filesystem")
	}
	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	modified, added, deleted := FilterTrackableChanges(context.Background(), dir, []string{"README.md"}, []string{"readme.md"}, nil)
	require.Equal(t, []string{"README.md"}, modified)
	require.Equal(t, []string{"readme.md"}, added, "a separate file on a case-sensitive filesystem")
	require.Empty(t, deleted)
}
