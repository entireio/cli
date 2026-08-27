package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCmd creates a minimal cobra.Command with captured stdout for testing.
func newTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout
}

// testBaseCommit is a fake commit hash used across classifySession tests.
const testBaseCommit = "abcdef1234567890abcdef1234567890abcdef12"

// createShadowBranchRef creates a shadow branch reference in the repo for
// the given base commit and worktree ID. Uses an empty tree commit.
func createShadowBranchRef(t *testing.T, repo *git.Repository, baseCommit, worktreeID string) {
	t.Helper()

	// Create empty tree
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, emptyTree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	// Create commit
	commitObj := &object.Commit{
		Author:    object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
		Committer: object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
		Message:   "shadow checkpoint",
		TreeHash:  treeHash,
	}
	enc := repo.Storer.NewEncodedObject()
	require.NoError(t, commitObj.Encode(enc))
	commitHash, err := repo.Storer.SetEncodedObject(enc)
	require.NoError(t, err)

	// Create branch reference
	branchName := checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(branchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	require.NoError(t, repo.Storer.SetReference(ref))
}

func TestClassifySession_ActiveStale_NilInteractionTime(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID:           "test-active-nil-time",
		BaseCommit:          testBaseCommit,
		Phase:               session.PhaseActive,
		StepCount:           3,
		LastInteractionTime: nil,
	}

	result := classifySession(state, repo, time.Now())

	require.NotNil(t, result, "active session with nil LastInteractionTime should be stuck")
	assert.Contains(t, result.Reason, "active, started")
	assert.Equal(t, 3, result.CheckpointCount)
	assert.False(t, result.HasShadowBranch)
}

func TestClassifySession_ActiveStale_OldInteractionTime(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	state := &strategy.SessionState{
		SessionID:           "test-active-stale",
		BaseCommit:          testBaseCommit,
		Phase:               session.PhaseActive,
		StepCount:           2,
		LastInteractionTime: &twoHoursAgo,
		FilesTouched:        []string{"file1.go", "file2.go"},
	}

	now := time.Now()
	result := classifySession(state, repo, now)

	require.NotNil(t, result, "active session with old interaction time should be stuck")
	assert.Contains(t, result.Reason, "active, last interaction")
	assert.Equal(t, 2, result.CheckpointCount)
	assert.Equal(t, 2, result.FilesTouchedCount)
}

func TestClassifySession_ActiveRecent_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	fiveMinutesAgo := time.Now().Add(-5 * time.Minute)
	state := &strategy.SessionState{
		SessionID:           "test-active-healthy",
		BaseCommit:          testBaseCommit,
		Phase:               session.PhaseActive,
		StepCount:           1,
		LastInteractionTime: &fiveMinutesAgo,
	}

	result := classifySession(state, repo, time.Now())
	assert.Nil(t, result, "active session with recent interaction should be healthy")
}

func TestClassifySession_EndedWithUncondensedData(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	baseCommit := testBaseCommit
	createShadowBranchRef(t, repo, baseCommit, "")

	state := &strategy.SessionState{
		SessionID:    "test-ended-uncondensed",
		BaseCommit:   baseCommit,
		Phase:        session.PhaseEnded,
		StepCount:    3,
		FilesTouched: []string{"main.go"},
	}

	result := classifySession(state, repo, time.Now())

	require.NotNil(t, result, "ended session with checkpoints and shadow branch should be stuck")
	assert.Equal(t, "ended with uncondensed checkpoint data", result.Reason)
	assert.True(t, result.HasShadowBranch)
	assert.Equal(t, 3, result.CheckpointCount)
	assert.Equal(t, 1, result.FilesTouchedCount)
}

func TestClassifySession_EndedNoShadowBranch_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID:  "test-ended-no-shadow",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  3,
	}

	result := classifySession(state, repo, time.Now())
	assert.Nil(t, result, "ended session without shadow branch should be healthy")
}

// An ENDED record-bearing session has condensable task content that never
// lives on the shadow branch, so branch absence must not classify it healthy.
func TestClassifySession_EndedRecordsOnlyNoShadowBranch_Stuck(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID: "test-ended-records-only", BaseCommit: testBaseCommit, Phase: session.PhaseEnded,
		TaskRecords: []session.TaskRecord{{ToolUseID: "toolu_1", StartedAt: time.Now(), CompletedAt: time.Now()}},
	}

	result := classifySession(state, repo, time.Now())
	require.NotNil(t, result, "ended record-bearing session must be reported even without a shadow branch")
	assert.Equal(t, "ended with uncondensed checkpoint data", result.Reason)
	assert.Equal(t, 1, result.CheckpointCount)
}

// FullyCondensed + leftover live record (pre-fix state or failed sweep capture) is healthy: everything worth keeping is materialized.
func TestClassifySession_EndedFullyCondensedLeftoverRecord_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID: "test-ended-condensed-leftover", BaseCommit: testBaseCommit, Phase: session.PhaseEnded,
		FullyCondensed: true, TaskRecords: []session.TaskRecord{{ToolUseID: "toolu_left", StartedAt: time.Now()}},
	}
	assert.Nil(t, classifySession(state, repo, time.Now()),
		"a FullyCondensed ended session must not be re-flagged for a leftover live record")
}

func TestClassifySession_EndedZeroStepCount_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	baseCommit := "1234567890abcdef1234567890abcdef12345678"
	createShadowBranchRef(t, repo, baseCommit, "")

	state := &strategy.SessionState{
		SessionID:  "test-ended-zero-steps",
		BaseCommit: baseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  0,
	}

	result := classifySession(state, repo, time.Now())
	assert.Nil(t, result, "ended session with zero steps should be healthy even with shadow branch")
}

