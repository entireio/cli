package strategy

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerFirstPushQuestionWith makes the first-push question return answer, as
// if typed at the terminal: under test the real question reads nothing.
func answerFirstPushQuestionWith(t *testing.T, answer checkpointPushQuestionAnswer) {
	t.Helper()
	t.Setenv(interactive.EnvTestTTY, "1")
	previous := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer { return answer }
	t.Cleanup(func() { askCheckpointPushQuestion = previous })
}

// firstPushWithOuterCheckpointRef runs the git hook's pre-push for a push that
// itself sends a queued checkpoint ref, as `git push --mirror` would.
func firstPushWithOuterCheckpointRef(t *testing.T, answer checkpointPushQuestionAnswer) (bareDir string, err error) {
	t.Helper()
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, answer)

	repo, openErr := gitrepo.OpenPath(workDir)
	require.NoError(t, openErr)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	outer := refs[0].String()
	tip := strings.TrimSpace(testutil.RunGit(t, workDir, "rev-parse", outer))
	ctx := WithPrePushRefs(t.Context(), []PrePushRef{{
		LocalRef: outer, LocalSHA: tip, RemoteRef: outer, RemoteSHA: plumbing.ZeroHash.String(),
	}})
	captureStderrWriter(t)
	return bareDir, NewManualCommitStrategy().PrePushFromGitHook(ctx, "origin")
}

// The outer-push check runs before the question, while the policy is still
// unset. Answering manual must still refuse a push that sends checkpoint refs
// itself, or that push uploads what the user just chose to keep.
func TestFirstPushQuestion_ManualRefusesPushCarryingCheckpointRefs(t *testing.T) {
	bareDir, err := firstPushWithOuterCheckpointRef(t, checkpointPushAnswerManual)

	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
	assert.Contains(t, err.Error(), "--no-verify")
	policy, loadErr := settings.LoadCheckpointPushPolicy(t.Context())
	require.NoError(t, loadErr)
	assert.Equal(t, settings.CheckpointPushPolicyManual, policy, "the answer is saved even though the push is refused")
	assert.Empty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", "refs/entire/")), "Entire's own flush sends nothing either")
}

func TestFirstPushQuestion_AutomaticLetsPushCarryingCheckpointRefsThrough(t *testing.T) {
	_, err := firstPushWithOuterCheckpointRef(t, checkpointPushAnswerAutomatic)
	require.NoError(t, err)
}

func TestFirstPushQuestion_ManualWithoutOuterCheckpointRefsHoldsQuietly(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, checkpointPushAnswerManual)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	stderr := captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(WithPrePushRefs(t.Context(), nil), "origin"))

	assert.Contains(t, stderr.String(), "2 checkpoints waiting")
	assertRefsAbsentFromRemote(t, bareDir, refs, "an answer of manual holds this push")
}

// The old-hook warning runs before the question, while the policy is unset.
// Answering manual on a clone whose hook does not pass the ref list must still
// warn that the push cannot be checked.
func TestFirstPushQuestion_ManualAnswerWarnsAboutAnOldHook(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, checkpointPushAnswerManual)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	stderr := captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Equal(t, 1, strings.Count(stderr.String(), "does not pass the refs being pushed"), "stderr:\n%s", stderr.String())
}

// The question can wait a long time. A policy chosen meanwhile, with
// `entire configure` or by a push from another worktree, wins over the answer.
func TestFirstPushQuestion_LateAnswerKeepsNewerChoice(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv(interactive.EnvTestTTY, "1")
	previous := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer {
		require.NoError(t, settings.SaveCheckpointPushPolicy(t.Context(), settings.CheckpointPushPolicyManual))
		return checkpointPushAnswerAutomatic
	}
	t.Cleanup(func() { askCheckpointPushQuestion = previous })
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(WithPrePushRefs(t.Context(), nil), "origin"))

	policy, err := settings.LoadCheckpointPushPolicy(t.Context())
	require.NoError(t, err)
	assert.Equal(t, settings.CheckpointPushPolicyManual, policy)
	assertRefsAbsentFromRemote(t, bareDir, refs, "the newer manual holds this push")
}

