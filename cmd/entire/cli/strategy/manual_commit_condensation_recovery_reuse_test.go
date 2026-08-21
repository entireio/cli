package strategy

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCondenseSession_KeepsCallerSuppliedCheckpointIDWhenRecoveryIsPending is
// the regression guard for the dangling commit trailer.
//
// Legacy reconciliation may return a *different* checkpoint ID than the one the
// caller passed in. That is fine for doctor and for the eager session-end path,
// which choose the ID themselves and record whatever comes back. It is not fine
// for PostCommit: its ID comes from the Entire-Checkpoint trailer, which is
// already written into the commit. If condensation redirects the write, the
// commit ends up naming a checkpoint that was never stored, and
// updateCombinedAttributionForCheckpoint writes attribution under that same
// non-existent ID.
//
// This is reachable without a process kill. CondenseSessionByID persists a
// recovery-required attempt before condensing; if condensation fails, the
// attempt remains for a later retry.
func TestCondenseSession_KeepsCallerSuppliedCheckpointIDWhenRecoveryIsPending(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	sessionID := "recovery-pending-then-commit"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(state *SessionState) error {
		endedAt := time.Now().UTC()
		state.Phase = session.PhaseEnded
		state.EndedAt = &endedAt
		state.FilesTouched = nil
		return nil
	}))

	// An interrupted write from an older CLI left this orphan behind.
	stale, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	orphanID := id.MustCheckpointID("111111111111")
	orphan, err := s.CondenseSession(context.Background(), repo, orphanID, stale, nil)
	require.NoError(t, err)
	require.False(t, orphan.Skipped)

	// Doctor reserved an ID and armed reconciliation, then failed before
	// condensing — so the flag is still set when the next commit arrives.
	trailerID := id.MustCheckpointID("222222222222")
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(state *SessionState) error {
		state.BeginCondensationAttempt(trailerID)
		state.RequireCondensationRecovery()
		return nil
	}))

	postCommitState, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	result, err := s.CondenseSession(context.Background(), repo, trailerID, postCommitState, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	assert.Equal(t, trailerID, result.CheckpointID,
		"a commit-driven condense must write the checkpoint ID already in the commit trailer")

	store := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs())
	summary, err := store.Read(context.Background(), trailerID)
	require.NoError(t, err)
	assert.NotNil(t, summary,
		"the commit's Entire-Checkpoint trailer must resolve to a stored checkpoint")
}

// TestTranscriptForRecoveryComparison_ReinjectsExternalizedImages covers the
// image path legacy reconciliation depends on.
//
// findInterruptedCondensation decides whether a stored checkpoint is the
// interrupted write by comparing transcripts byte-for-byte. A committed
// checkpoint has its inline images externalized into assets, so the comparison
// only lines up if reinjection reproduces the pre-externalization bytes exactly.
// Every other recovery test passes nil assets, which skips this entirely.
func TestTranscriptForRecoveryComparison_ReinjectsExternalizedImages(t *testing.T) {
	t.Parallel()

	b64 := base64.StdEncoding.EncodeToString(
		[]byte("\x89PNG\r\n\x1a\nfake-png-bytes-long-enough-to-be-externalized\x00\x01\x02"))
	original := []byte(`{"type":"user","message":{"content":[` +
		`{"type":"text","text":"look at this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n")

	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	require.NotNil(t, codec, "claude-code must have an image codec for this test to mean anything")

	externalized, extracted, err := codec.ExtractImages(original)
	require.NoError(t, err)
	require.NotEmpty(t, extracted, "the image must be externalized")
	require.NotContains(t, string(externalized), b64, "the stored transcript must not carry the base64")

	assets := make([]cpkg.TranscriptAsset, 0, len(extracted))
	for _, a := range extracted {
		assets = append(assets, cpkg.TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data})
	}

	compared, err := transcriptForRecoveryComparison(
		agent.AgentTypeClaudeCode, redact.AlreadyRedacted(externalized), assets)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(compared),
		"reinjection must reproduce the pre-externalization transcript byte-for-byte")
}

// TestTranscriptForRecoveryComparison_MissingAssetDoesNotFabricateAMatch guards
// the other direction: if an asset cannot be resolved, the comparison text must
// not silently come back equal to a stored transcript it does not match.
func TestTranscriptForRecoveryComparison_MissingAssetDoesNotFabricateAMatch(t *testing.T) {
	t.Parallel()

	b64 := base64.StdEncoding.EncodeToString(
		[]byte("\x89PNG\r\n\x1a\nanother-fake-png-payload-long-enough-to-externalize\x00"))
	original := []byte(`{"type":"user","message":{"content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n")

	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	require.NotNil(t, codec)
	externalized, extracted, err := codec.ExtractImages(original)
	require.NoError(t, err)
	require.NotEmpty(t, extracted)

	// Assets present but none matching the placeholder in the transcript.
	compared, err := transcriptForRecoveryComparison(
		agent.AgentTypeClaudeCode, redact.AlreadyRedacted(externalized),
		[]cpkg.TranscriptAsset{{Name: "assets/not-the-one.png", MediaType: "image/png", Data: []byte("x")}})
	require.NoError(t, err)
	assert.NotEqual(t, string(original), string(compared),
		"an unresolved asset must not produce a false transcript match")
	assert.True(t, strings.Contains(string(compared), "entire-asset:") || len(compared) != len(original),
		"the unresolved placeholder should still be visible in the comparison text")
}