func TestClassifySession_IdlePhase_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID:  "test-idle",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseIdle,
		StepCount:  1,
	}

	result := classifySession(state, repo, time.Now())
	assert.Nil(t, result, "IDLE session should be healthy")
}

func TestClassifySession_EmptyPhase_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &strategy.SessionState{
		SessionID:  "test-empty-phase",
		BaseCommit: testBaseCommit,
		Phase:      "",
		StepCount:  1,
	}

	result := classifySession(state, repo, time.Now())
	assert.Nil(t, result, "empty phase (backward compat) should be healthy")
}

func TestClassifySession_StalenessThresholdBoundary(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	now := time.Now()

	// Exactly at the threshold — should be stuck (> check, not >=, but let's verify)
	justOverThreshold := now.Add(-session.StuckActiveThreshold - time.Second)
	state := &strategy.SessionState{
		SessionID:           "test-boundary-over",
		BaseCommit:          testBaseCommit,
		Phase:               session.PhaseActive,
		StepCount:           1,
		LastInteractionTime: &justOverThreshold,
	}

	result := classifySession(state, repo, now)
	require.NotNil(t, result, "session just over staleness threshold should be stuck")

	// Just under the threshold — should be healthy
	justUnderThreshold := now.Add(-session.StuckActiveThreshold + time.Minute)
	state2 := &strategy.SessionState{
		SessionID:           "test-boundary-under",
		BaseCommit:          testBaseCommit,
		Phase:               session.PhaseActive,
		StepCount:           1,
		LastInteractionTime: &justUnderThreshold,
	}

	result2 := classifySession(state2, repo, now)
	assert.Nil(t, result2, "session just under staleness threshold should be healthy")
}

func TestClassifySession_ActiveWithShadowBranch(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	baseCommit := testBaseCommit
	createShadowBranchRef(t, repo, baseCommit, "")

	state := &strategy.SessionState{
		SessionID:           "test-active-shadow",
		BaseCommit:          baseCommit,
		Phase:               session.PhaseActive,
		StepCount:           2,
		LastInteractionTime: nil,
	}

	result := classifySession(state, repo, time.Now())

	require.NotNil(t, result)
	assert.True(t, result.HasShadowBranch, "should detect existing shadow branch")
	assert.NotEmpty(t, result.ShadowBranch)
}

func TestClassifySession_WorktreeIDInShadowBranch(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	baseCommit := testBaseCommit
	worktreeID := "my-worktree"
	createShadowBranchRef(t, repo, baseCommit, worktreeID)

	state := &strategy.SessionState{
		SessionID:    "test-worktree-shadow",
		BaseCommit:   baseCommit,
		WorktreeID:   worktreeID,
		Phase:        session.PhaseEnded,
		StepCount:    1,
		FilesTouched: []string{"a.go"},
	}

	result := classifySession(state, repo, time.Now())

	require.NotNil(t, result, "ended session with worktree shadow branch should be stuck")
	assert.True(t, result.HasShadowBranch)
	expectedBranch := checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
	assert.Equal(t, expectedBranch, result.ShadowBranch)
}

// Distinct parentless commits have no common ancestor.
// writeRootlessMetadataCommit builds a metadata commit with an empty tree.
// parents is variadic so a test can build connected history (shared base, two
// children) without a second copy of the go-git plumbing dance; passing none
// yields the rootless commit the disconnection tests want.
func writeRootlessMetadataCommit(t *testing.T, repo *git.Repository, message string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, emptyTree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	commitObj := &object.Commit{
		Author:    object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
		Committer: object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
		Message:   message,
		TreeHash:  treeHash,

		ParentHashes: parents,
	}
	enc := repo.Storer.NewEncodedObject()
	require.NoError(t, commitObj.Encode(enc))
	hash, err := repo.Storer.SetEncodedObject(enc)
	require.NoError(t, err)
	return hash
}

// TestRunSessionsFix_MetadataCheckFailure_PropagatesError verifies that when
// checkDisconnectedMetadata fails, runSessionsFix returns a SilentError so the
// custom stderr message is not printed twice by main.go.
func TestRunSessionsFix_MetadataCheckFailure_PropagatesError(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a real local metadata branch
	localHash := writeRootlessMetadataCommit(t, repo, "metadata")
	localRef := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), localHash)
	require.NoError(t, repo.Storer.SetReference(localRef))

	// Configure an origin remote so origin is a checkpoint read candidate —
	// the metadata check only consults tracking refs of configured candidates.
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")

	// Create a remote-tracking ref that points to a nonexistent object.
	// This makes IsMetadataDisconnected call git merge-base with a bad hash,
	// which fails with a non-0/1 exit code → treated as an error.
	bogusHash := plumbing.NewHash("0000000000000000000000000000000000000001")
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), bogusHash)
	require.NoError(t, repo.Storer.SetReference(remoteRef))

	// Build a minimal cobra command with captured output and context
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err = runSessionsFix(cmd, true)

	// The metadata check error should be propagated, not swallowed.
	// It should be SilentError because the user-facing message was already printed.
	require.Error(t, err, "runSessionsFix should return error when metadata check fails")
	var silentErr *SilentError
	require.ErrorAs(t, err, &silentErr)
	assert.Contains(t, err.Error(), "metadata check failed")
	assert.Contains(t, stderr.String(), "Error: metadata check failed")
}