// Answering manual re-checks the push for held content, including a waiting
// checkpoint sent by commit under another name.
func TestFirstPushQuestion_ManualRefusesPushSendingHeldCommit(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, checkpointPushAnswerManual)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	tip := strings.TrimSpace(testutil.RunGit(t, workDir, "rev-parse", refs[0].String()))
	ctx := WithPrePushRefs(t.Context(), []PrePushRef{{
		LocalRef: tip, LocalSHA: tip, RemoteRef: "refs/heads/copy", RemoteSHA: plumbing.ZeroHash.String(),
	}})
	captureStderrWriter(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(ctx, "origin")

	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
	assertRefsAbsentFromRemote(t, bareDir, refs, "Entire's own flush sends nothing either")
}

// When automatic was stored while the question waited, an answer of manual
// loses: the push is neither refused nor held, and the stored policy decides.
func TestFirstPushQuestion_LostRaceLetsStoredPolicyDecide(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv(interactive.EnvTestTTY, "1")
	previous := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer {
		require.NoError(t, settings.SaveCheckpointPushPolicy(t.Context(), settings.CheckpointPushPolicyAutomatic))
		return checkpointPushAnswerManual
	}
	t.Cleanup(func() { askCheckpointPushQuestion = previous })
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	outer := refs[0].String()
	tip := strings.TrimSpace(testutil.RunGit(t, workDir, "rev-parse", outer))
	ctx := WithPrePushRefs(t.Context(), []PrePushRef{{
		LocalRef: outer, LocalSHA: tip, RemoteRef: outer, RemoteSHA: plumbing.ZeroHash.String(),
	}})
	captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(ctx, "origin"), "automatic was stored, so the push is not refused")
	assert.NotEmpty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", "refs/entire/")), "and Entire uploads as automatic does")
}

// After a saved manual answer, the waiting line that follows already names
// the upload command; the answer line must not repeat it.
func TestFirstPushQuestion_ManualAnswerNamesTheUploadCommandOnce(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, checkpointPushAnswerManual)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	stderr := captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(WithPrePushRefs(t.Context(), nil), "origin"))

	assert.Equal(t, 1, strings.Count(stderr.String(), "entire checkpoint push --all"), "stderr:\n%s", stderr.String())
}

// When manual was stored while the question waited, the stored policy wins,
// and the user's push is checked against it: the check before the question ran
// while the policy was unset.
func TestFirstPushQuestion_LostRaceToManualStillRefusesHeldRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv(interactive.EnvTestTTY, "1")
	previous := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer {
		require.NoError(t, settings.SaveCheckpointPushPolicy(t.Context(), settings.CheckpointPushPolicyManual))
		return checkpointPushAnswerAutomatic
	}
	t.Cleanup(func() { askCheckpointPushQuestion = previous })
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	outer := refs[0].String()
	tip := strings.TrimSpace(testutil.RunGit(t, workDir, "rev-parse", outer))
	ctx := WithPrePushRefs(t.Context(), []PrePushRef{{
		LocalRef: outer, LocalSHA: tip, RemoteRef: outer, RemoteSHA: plumbing.ZeroHash.String(),
	}})
	captureStderrWriter(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(ctx, "origin")

	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
	assertRefsAbsentFromRemote(t, bareDir, refs, "the stored manual holds this push")
}

// The old-hook warning before the question ran while the policy was unset, so
// a manual policy stored meanwhile must still bring it.
func TestFirstPushQuestion_LostRaceToManualWarnsAboutAnOldHook(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv(interactive.EnvTestTTY, "1")
	previous := askCheckpointPushQuestion
	askCheckpointPushQuestion = func() checkpointPushQuestionAnswer {
		require.NoError(t, settings.SaveCheckpointPushPolicy(t.Context(), settings.CheckpointPushPolicyManual))
		return checkpointPushAnswerAutomatic
	}
	t.Cleanup(func() { askCheckpointPushQuestion = previous })
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	stderr := captureStderrWriter(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Equal(t, 1, strings.Count(stderr.String(), "does not pass the refs being pushed"), "stderr:\n%s", stderr.String())
}

// When the hook could not save the ref list, the push on which the user
// answers manual may be a `--mirror`; it is refused, as the next one would be.
func TestFirstPushQuestion_ManualRefusesPushWhoseRefsWereNotSaved(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	answerFirstPushQuestionWith(t, checkpointPushAnswerManual)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	captureStderrWriter(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(WithPrePushRefsUnsaved(t.Context()), "origin")

	require.ErrorIs(t, err, ErrOuterPushUncheckable)
	assert.Contains(t, err.Error(), "you just chose the manual checkpoint push policy")
	assertRefsAbsentFromRemote(t, bareDir, refs, "Entire's own flush sends nothing either")
}
