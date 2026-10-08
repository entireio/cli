package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// releaseStoredCopyForTest releases sessionID's stored copy the way the commit
// path does, for a session recorded in the current worktree.
func releaseStoredCopyForTest(ctx context.Context, sessionID string) {
	state := &SessionState{SessionID: sessionID}
	clearStagedFilesIn(storedSessionRootOrNil(ctx, state), sessionID)
}

// Condensation reads a linked-worktree session's stored copy from that
// worktree (storedSessionRoot), so the release that follows a commit made from
// another worktree must remove that same copy: the commit path releases
// through the root the condensation resolved, not the committing worktree's.
func TestClearStagedFiles_ReleasesLinkedWorktreeCopy(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	mainDir := setupGitRepo(t)
	worktreeDir := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", worktreeDir, "-b", "linked-branch")
	worktreeDir = filepath.FromSlash(strings.TrimSpace(testutil.RunGit(t, worktreeDir, "rev-parse", "--show-toplevel")))
	sessionID := "linked-worktree-release"
	writeStoredCopy(t, worktreeDir, sessionID, "linked")

	t.Chdir(mainDir) // the commit (and its PostCommit) happens in the main worktree
	paths.ClearWorktreeRootCache()
	state := &SessionState{SessionID: sessionID, WorktreePath: worktreeDir}
	clearStagedFilesIn(storedSessionRootOrNil(t.Context(), state), sessionID)

	metadataDir := filepath.Join(worktreeDir, paths.SessionMetadataDirFromSessionID(sessionID))
	assert.NoFileExists(t, filepath.Join(metadataDir, paths.TranscriptFileName),
		"the consumed full.jsonl in the linked worktree is released")
	assert.NoFileExists(t, filepath.Join(metadataDir, paths.PromptFileName))
}