// Even forced doctor repairs must not advance local state from a legacy tier.
func TestCheckDisconnectedMetadata_NonElectedRemote_ReportOnly(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and IsolateGitConfigEnv (t.Setenv)
	// modify process-global state.
	testutil.IsolateGitConfigEnv(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Local metadata branch and origin's tracking ref share no common
	// ancestor — a genuine disconnection on the NON-elected remote.
	localHash := writeRootlessMetadataCommit(t, repo, "local metadata")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), localHash)))

	remoteHash := writeRootlessMetadataCommit(t, repo, "stale origin metadata")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), remoteHash)))

	// The election picks upstream; origin is only the read-only legacy tier.
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, checkDisconnectedMetadata(cmd, true))

	output := stdout.String()
	assert.Contains(t, output, "Metadata branches: DISCONNECTED",
		"the disconnection must still be reported")
	assert.Contains(t, output, "not the elected checkpoint sync remote",
		"the gate must explain why the repair is withheld")
	assert.NotContains(t, output, "Fixed: metadata branches reconciled",
		"the repair must not run against a non-elected remote")

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, localHash, localRef.Hash(),
		"local v1 must be untouched — origin's stale tracking ref never drives a local-ref rewrite")
}

func TestRunSessionsFix_ForceDiscardOutput_Indented(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	state := &strategy.SessionState{
		SessionID:  "2026-02-02-doctor-output",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseActive,
		StartedAt:  time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runSessionsFix(cmd, true))
	assert.Empty(t, stderr.String())

	output := stdout.String()
	assert.Contains(t, output, "✓ Metadata branches: OK")
	assert.Contains(t, output, "Found 1 stuck session(s):")
	assert.Contains(t, output, "  Session: 2026-02-02-doctor-output")
	assert.Contains(t, output, "  ✓ Discarded session 2026-02-02-doctor-output")

	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Discarded session") {
			assert.True(t, strings.HasPrefix(line, "  ✓ "), "expected nested success line to stay indented: %q", line)
		}
	}
}

// TestRunSessionsFix_NonInteractive_HintsForceInsteadOfPrompting — `entire
// doctor` without --force used to open the stuck-session huh prompt even with
// no TTY to ask on, crashing mid-scan with "bubbletea: could not open TTY" and
// exiting 1. Non-interactive callers (agents, CI) must instead get the
// diagnosis for EVERY stuck session (no early return after the first hint),
// a Fix: line disclosing what --force would do, and the --force hint, with
// the sessions left untouched.
// newTestCmd is not used here because it discards the stderr buffer; this
// test asserts stderr stays empty.
func TestRunSessionsFix_NonInteractive_HintsForceInsteadOfPrompting(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	for _, id := range []string{"2026-08-17-doctor-no-tty", "2026-08-17-doctor-no-tty-2"} {
		state := &strategy.SessionState{
			SessionID:  id,
			BaseCommit: testBaseCommit,
			Phase:      session.PhaseActive,
			StartedAt:  time.Now().Add(-2 * time.Hour),
		}
		require.NoError(t, strategy.SaveSessionState(context.Background(), state))
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runSessionsFix(cmd, false))
	assert.Empty(t, stderr.String())

	output := stdout.String()
	assert.Contains(t, output, "Found 2 stuck session(s):")
	assert.Contains(t, output, "  Session: 2026-08-17-doctor-no-tty")
	assert.Contains(t, output, "  Session: 2026-08-17-doctor-no-tty-2")
	// No shadow branch exists, so --force would discard — the hint must say so.
	assert.Contains(t, output, "  Fix: discard (no condensable checkpoint data).")
	assert.Contains(t, output, "entire doctor --force")
	assert.NotContains(t, output, "Discarded session")
	assert.NotContains(t, output, "Condensed session")

	// The sessions must survive untouched so --force (or an interactive run)
	// can still act on them.
	states, err := strategy.ListSessionStates(context.Background())
	require.NoError(t, err)
	require.Len(t, states, 2)
	ids := []string{states[0].SessionID, states[1].SessionID}
	assert.ElementsMatch(t, []string{"2026-08-17-doctor-no-tty", "2026-08-17-doctor-no-tty-2"}, ids)
}

func TestRunSessionsFix_NonInteractive_TaskContentHintMatchesForceCondense(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	state := &strategy.SessionState{
		SessionID:  "2026-08-20-doctor-task-content",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StartedAt:  time.Now().Add(-2 * time.Hour),
		TaskRecords: []session.TaskRecord{{
			ToolUseID:   "toolu_doctor_task_content",
			StartedAt:   time.Now().Add(-time.Hour),
			CompletedAt: time.Now(),
		}},
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, runSessionsFix(cmd, false))
	assert.Empty(t, stderr.String())

	output := stdout.String()
	assert.Contains(t, output, "  Fix: condense to permanent storage.")
	assert.NotContains(t, output, "  Fix: discard (no condensable checkpoint data).")
}

// Doctor's logging setup must cover the whole command, not just the
// exited-session sweep. With no exited session to finalize — the common case,
// and the one this fixture builds — the sweep returns before it touches logging,
// so the condense and discard handlers that run afterwards would emit structured
// slog lines to slog.Default(), i.e. onto the user's terminal interleaved with
// doctor's own report.
//
// Cannot use t.Parallel(): t.Chdir and the process-global slog default.
func TestRunSessionsFix_HandlerLogsStayOffTheTerminal(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	createShadowBranchRef(t, repo, testBaseCommit, "")

	// Already ended, so never a candidate for the exited-owner sweep, but stuck
	// with uncondensed data — which sends --force down the condensing path, and
	// CondenseSessionByID logs.
	require.NoError(t, strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID:  "2026-02-02-doctor-logging",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  3,
		StartedAt:  time.Now().Add(-2 * time.Hour),
	}))

	// Anything reaching slog.Default() is on the user's terminal in production.
	var fallback bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&fallback, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The root pre-run installs the logger for every command; this stands in for
	// it, since runSessionsFix is called directly rather than through the tree.
	l, logErr := newLogger(context.Background())
	require.NoError(t, logErr)
	t.Cleanup(func() { _ = l.Close() })

	cmd, _ := newTestCmd(t)
	cmd.SetContext(logging.WithLogger(cmd.Context(), l))
	require.NoError(t, runSessionsFix(cmd, true))
	require.NoError(t, l.Close()) // flush before reading the file

	assert.Empty(t, fallback.String(),
		"handler logs went to slog.Default() (the user's terminal) instead of .entire/logs/")

	logged, err := os.ReadFile(filepath.Join(dir, ".entire", "logs", "entire.log"))
	require.NoError(t, err, "doctor did not initialize file logging")
	assert.NotEmpty(t, logged, "nothing was logged, so this test proves nothing about where logs go")
}

