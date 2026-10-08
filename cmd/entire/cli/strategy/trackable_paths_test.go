package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mid-turn commit of an ACTIVE session with no turn-end step yet takes its
// files from the transcript (resolveFilesTouched), and carry-forward writes
// what remains into FilesTouched. A gitignored file or a submodule path the
// transcript names must not survive that route either: neither can ever be
// committed, so it would keep the session pending forever.
// Uses t.Chdir — do NOT add t.Parallel().
func TestPostCommit_MidTurnCarryForward_DropsIgnoredAndSubmodulePaths(t *testing.T) {
	dir, worktreePath := setupIgnoreAndSubmoduleRepo(t)
	for _, name := range []string{"committed.txt", "pending.txt", "ignored.env"} {
		testutil.WriteFile(t, dir, name, name+"\n")
	}
	s := &ManualCommitStrategy{}
	sessionID := "test-midturn-untrackable"
	saveMidTurnSession(t, s, dir, worktreePath, sessionID, "committed.txt", "pending.txt", "ignored.env", "sub")

	testutil.GitAdd(t, dir, "committed.txt")
	testutil.GitCommit(t, dir, "commit one file\n\n"+trailers.CheckpointTrailerKey+": "+"ef12ab34cd56")
	require.NoError(t, s.PostCommit(context.Background()))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Contains(t, state.FilesTouched, "pending.txt", "fixture: carry-forward ran for the uncommitted file")
	assert.NotContains(t, state.FilesTouched, "ignored.env")
	assert.NotContains(t, state.FilesTouched, "sub")
}

// A path inside a submodule is the nested repository's own work, and git
// refuses to ignore-check it ("Pathspec is in submodule"), which used to fail
// the whole check-ignore batch and keep every path in it, gitignored ones
// included. After the only superproject file is committed, nothing may stay
// pending. Uses t.Chdir — do NOT add t.Parallel().
func TestPostCommit_MidTurnCarryForward_DropsPathsInsideSubmodule(t *testing.T) {
	dir, worktreePath := setupIgnoreAndSubmoduleRepo(t)
	testutil.WriteFile(t, dir, "agent.txt", "agent\n")
	testutil.WriteFile(t, dir, "ignored.env", "secret\n")
	testutil.WriteFile(t, dir, "sub/lib.txt", "v2\n")
	s := &ManualCommitStrategy{}
	sessionID := "test-midturn-inside-submodule"
	saveMidTurnSession(t, s, dir, worktreePath, sessionID, "agent.txt", "ignored.env", "sub/lib.txt")

	testutil.GitAdd(t, dir, "agent.txt")
	testutil.GitCommit(t, dir, "commit agent file\n\n"+trailers.CheckpointTrailerKey+": "+"ab12cd34ef56")
	require.NoError(t, s.PostCommit(context.Background()))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.FilesTouched, "neither the gitignored file nor the submodule's file is superproject work")
}

// setupIgnoreAndSubmoduleRepo creates a repository that ignores ignored.env
// and has a real submodule at sub, chdirs into it, and returns its directory
// and worktree root.
func setupIgnoreAndSubmoduleRepo(t *testing.T) (dir, worktreePath string) {
	t.Helper()
	dir = setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	testutil.WriteFile(t, dir, ".gitignore", "ignored.env\n")
	testutil.GitAdd(t, dir, ".gitignore")
	testutil.GitCommit(t, dir, "ignore rules")

	subSrc := t.TempDir()
	testutil.InitRepo(t, subSrc)
	testutil.WriteFile(t, subSrc, "lib.txt", "v1\n")
	testutil.GitAdd(t, subSrc, "lib.txt")
	testutil.GitCommit(t, subSrc, "lib v1")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	testutil.RunGit(t, dir, "commit", "-q", "-m", "add submodule")

	worktreePath, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	return dir, worktreePath
}

// saveMidTurnSession saves an ACTIVE Claude Code session with no turn-end step
// whose transcript writes each of files, so a commit takes its files from the
// transcript.
func saveMidTurnSession(t *testing.T, s *ManualCommitStrategy, dir, worktreePath, sessionID string, files ...string) {
	t.Helper()
	writeLine := func(path string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` +
			filepath.Join(worktreePath, path) + `","content":"x"}}]}}` + "\n"
	}
	var transcript strings.Builder
	transcript.WriteString(`{"type":"human","message":{"content":"write files"}}` + "\n")
	for _, file := range files {
		transcript.WriteString(writeLine(file))
	}
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript.String()), 0o644))
	stale := time.Now().Add(-3 * time.Minute)
	require.NoError(t, os.Chtimes(transcriptPath, stale, stale))

	now := time.Now()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	require.NoError(t, err)
	require.NoError(t, s.saveSessionState(context.Background(), &SessionState{
		SessionID:           sessionID,
		BaseCommit:          testutil.GetHeadHash(t, dir),
		WorktreePath:        worktreePath,
		WorktreeID:          worktreeID,
		StartedAt:           now,
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
	}))
}

