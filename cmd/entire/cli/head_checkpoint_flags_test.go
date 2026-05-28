package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

const (
	headFlagsTestAuthorName  = "Test"
	headFlagsTestAuthorEmail = "head-flags-test@entire.local"
)

// setupHeadFlagsRepo creates a git repo with an initial commit, switches the
// process CWD to it (cannot t.Parallel — t.Chdir conflicts), and returns the
// opened *git.Repository. Settings have v2 enabled so the v2 store also
// resolves the checkpoint summary.
func setupHeadFlagsRepo(t *testing.T) *git.Repository {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	require.NoError(t, os.MkdirAll(".entire", 0o750))
	require.NoError(t, os.WriteFile(
		".entire/settings.json",
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o600,
	))

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	return repo
}

// writeHeadCheckpointWithFlags writes a committed checkpoint and amends
// HEAD so it points at it via the Entire-Checkpoint trailer. The session
// metadata is configured with the supplied flags so the resolved summary
// surfaces them.
func writeHeadCheckpointWithFlags(t *testing.T, repo *git.Repository, hasReview, hasInvestigation bool) id.CheckpointID {
	t.Helper()
	cpID := id.MustCheckpointID("aabbccdd1122")
	store := checkpoint.NewGitStore(repo)
	require.NoError(t, store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "head-flags-session",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		AuthorName:       headFlagsTestAuthorName,
		AuthorEmail:      headFlagsTestAuthorEmail,
		HasReview:        hasReview,
		HasInvestigation: hasInvestigation,
	}))

	// Amend HEAD so it carries the Entire-Checkpoint trailer pointing at cpID.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	runGitInDir(t, cwd, "commit", "--amend", "-m", "init\n\nEntire-Checkpoint: "+cpID.String())
	return cpID
}

func TestHeadCheckpointFlags_BothFlagsTrue(t *testing.T) {
	repo := setupHeadFlagsRepo(t)
	cpID := writeHeadCheckpointWithFlags(t, repo, true, true)

	hasReview, hasInvestigation, info := headCheckpointFlags(context.Background())
	require.True(t, hasReview, "HasReview should be true when summary has it set")
	require.True(t, hasInvestigation, "HasInvestigation should be true when summary has it set")
	require.Contains(t, info, cpID.String(), "info string should reference the checkpoint id")
	require.True(t, strings.HasPrefix(info, "checkpoint "), "info should start with 'checkpoint '")
}

func TestHeadCheckpointFlags_NeitherFlag(t *testing.T) {
	repo := setupHeadFlagsRepo(t)
	// Write a checkpoint but with no review/investigate flags, then verify
	// the helper returns (false, false, info) — info is non-empty because a
	// checkpoint exists at HEAD; the flags simply aren't set.
	cpID := writeHeadCheckpointWithFlags(t, repo, false, false)

	hasReview, hasInvestigation, info := headCheckpointFlags(context.Background())
	require.False(t, hasReview)
	require.False(t, hasInvestigation)
	require.Contains(t, info, cpID.String(),
		"info string should still resolve to the checkpoint id even when both flags are false")
}

func TestHeadCheckpointFlags_NoCheckpointAtHead(t *testing.T) {
	// Fresh repo with an initial commit but no Entire-Checkpoint trailer.
	setupHeadFlagsRepo(t)

	hasReview, hasInvestigation, info := headCheckpointFlags(context.Background())
	require.False(t, hasReview)
	require.False(t, hasInvestigation)
	require.Empty(t, info, "info must be empty when HEAD has no Entire-Checkpoint trailer")
}

// TestHeadHasReviewCheckpoint_WrapperPreservesContract pins the
// (bool, string) signature for legacy callers (review re-run guard, status).
// When HasReview is false but HasInvestigation is true, the wrapper must
// still return false (it doesn't get to look at the investigation flag).
func TestHeadHasReviewCheckpoint_WrapperPreservesContract(t *testing.T) {
	repo := setupHeadFlagsRepo(t)
	writeHeadCheckpointWithFlags(t, repo, false, true)

	hasReview, info := headHasReviewCheckpoint(context.Background())
	require.False(t, hasReview, "wrapper must not piggyback on HasInvestigation")
	require.Empty(t, info, "info must be empty when the wrapper returns false")
}

// TestHeadHasInvestigateCheckpoint_OnlyInvestigation mirrors the review
// wrapper test for the investigate-only path.
func TestHeadHasInvestigateCheckpoint_OnlyInvestigation(t *testing.T) {
	repo := setupHeadFlagsRepo(t)
	cpID := writeHeadCheckpointWithFlags(t, repo, false, true)

	hasInvestigation, info := headHasInvestigateCheckpoint(context.Background())
	require.True(t, hasInvestigation)
	require.Contains(t, info, cpID.String())
}

// TestHeadHasInvestigateCheckpoint_WrapperPreservesContract pins the
// symmetric invariant: when HasReview is true but HasInvestigation is
// false, the investigate wrapper must NOT piggyback on the review flag.
func TestHeadHasInvestigateCheckpoint_WrapperPreservesContract(t *testing.T) {
	repo := setupHeadFlagsRepo(t)
	writeHeadCheckpointWithFlags(t, repo, true, false)

	hasInvestigation, info := headHasInvestigateCheckpoint(context.Background())
	require.False(t, hasInvestigation, "wrapper must not piggyback on HasReview")
	require.Empty(t, info, "info must be empty when the wrapper returns false")
}