// An unwritable .entire/logs is the one Entire failure with no channel of its
// own: the write that would report it is the write being dropped, so it exits 0
// with an empty log and looks exactly like a repo where nothing ran. doctor is
// the command users reach for when a redaction rule seems not to fire, so it has
// to be the one that says so.
func TestCheckLogSink_ReportsUnwritableLogDirectory(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	// A regular file where the directory must go. Chosen over chmod because it
	// fails MkdirAll on Windows too, where the test suite also runs.
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "logs"), []byte("not a directory"), 0o600))

	l, err := newLogger(context.Background())
	require.NoError(t, err, "an unusable log dir must not fail logger construction")
	t.Cleanup(func() { _ = l.Close() })

	cmd, stdout := newTestCmd(t)
	cmd.SetContext(logging.WithLogger(cmd.Context(), l))
	checkLogSink(cmd)

	output := stdout.String()
	assert.Contains(t, output, "Operational logs: NOT WRITABLE")
	assert.Contains(t, output, logging.LogsDir,
		"the report must name the directory to fix")
}

// The check has to be silent on the happy path, or it trains users to skip
// doctor's output — and silent for a repo that never set Entire up, where the
// entry point installs no logger and there is nothing to nag about.
func TestCheckLogSink_SilentWhenWritableOrAbsent(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	t.Run("writable", func(t *testing.T) {
		l, err := newLogger(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() { _ = l.Close() })

		cmd, stdout := newTestCmd(t)
		cmd.SetContext(logging.WithLogger(cmd.Context(), l))
		checkLogSink(cmd)

		assert.Empty(t, stdout.String(), "a writable log directory must produce no output")
	})

	t.Run("no logger installed", func(t *testing.T) {
		cmd, stdout := newTestCmd(t)
		checkLogSink(cmd)

		assert.Empty(t, stdout.String(),
			"a repo where Entire was never set up has no logger and nothing to report")
	})
}

// TestCheckCodexHookTrust_SilentWhenCodexNotInstalled — `entire doctor`
// shouldn't print anything Codex-related when this repo doesn't have
// .codex/hooks.json. Other agents (Claude, Cursor) keep their existing
// quiet behavior; the codex check has to be opt-in by file presence.
func TestCheckCodexHookTrust_SilentWhenCodexNotInstalled(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.NotContains(t, stdout.String(), "Codex hook trust")
}

func TestCheckCodexHookTrust_MalformedAuthorityReportsInvalid(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".codex", "hooks.json"), []byte(`{"hooks":`), 0o600))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	out := stdout.String()
	require.Contains(t, out, "Codex hooks: MALFORMED DISCOVERED CONFIGURATION")
	require.Contains(t, out, resolvedHooksPath(t, dir))
	require.Contains(t, out, "unexpected end of JSON input")
	require.NotContains(t, out, "✓ Codex hooks: INSTALLED")
	require.NotContains(t, out, "Codex hook trust:")
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

func TestCheckCodexHookTrust_SilentForUserOnlyHooks(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".codex"), 0o750))
	userOnly := `{"custom":true,"hooks":{"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"my-user-hook"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".codex", "hooks.json"), []byte(userOnly), 0o600))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.NotContains(t, stdout.String(), "Codex hooks:")
	require.NotContains(t, GetAgentsWithHooksInstalled(context.Background()), agent.AgentNameCodex)
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

// resolvedHooksPath returns the .codex/hooks.json path under dir using the
// symlink-resolved form `git rev-parse --show-toplevel` would return. Test
// fixtures need this because t.TempDir() can produce a /var path while git
// hands back the /private/var equivalent on macOS — divergence between the
// two breaks the trust-state key match the production code uses.
func resolvedHooksPath(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return filepath.Join(resolved, ".codex", "hooks.json")
}

// canonicalCodexHooksJSON returns a hooks.json declaring every canonical
// Entire-managed event. Tests use this as the "current"
// install baseline so the missing-hooks check passes.
func canonicalCodexHooksJSON() string {
	return `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"SessionEnd":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-end","timeout":3}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}],
		"SubagentStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-start","timeout":30}]}],
		"SubagentStop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-stop","timeout":30}]}]
	}}`
}

func writeCodexHooksForDiagnosticTest(t *testing.T, root, contents string) {
	t.Helper()
	projectDir := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(projectDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, codex.HooksFileName), []byte(contents), 0o600))
}

// TestCheckCodexHookTrust_OKWhenAllTrusted prints "✓ Codex hook trust: OK"
// when every event declared in hooks.json has a matching state entry.
func TestCheckCodexHookTrust_OKWhenAllTrusted(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))

	hooksPath := resolvedHooksPath(t, dir)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	configTOML := `[hooks.state."` + hooksPath + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + hooksPath + `:session_end:0:0"]
trusted_hash = "sha256:eee"

[hooks.state."` + hooksPath + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + hooksPath + `:stop:0:0"]
trusted_hash = "sha256:ccc"

[hooks.state."` + hooksPath + `:post_tool_use:0:0"]
trusted_hash = "sha256:ddd"

[hooks.state."` + hooksPath + `:subagent_start:0:0"]
trusted_hash = "sha256:eee"

