package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// turnOnGlobalEnable gives the test its own user settings file with
// global_enable on, and a caller environment that is a person's, not the
// agent running the suite. Not parallel: t.Setenv.
func turnOnGlobalEnable(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
	for _, name := range review.AgentCallerEnvVars() {
		t.Setenv(name, "")
	}
	settings.ClearOriginKeyCache()
	t.Cleanup(settings.ClearOriginKeyCache)
}

// answerGlobalEnablePushConsentWith makes the consent question answer as
// given, as if typed at a terminal, and counts how often it and the first-push
// policy question are asked.
func answerGlobalEnablePushConsentWith(t *testing.T, yes bool) (consent, policy *int) {
	t.Helper()
	t.Setenv(interactive.EnvTestTTY, "1")
	consent, policy = new(int), new(int)
	previous := askGlobalEnablePushConsent
	askGlobalEnablePushConsent = func(int, string) bool { *consent++; return yes }
	t.Cleanup(func() { askGlobalEnablePushConsent = previous })
	previousPolicy := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer { *policy++; return checkpointPushAnswerNotNow }
	t.Cleanup(func() { askCheckpointPushQuestion = previousPolicy })
	return consent, policy
}

// globalOnlyRefsRepo is a git-refs repository on only through global enable
// with two queued checkpoint refs and an empty origin.
func globalOnlyRefsRepo(t *testing.T) (workDir, bareDir string) {
	t.Helper()
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	turnOnGlobalEnable(t)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)
	return workDir, bareDir
}

func remoteCheckpointRefs(t *testing.T, bareDir string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", "refs/entire/"))
}

func userSettingsFile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv(userdirs.EnvConfigDir), usersettings.FileName))
	require.NoError(t, err)
	return string(data)
}

// Without a terminal to ask in, checkpoints captured only through global
// enable stay queued, the push itself succeeds, and one line says why.
func TestGlobalEnableConsent_HoldsWithoutATerminal(t *testing.T) {
	_, bareDir := globalOnlyRefsRepo(t)
	stderr := captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Empty(t, remoteCheckpointRefs(t, bareDir))
	assert.Contains(t, stderr.String(), "holding 2 session checkpoints")
	outgoing, err := ListOutgoingCheckpoints(t.Context())
	require.NoError(t, err)
	assert.Len(t, outgoing.Checkpoints, 2, "held checkpoints stay queued")
}

// A yes at the terminal is recorded for the repository and this push goes
// ahead, without the first-push policy question after it.
func TestGlobalEnableConsent_YesRecordsAndPushes(t *testing.T) {
	_, bareDir := globalOnlyRefsRepo(t)
	asked, policyAsked := answerGlobalEnablePushConsentWith(t, true)
	captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Equal(t, 1, *asked)
	assert.Zero(t, *policyAsked, "one question per push")
	assert.NotEmpty(t, remoteCheckpointRefs(t, bareDir))
	assert.Contains(t, userSettingsFile(t), `"push_checkpoints": true`)

	s, err := settings.Load(t.Context())
	require.NoError(t, err)
	assert.True(t, s.EnabledOnlyByGlobal(), "consent to push is not a claim")
}

// A no holds, and the question comes back on the next push.
func TestGlobalEnableConsent_NoHolds(t *testing.T) {
	_, bareDir := globalOnlyRefsRepo(t)
	asked, _ := answerGlobalEnablePushConsentWith(t, false)
	stderr := captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Equal(t, 2, *asked)
	assert.Empty(t, remoteCheckpointRefs(t, bareDir))
	assert.Contains(t, stderr.String(), "holding 2 session checkpoints")
}

// An agent's shell can pass the terminal probe, but its yes is not the
// developer's: it is never asked, and the checkpoints are held.
func TestGlobalEnableConsent_AgentSessionIsNotAsked(t *testing.T) {
	_, bareDir := globalOnlyRefsRepo(t)
	asked, _ := answerGlobalEnablePushConsentWith(t, true)
	t.Setenv("CLAUDECODE", "1")
	captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Zero(t, *asked)
	assert.Empty(t, remoteCheckpointRefs(t, bareDir))
}

// Once agreed, every push carries checkpoints without asking.
func TestGlobalEnableConsent_AgreedPushesWithoutAsking(t *testing.T) {
	workDir, bareDir := globalOnlyRefsRepo(t)
	_, err := settings.SetRepoPushConsent(t.Context(), workDir, true)
	require.NoError(t, err)
	asked, _ := answerGlobalEnablePushConsentWith(t, false)
	captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Zero(t, *asked)
	assert.NotEmpty(t, remoteCheckpointRefs(t, bareDir))
}

// The explicit pushes never ask; they report the hold, and the checkpoints
// stay queued.
func TestGlobalEnableConsent_ExplicitPushesReportTheHold(t *testing.T) {
	workDir, bareDir := globalOnlyRefsRepo(t)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	_, _, err = PushQueuedCheckpointRefs(t.Context(), repo, "origin")
	require.ErrorIs(t, err, ErrCheckpointPushHeldForConsent)
	_, _, err = CheckpointPushTarget(t.Context(), "origin")
	require.ErrorIs(t, err, ErrCheckpointPushHeldForConsent)
	_, err = PushCheckpoints(t.Context(), nil)
	require.ErrorIs(t, err, ErrCheckpointPushHeldForConsent)
	assert.Empty(t, remoteCheckpointRefs(t, bareDir))
}

// The git-branch backend is held and released the same way: its v1 branch
// stays local until the developer agrees, then goes with the next push.
func TestGlobalEnableConsent_GitBranchBackend(t *testing.T) {
	dir, _, second := initCountTestRepo(t)
	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	testutil.AddRemote(t, dir, "origin", bareDir)
	testutil.RunGit(t, dir, "push", "origin", "HEAD:refs/heads/main")
	testutil.GitUpdateRef(t, dir, v1LocalRef, second)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	turnOnGlobalEnable(t)
	stderr := captureStderrWriter(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Empty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", v1LocalRef)))
	assert.Contains(t, stderr.String(), "holding")

	answerGlobalEnablePushConsentWith(t, true)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.NotEmpty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", v1LocalRef)))
}
