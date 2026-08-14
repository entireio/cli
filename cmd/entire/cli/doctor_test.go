package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
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

// canonicalCodexHooksJSON returns a hooks.json declaring all four
// canonical Entire-managed events. Tests use this as the "current"
// install baseline so the missing-hooks check passes.
func canonicalCodexHooksJSON() string {
	return `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}]
	}}`
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

[hooks.state."` + hooksPath + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + hooksPath + `:stop:0:0"]
trusted_hash = "sha256:ccc"

[hooks.state."` + hooksPath + `:post_tool_use:0:0"]
trusted_hash = "sha256:ddd"
`
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	cmd, stdout := newTestCmd(t)
	checkCodexHookTrust(cmd)
	require.Contains(t, stdout.String(), "✓ Codex hook trust: OK")
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
	// Trust three of four — PostToolUse is the gap.
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
	require.Contains(t, out, "Codex hook trust: REVIEW NEEDED")
	require.Contains(t, out, "1 hook(s) declared")
	require.Contains(t, out, "- post_tool_use")
	require.Contains(t, out, "Open /hooks inside Codex")
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

	if _, err := (&claudecode.ClaudeCodeAgent{}).InstallHooks(context.Background(), false, false); err != nil {
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
	require.Contains(t, out, "- post_tool_use")
	require.Contains(t, out, "entire enable")
	require.NotContains(t, out, "Codex hook trust: REVIEW NEEDED")
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