[hooks.state."` + hooksPath + `:subagent_stop:0:0"]
trusted_hash = "sha256:fff"
`
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "✓ Codex hooks: INSTALLED")
	require.Contains(t, stdout.String(), "✓ Codex hook approval records: PRESENT")
}

// TestCheckCodexHookTrust_ListsMissingEvents prints the gap list when a
// hook event has no corresponding trusted_hash. Pinning the format
// keeps the doctor output script-grep-friendly.
func TestCheckCodexHookTrust_ListsMissingEvents(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))

	hooksPath := resolvedHooksPath(t, dir)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	// Trust all but one — PostToolUse is the gap.
	configTOML := `[hooks.state."` + hooksPath + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + hooksPath + `:session_end:0:0"]
trusted_hash = "sha256:eee"

[hooks.state."` + hooksPath + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + hooksPath + `:stop:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)

	out := stdout.String()
	require.Contains(t, out, "Codex hook trust: REVIEW NEEDED")
	// The fixture trusts session_start/user_prompt_submit/stop, so the remaining
	// three declared events are untrusted. Codex refuses to run untrusted hooks, so
	// each one named here is a hook that silently would not fire.
	require.Contains(t, out, "3 installed hook(s)")
	require.Contains(t, out, "- post_tool_use")
	require.Contains(t, out, "- subagent_start")
	require.Contains(t, out, "- subagent_stop")
	require.Contains(t, out, "Open /hooks inside Codex")
}

func TestCheckCodexHookTrust_UnknownWhenApprovalRecordsUnreadable(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".codex", "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "✓ Codex hooks: INSTALLED")
	require.Contains(t, stdout.String(), "Codex hook trust: UNKNOWN")
	require.Contains(t, stdout.String(), "review their active state")
}

func TestCheckCodexHookTrust_LinkedWorktreeReportsInactiveCurrentWorktreeFile(t *testing.T) {
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linkedRoot, ".codex", "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	out := stdout.String()
	require.Contains(t, out, "Codex hooks: NOT ACTIVE IN THIS WORKTREE")
	require.Contains(t, out, resolvedHooksPath(t, linkedRoot))
	require.Contains(t, out, resolvedHooksPath(t, repoRoot))
	require.Contains(t, out, "Codex will read the discovered file above, not the current-worktree file above")
	require.Contains(t, out, ".codex/hooks.json is tracked — commit it and make sure the root worktree has it")
	require.Contains(t, out, "(merge to the default branch, or check that branch out there).")
	require.NotContains(t, out, "If that root is a Git checkout")
	require.NotContains(t, out, "In a .bare layout")
	require.NotContains(t, out, "migrate")
	require.Contains(t, GetAgentsWithHooksInstalled(context.Background()), agent.AgentNameCodex)
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

// TestCheckCodexHookTrust_BareWorktreeReportsActiveRootHooks verifies the
// healthy state when Codex discovers the layout root's project hooks.
func TestCheckCodexHookTrust_BareWorktreeReportsActiveRootHooks(t *testing.T) {
	tmp, layoutRoot, linkedRoot := setupBareRepoForDoctorTest(t)
	ag := &codex.CodexAgent{}
	t.Chdir(layoutRoot)
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	t.Chdir(linkedRoot)
	_, err = ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	out := stdout.String()
	require.Contains(t, out, "✓ Codex hooks: ACTIVE (via root checkout)")
	require.Contains(t, out, resolvedHooksPath(t, layoutRoot))
	require.NotContains(t, out, "CURRENT-WORKTREE FILE NOT DISCOVERED")
	require.NotContains(t, out, "run `entire doctor`")
}

func TestCheckCodexHookTrust_InvalidWorktreePrecedesDiscoveredHooks(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("directory symlinks require privileges on Windows")
	}

	for _, test := range []struct {
		name       string
		discovered string
	}{
		{name: "healthy discovered hooks", discovered: canonicalCodexHooksJSON()},
		{name: "invalid discovered hooks", discovered: "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
			writeCodexHooksForDiagnosticTest(t, repoRoot, test.discovered)
			require.NoError(t, os.Symlink(filepath.Join(repoRoot, ".codex"), filepath.Join(linkedRoot, ".codex")))
			t.Chdir(linkedRoot)
			t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

			cmd, stdout := newTestCmd(t)
			checkCodexHookTrust(cmd)
			out := stdout.String()
			if test.discovered == "{" {
				require.Contains(t, out, "Codex hooks: MALFORMED DISCOVERED CONFIGURATION")
			} else {
				require.Contains(t, out, "Codex hooks: PROJECT LAYER MISSING")
			}
			if test.discovered == "{" {
				require.Contains(t, out, resolvedHooksPath(t, repoRoot))
			} else {
				require.Contains(t, out, filepath.Dir(resolvedHooksPath(t, linkedRoot))+" (missing)")
			}
			require.NotContains(t, out, "INVALID CURRENT-WORKTREE CONFIGURATION")
			require.NotContains(t, out, "✓ Codex hooks: INSTALLED")
			require.NotContains(t, out, "Codex hook trust: REVIEW NEEDED")
		})
	}
}