// A path beneath a symlinked directory is refused by check-ignore ("beyond a
// symbolic link") and dropped; the gitignored path in the same batch is still
// dropped and an ordinary one kept. Uses t.Chdir — do NOT add t.Parallel().
func TestFilterTrackableChanges_DropsPathBeneathSymlinkedDirectory(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	dir := setupGitRepo(t)
	t.Chdir(dir)
	testutil.WriteFile(t, dir, ".gitignore", "ignored.env\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o755))
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "linkdir")))
	testutil.WriteFile(t, dir, "real/x.txt", "x\n")

	kept, _, _ := FilterTrackableChanges(context.Background(), dir,
		[]string{"agent.txt", "ignored.env", "linkdir/x.txt"}, nil, nil)
	assert.Equal(t, []string{"agent.txt"}, kept)
}

// Deleted paths are never ignore-checked (git check-ignore reports a path
// whose deletion is staged); modified and added ones are. Uses t.Chdir — do
// NOT add t.Parallel().
func TestFilterTrackableChanges_IgnoreRulesSkipDeletions(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	testutil.WriteFile(t, dir, ".gitignore", "*.log\n")
	testutil.WriteFile(t, dir, "tracked.log", "x\n")
	testutil.GitAdd(t, dir, ".gitignore")
	testutil.GitAddForce(t, dir, "tracked.log")
	testutil.GitCommit(t, dir, "track a log")
	testutil.RunGit(t, dir, "rm", "-q", "tracked.log")

	modified, added, deleted := FilterTrackableChanges(context.Background(), dir,
		[]string{"src.go", "debug.log"}, []string{"new.log", "new.go"}, []string{"tracked.log"})
	assert.Equal(t, []string{"src.go"}, modified)
	assert.Equal(t, []string{"new.go"}, added)
	assert.Equal(t, []string{"tracked.log"}, deleted)
}

// FilterTrackableChanges asks git about a path once per process: a commit hook
// filters the transcript files of every session in the worktree, and the
// answers cannot change within one hook. The test proves the second call is
// served from the cache by removing the ignore rule between the calls — git
// would now answer "not ignored" — and checking the first answer still holds.
// Not parallel: the cache is process-global and the test resets it.
func TestFilterTrackableChanges_CachesAnswersPerProcess(t *testing.T) {
	dir := setupGitRepo(t)
	resetTrackablePathCacheForTesting()
	t.Cleanup(resetTrackablePathCacheForTesting)
	testutil.WriteFile(t, dir, ".gitignore", "ignored.env\n")
	ctx := context.Background()

	kept, _, _ := FilterTrackableChanges(ctx, dir, []string{"ignored.env", "src.go"}, nil, nil)
	require.Equal(t, []string{"src.go"}, kept)

	require.NoError(t, os.Remove(filepath.Join(dir, ".gitignore")))
	kept, _, _ = FilterTrackableChanges(ctx, dir, []string{"ignored.env", "src.go"}, nil, nil)
	assert.Equal(t, []string{"src.go"}, kept, "the second call is answered from the cache without asking git")

	resetTrackablePathCacheForTesting()
	kept, _, _ = FilterTrackableChanges(ctx, dir, []string{"ignored.env", "src.go"}, nil, nil)
	assert.Equal(t, []string{"ignored.env", "src.go"}, kept, "after a reset git is asked again")
}

// A submodule that is added but not yet committed is a gitlink only in the
// index. It is still found, and a path beneath it is dropped. Uses t.Chdir —
// do NOT add t.Parallel().
func TestFilterTrackableChanges_DropsPathsInsideStagedSubmodule(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	subSrc := t.TempDir()
	testutil.InitRepo(t, subSrc)
	testutil.WriteFile(t, subSrc, "lib.txt", "v1\n")
	testutil.GitAdd(t, subSrc, "lib.txt")
	testutil.GitCommit(t, subSrc, "lib v1")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	testutil.WriteFile(t, dir, "sub/lib.txt", "v2\n")

	found, err := gitrepo.GitlinkPaths(context.Background(), dir, []string{"sub"})
	require.NoError(t, err)
	assert.Contains(t, found, "sub", "the staged-only submodule is a gitlink")

	kept, _, _ := FilterTrackableChanges(context.Background(), dir, []string{"agent.txt", "sub/lib.txt"}, nil, nil)
	assert.Equal(t, []string{"agent.txt"}, kept)
}