func TestCheckCodexHookTrust_SecondWorktreeLocalCopyRemainsLocal(t *testing.T) {
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))
	ag := &codex.CodexAgent{}

	t.Chdir(repoRoot)
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Positive(t, count)

	legacyPath := filepath.Join(linkedRoot, ".codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0o750))
	require.NoError(t, os.WriteFile(legacyPath, []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(linkedRoot)
	require.Equal(t, agent.HooksOutdated, ag.CheckHookConfig(context.Background()))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "✓ Codex hooks: ACTIVE (via root checkout)")
	require.NotContains(t, stdout.String(), "remove")

	count, err = ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Positive(t, count)
	require.FileExists(t, legacyPath)
	require.Equal(t, agent.HooksCurrent, ag.CheckHookConfig(context.Background()))
}

func TestCheckCodexHookTrust_LinkedWorktreeReadsAuthoritativeMissingHooks(t *testing.T) {
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))
	stale := `{"hooks":{
  "SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start"}]}],
  "UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit"}]}],
  "Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop"}]}]
}}`
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".codex", "hooks.json"), []byte(stale), 0o600))
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "Codex hooks: OUT OF DATE")
	require.Contains(t, stdout.String(), "- post_tool_use")
	require.NotContains(t, stdout.String(), "MISPLACED")
}

func TestCheckCodexHookTrust_LinkedWorktreeReportsMissingProjectLayer(t *testing.T) {
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".codex", "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	out := stdout.String()
	require.Contains(t, out, "Codex hooks: PROJECT LAYER MISSING")
	require.Contains(t, out, filepath.Dir(resolvedHooksPath(t, linkedRoot))+" (missing)")
	require.Contains(t, out, resolvedHooksPath(t, repoRoot))
	require.Contains(t, out, ".codex/hooks.json is tracked — commit it and make sure the root worktree has it")
	require.NotContains(t, out, "In a .bare layout")
	require.NotContains(t, GetAgentsWithHooksInstalled(context.Background()), agent.AgentNameCodex)
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

func TestCheckCodexHookTrust_LinkedSubmoduleUsesCurrentWorktreeFallback(t *testing.T) {
	linkedSubmoduleRoot := setupLinkedSubmoduleForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(linkedSubmoduleRoot, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linkedSubmoduleRoot, ".codex", "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(linkedSubmoduleRoot)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "Codex hooks: OUT OF DATE")
	require.NotContains(t, stdout.String(), "Codex hooks: UNRESOLVED")
	require.Contains(t, GetAgentsWithHooksInstalled(context.Background()), agent.AgentNameCodex)
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

func TestCheckCodexHookTrust_CodexHomeCollisionReportsUnsupported(t *testing.T) {
	_, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linkedRoot, ".codex", "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(repoRoot, ".codex"))

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "Codex hooks: UNRESOLVED")
	require.Contains(t, stdout.String(), "user-wide")
	require.NotContains(t, OutdatedHookAgents(context.Background()), agent.AgentNameCodex)
}

func TestSetupAgentHooks_UsesCurrentCheckoutWhenCodexDiscoveryIsUnresolved(t *testing.T) {
	_, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(repoRoot, ".codex"))
	ag := &codex.CodexAgent{}

	installed, err := setupAgentHooks(context.Background(), ag, false)
	require.NoError(t, err)
	require.Equal(t, 7, installed)
	require.FileExists(t, filepath.Join(linkedRoot, ".codex", "hooks.json"))
	require.NoFileExists(t, filepath.Join(repoRoot, ".codex", "hooks.json"))
}

func TestCheckCodexHookTrust_ResolverFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".codex", "hooks.json"), 0o750))
	t.Chdir(dir)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "Codex hooks: UNRESOLVED")
	require.Contains(t, stdout.String(), "Git layout could not be resolved")
}

func setupLinkedRepoForDoctorTest(t *testing.T) (tmp, repoRoot, linkedRoot string) {
	t.Helper()
	tmp = t.TempDir()
	repoRoot = filepath.Join(tmp, "repo")
	linkedRoot = filepath.Join(tmp, "linked")
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
	runGitForDoctorTest(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	return tmp, repoRoot, linkedRoot
}

func setupBareRepoForDoctorTest(t *testing.T) (tmp, layoutRoot, linkedRoot string) {
	t.Helper()
	tmp = setupTestDir(t)
	seedRoot := filepath.Join(tmp, "seed")
	layoutRoot = filepath.Join(tmp, "layout")
	bareRoot := filepath.Join(layoutRoot, ".bare")
	mainRoot := filepath.Join(layoutRoot, "main")
	linkedRoot = filepath.Join(layoutRoot, "feature")

	testutil.InitRepo(t, seedRoot)
	testutil.WriteFile(t, seedRoot, "README.md", "initial\n")
	testutil.GitAdd(t, seedRoot, "README.md")
	testutil.GitCommit(t, seedRoot, "initial")
	require.NoError(t, os.MkdirAll(layoutRoot, 0o750))
	runGitForDoctorTest(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	require.NoError(t, os.WriteFile(filepath.Join(layoutRoot, ".git"), []byte("gitdir: ./.bare\n"), 0o600))
	runGitForDoctorTest(t, tmp, "--git-dir", bareRoot, "worktree", "add", mainRoot)
	runGitForDoctorTest(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", linkedRoot)
	return tmp, layoutRoot, linkedRoot
}

func setupLinkedSubmoduleForDoctorTest(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	subjectRoot := filepath.Join(tmp, "subject")
	superRoot := filepath.Join(tmp, "super")
	submoduleRoot := filepath.Join(superRoot, "sub")
	linkedSubmoduleRoot := filepath.Join(tmp, "linked-sub")
	for _, repoRoot := range []string{subjectRoot, superRoot} {
		testutil.InitRepo(t, repoRoot)
		testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
		testutil.GitAdd(t, repoRoot, "README.md")
		testutil.GitCommit(t, repoRoot, "initial")
	}
	runGitForDoctorTest(t, superRoot, "-c", "protocol.file.allow=always", "submodule", "add", subjectRoot, "sub")
	testutil.GitAdd(t, superRoot, ".gitmodules", "sub")
	testutil.GitCommit(t, superRoot, "add submodule")
	runGitForDoctorTest(t, submoduleRoot, "worktree", "add", "-b", "linked", linkedSubmoduleRoot)
	return linkedSubmoduleRoot
}

func runGitForDoctorTest(t *testing.T, repoRoot string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repoRoot}, args...)
	gitCmd := exec.CommandContext(t.Context(), "git", commandArgs...)
	gitCmd.Dir = repoRoot
	gitCmd.Env = testutil.GitIsolatedEnv()
	output, err := gitCmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

// TestCheckHookDrift_SilentWhenNotInstalled — the generalized drift check
// prints nothing Claude-Code-related when this repo has no Entire hooks
// installed.
func TestCheckHookDrift_SilentWhenNotInstalled(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	cmd, stdout := newTestCmd(t)
	checkHookDrift(cmd)
	require.NotContains(t, stdout.String(), "Claude Code hook")
}

// TestCheckHookDrift_ClaudeCodeOKWhenCurrent — a fresh Claude Code install
// writes the current matchers, so the drift check reports OK.
func TestCheckHookDrift_ClaudeCodeOKWhenCurrent(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	if _, err := (&claudecode.ClaudeCodeAgent{}).InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	cmd, stdout := newTestCmd(t)
	checkHookDrift(cmd)
	require.Contains(t, stdout.String(), "✓ Claude Code hook config: OK")
}

// TestCheckHookDrift_ClaudeCodeWarnsWhenOutdated — a Claude Code config left by
// an older CLI (hooks under the stale Task/TodoWrite matchers) is reported OUT
// OF DATE with the --force fix hint.
func TestCheckHookDrift_ClaudeCodeWarnsWhenOutdated(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	stale := `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}],
    "PreToolUse": [{"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks claude-code pre-task"}]}],
    "PostToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks claude-code post-task"}]},
      {"matcher": "TodoWrite", "hooks": [{"type": "command", "command": "entire hooks claude-code post-todo"}]}
    ]
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, claudecode.ClaudeSettingsFileName), []byte(stale), 0o600))

	cmd, stdout := newTestCmd(t)
	checkHookDrift(cmd)

	out := stdout.String()
	require.Contains(t, out, "Claude Code hooks: OUT OF DATE")
	require.Contains(t, out, "entire enable --force")
}

// TestCheckCodexHookTrust_FlagsStaleHooksFile — user enabled Codex on
// an older release that didn't ship PostToolUse. Their hooks.json has
// only the three legacy events. Doctor must surface the gap and tell
// them to re-run `entire enable`.
func TestCheckCodexHookTrust_FlagsStaleHooksFile(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	// The install set of an older release: no PostToolUse, and no SessionEnd
	// either — both post-date it.
	staleHooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}]
	}}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(staleHooksJSON), 0o600))

	hooksPath := resolvedHooksPath(t, dir)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	// Trust the three legacy events so the trust check itself stays quiet —
	// only the stale-file finding should fire.
	configTOML := `[hooks.state."` + hooksPath + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + hooksPath + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + hooksPath + `:stop:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)

	out := stdout.String()
	require.Contains(t, out, "Codex hooks: OUT OF DATE")
	require.Contains(t, out, "- session_end")
	require.Contains(t, out, "- post_tool_use")
	require.Contains(t, out, "entire enable")
	// Trust stays quiet: every event the stale file actually declares is trusted,
	// so only the out-of-date finding should fire.
	require.NotContains(t, out, "Codex hook trust: REVIEW NEEDED")
}

// antigravityHooksJSON returns a minimal .agents/hooks.json declaring the
// Entire PreInvocation hook, enough for AreHooksInstalled to report true.
func antigravityHooksJSON() string {
	return `{"entire":{"PreInvocation":[{"type":"command","command":"entire hooks antigravity pre-invocation"}]}}`
}

// stubAgyOnPath prepends a directory containing a fake executable `agy` to
// PATH so the doctor check's binary-presence guard passes deterministically.
func stubAgyOnPath(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "agy")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCheckAntigravityTitleTee_SilentWhenAgyNotInstalled stays quiet for
// developers who don't use agy at all: .agents/hooks.json is committable, so
// a teammate's checkout can have Antigravity hooks "installed" on a machine
// with no agy binary — warning there (and suggesting a repair that writes
// agy's global settings) is a false positive.
func TestCheckAntigravityTitleTee_SilentWhenAgyNotInstalled(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	agentsDir := filepath.Join(dir, ".agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "hooks.json"),
		[]byte(antigravityHooksJSON()), 0o600))
	t.Setenv("ENTIRE_ANTIGRAVITY_CONFIG_DIR", filepath.Join(t.TempDir(), "agy"))
	t.Setenv("PATH", t.TempDir()) // no agy binary anywhere on PATH

	cmd, stdout := newTestCmd(t)
	checkAntigravityTitleTee(cmd)
	require.NotContains(t, stdout.String(), "Antigravity title-tee")
}

// TestCheckAntigravityTitleTee_SilentWhenHooksNotInstalled stays quiet when
// the repo has no Antigravity hooks — nothing to check.
func TestCheckAntigravityTitleTee_SilentWhenHooksNotInstalled(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	t.Setenv("ENTIRE_ANTIGRAVITY_CONFIG_DIR", filepath.Join(t.TempDir(), "agy"))

	cmd, stdout := newTestCmd(t)
	checkAntigravityTitleTee(cmd)
	require.NotContains(t, stdout.String(), "Antigravity title-tee")
}

// TestCheckAntigravityTitleTee_OKWhenConfigured reports OK when hooks are
// installed and agy's title slot routes through the title-tee shim.
func TestCheckAntigravityTitleTee_OKWhenConfigured(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	stubAgyOnPath(t)

	agentsDir := filepath.Join(dir, ".agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "hooks.json"),
		[]byte(antigravityHooksJSON()), 0o600))

	cfgDir := filepath.Join(t.TempDir(), "agy")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "settings.json"),
		[]byte(`{"title":{"type":"command","command":"entire hooks antigravity title-tee"}}`), 0o600))
	t.Setenv("ENTIRE_ANTIGRAVITY_CONFIG_DIR", cfgDir)

	cmd, stdout := newTestCmd(t)
	checkAntigravityTitleTee(cmd)
	require.Contains(t, stdout.String(), "✓ Antigravity title-tee: OK")
}

// TestCheckAntigravityTitleTee_WarnsWhenNotConfigured surfaces the missing
// token-usage surface when hooks are installed but the title slot is unclaimed.
func TestCheckAntigravityTitleTee_WarnsWhenNotConfigured(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	stubAgyOnPath(t)

	agentsDir := filepath.Join(dir, ".agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "hooks.json"),
		[]byte(antigravityHooksJSON()), 0o600))

	// Empty agy config dir — no title slot claimed.
	t.Setenv("ENTIRE_ANTIGRAVITY_CONFIG_DIR", filepath.Join(t.TempDir(), "agy"))

	cmd, stdout := newTestCmd(t)
	checkAntigravityTitleTee(cmd)

	out := stdout.String()
	require.Contains(t, out, "Antigravity title-tee: NOT CONFIGURED")
	require.Contains(t, out, "token counts")
	require.Contains(t, out, "entire agent add antigravity")
}

// TestConfirmDoctorFix_CancelledContext verifies that a cancelled command
// context makes the confirm prompt return (false, nil) rather than surfacing a
// wrapped error — doctor fixes are skipped cleanly on interrupt.
func TestConfirmDoctorFix_CancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before prompting

	var out bytes.Buffer
	proceed, err := confirmDoctorFix(ctx, &out, "Apply fix?")
	require.NoError(t, err)
	assert.False(t, proceed)
}

// TestConfirmDoctorFix_NonInteractive_DeclinesWithoutPrompt — the disconnected
// metadata check used to call confirmDoctorFix unguarded, so headless runs
// (agents, CI) crashed opening /dev/tty ("could not open TTY"). Under `go
// test` the environment is non-interactive by default, so the prompt must
// decline cleanly with (false, nil) and print nothing.
func TestConfirmDoctorFix_NonInteractive_DeclinesWithoutPrompt(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	proceed, err := confirmDoctorFix(context.Background(), &out, "Apply fix?")
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Empty(t, out.String())
}

// setupDivergedMetadata points local v1 and origin's tracking ref at two
// different children of one base commit, and returns both tips.
func setupDivergedMetadata(t *testing.T, repo *git.Repository) (local, remote plumbing.Hash) {
	t.Helper()
	base := writeRootlessMetadataCommit(t, repo, "shared base")
	local = writeRootlessMetadataCommit(t, repo, "local checkpoint", base)
	remote = writeRootlessMetadataCommit(t, repo, "remote checkpoint", base)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), local)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), remote)))
	return local, remote
}

func runCheckDisconnectedMetadata(t *testing.T) string {
	t.Helper()
	cmd, stdout := newTestCmd(t)
	require.NoError(t, checkDisconnectedMetadata(cmd, true))
	return stdout.String()
}

// TestCheckDisconnectedMetadata_Diverged_ReportedAsSelfHealing covers the state
// that previously printed a bare "OK": local and remote both advanced, so the
// next fetch rewrites the local ref by replaying local commits. Nothing else
// surfaces that, since the replay itself only logs.
func TestCheckDisconnectedMetadata_Diverged_ReportedAsSelfHealing(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and IsolateGitConfigEnv modify globals.
	testutil.IsolateGitConfigEnv(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	local, remote := setupDivergedMetadata(t, repo)

	// origin is the elected remote here, so the replay really will happen.
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")

	output := runCheckDisconnectedMetadata(t)

	assert.Contains(t, output, "Metadata branches: DIVERGED")
	assert.NotContains(t, output, "DISCONNECTED", "shared ancestry is not a disconnection")
	assert.Contains(t, output, local.String()[:12], "the local tip must be shown")
	assert.Contains(t, output, remote.String()[:12], "the remote tip must be shown")
	assert.Contains(t, output, "No action needed", "divergence against the elected remote self-heals")
	assert.Contains(t, output, "new hashes", "the user must learn the replayed commits are re-hashed")

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, local, localRef.Hash(), "reporting must not move any ref")
}

// TestCheckDisconnectedMetadata_Diverged_LegacyTierWontReconcile is the other
// half: the confinement rule means a diverged legacy-tier ref never advances the
// local ref, so promising self-healing there would be a lie.
func TestCheckDisconnectedMetadata_Diverged_LegacyTierWontReconcile(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	setupDivergedMetadata(t, repo)

	// Election picks upstream; origin carries the diverged tracking ref.
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	output := runCheckDisconnectedMetadata(t)

	assert.Contains(t, output, "Metadata branches: DIVERGED")
	assert.Contains(t, output, "legacy read tier")
	assert.Contains(t, output, "Nothing will reconcile this on its own.")
	assert.NotContains(t, output, "No action needed",
		"a legacy-tier divergence does not self-heal; saying so would be wrong")
}

// TestCheckDisconnectedMetadata_Aligned_StaysQuiet pins that the common case is
// unchanged — the divergence check must not add noise to a healthy repo.
func TestCheckDisconnectedMetadata_Aligned_StaysQuiet(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	tip := writeRootlessMetadataCommit(t, repo, "agreed metadata")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), tip)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), tip)))
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")

	output := runCheckDisconnectedMetadata(t)

	assert.Contains(t, output, "✓ Metadata branches: OK")
	assert.NotContains(t, output, "DIVERGED")
}
