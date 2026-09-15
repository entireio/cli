package strategy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func newShadowOnlyCommit(t *testing.T, env *shadowCleanupEnv, shadow string) plumbing.Hash {
	t.Helper()
	hash, err := checkpoint.CreateCommit(t.Context(), env.repo, emptyTreeHash(t, env.repo), env.baseHash, "shadow-only history", "test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, env.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadow), hash)))
	return hash
}

func requireShadowBranchAt(t *testing.T, env *shadowCleanupEnv, shadow string, hash plumbing.Hash) {
	t.Helper()
	ref, err := env.repo.Reference(plumbing.NewBranchReferenceName(shadow), false)
	require.NoError(t, err)
	require.Equal(t, hash, ref.Hash())
}

func TestCleanupPushedShadowBranches_PreservesExpiredUncondensed(t *testing.T) {
	for _, phase := range []session.Phase{session.PhaseActive, session.PhaseEnded} {
		t.Run(string(phase), func(t *testing.T) {
			env := newShadowCleanupEnv(t)
			ctx := t.Context()
			shadow := env.addShadowBranch(env.baseHash.String(), "")
			hash := newShadowOnlyCommit(t, env, shadow)
			recent := time.Now().Add(-time.Hour)
			state := &SessionState{SessionID: "pending-session", BaseCommit: env.baseHash.String(), StartedAt: recent, LastInteractionTime: &recent, Phase: phase, StepCount: 1, FullyCondensed: false}
			if phase == session.PhaseEnded {
				state.EndedAt = &recent
			}
			require.NoError(t, SaveSessionState(ctx, state))
			deleted, err := CleanupPushedShadowBranches(ctx)
			require.NoError(t, err)
			require.Zero(t, deleted)
			require.True(t, env.branchExists(shadow))
			old := time.Now().Add(-8 * 24 * time.Hour)
			state.StartedAt = old
			state.LastInteractionTime = &old
			if phase == session.PhaseEnded {
				state.EndedAt = &old
			}
			require.NoError(t, SaveSessionState(ctx, state))
			deleted, err = CleanupPushedShadowBranches(ctx)
			require.NoError(t, err)
			require.Zero(t, deleted)
			require.True(t, env.branchExists(shadow))
			_, err = os.Stat(filepath.Join(env.dir, ".git", "entire-sessions", state.SessionID+".json"))
			require.NoError(t, err)
			t.Logf("phase=%s control_deleted=0 aged_deleted=%d state_preserved=true", phase, deleted)
			requireShadowBranchAt(t, env, shadow, hash)
		})
	}
}

func TestResetSession_PreservesCorruptSiblingShadow(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			env := newShadowCleanupEnv(t)
			ctx := t.Context()
			shadow := env.addShadowBranch(env.baseHash.String(), "")
			hash := newShadowOnlyCommit(t, env, shadow)
			for _, id := range []string{"reset-me", "keep-me"} {
				require.NoError(t, SaveSessionState(ctx, &SessionState{SessionID: id, BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseActive, StepCount: 1}))
			}
			sibling := filepath.Join(env.dir, ".git", "entire-sessions", "keep-me.json")
			if corrupt {
				require.NoError(t, os.WriteFile(sibling, []byte(`{"session_id":`), 0600))
			}
			var output strings.Builder
			var warnings strings.Builder
			resetErr := NewManualCommitStrategy().ResetSession(ctx, &output, &warnings, "reset-me")
			if corrupt {
				require.NoError(t, resetErr)
				require.Contains(t, warnings.String(), "Warning: failed to clean up shadow branch")
			} else {
				require.NoError(t, resetErr)
				require.Empty(t, warnings.String())
			}
			_, err := os.Stat(filepath.Join(env.dir, ".git", "entire-sessions", "reset-me.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
			require.True(t, env.branchExists(shadow))
			_, err = os.Stat(sibling)
			require.NoError(t, err)
			t.Logf("corrupt=%v shared_branch_exists=%v sibling_state_exists=true output=%q warnings=%q", corrupt, env.branchExists(shadow), output.String(), warnings.String())
			requireShadowBranchAt(t, env, shadow, hash)
			if corrupt {
				require.NoError(t, SaveSessionState(ctx, &SessionState{SessionID: "keep-me", BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseActive, StepCount: 1}))
				require.NoError(t, NewManualCommitStrategy().ResetSession(ctx, &output, io.Discard, "keep-me"))
				require.False(t, env.branchExists(shadow))
			}
		})
	}
}

func TestListAllSessionStates_PreservesUnreadableShadow(t *testing.T) {
	env := newShadowCleanupEnv(t)
	ctx := t.Context()
	shadow := env.addShadowBranch(env.baseHash.String(), "")
	hash := newShadowOnlyCommit(t, env, shadow)
	state := &SessionState{SessionID: "idle-pending", BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseIdle, StepCount: 1}
	require.NoError(t, SaveSessionState(ctx, state))
	strat := NewManualCommitStrategy()
	states, err := strat.listAllSessionStates(ctx)
	require.NoError(t, err)
	require.Len(t, states, 1)
	refPath := filepath.Join(env.dir, ".git", "refs", "heads", shadow)
	require.NoError(t, os.Chmod(refPath, 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(refPath, 0o600)) })
	_, rawErr := os.ReadFile(refPath)
	if !os.IsPermission(rawErr) {
		t.Skip("filesystem does not enforce unreadable file permissions")
	}
	_, readErr := env.repo.Reference(plumbing.NewBranchReferenceName(shadow), true)
	require.Error(t, readErr)
	require.ErrorIs(t, readErr, plumbing.ErrReferenceNotFound)
	states, err = strat.listAllSessionStates(ctx)
	require.NoError(t, err)
	require.Empty(t, states)
	_, statErr := os.Stat(filepath.Join(env.dir, ".git", "entire-sessions", state.SessionID+".json"))
	require.NoError(t, statErr)
	t.Logf("ref_error=%v list_error=%v state_preserved=true", readErr, err)
	require.NoError(t, os.Chmod(refPath, 0600))
	deleted, err := CleanupPushedShadowBranches(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
	requireShadowBranchAt(t, env, shadow, hash)
}

func TestCleanupPushedShadowBranches_PreservesCorruptState(t *testing.T) {
	env := newShadowCleanupEnv(t)
	ctx := t.Context()
	shadow := env.addShadowBranch(env.baseHash.String(), "")
	hash := newShadowOnlyCommit(t, env, shadow)
	env.addSessionState("pending", env.baseHash.String(), "", nil, nil, false)
	path := filepath.Join(env.dir, ".git", "entire-sessions", "pending.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"session_id":`), 0o600))
	deleted, err := CleanupPushedShadowBranches(ctx)
	require.Error(t, err)
	require.Zero(t, deleted)
	require.True(t, env.branchExists(shadow))
	requireShadowBranchAt(t, env, shadow, hash)
	env.addSessionState("pending", env.baseHash.String(), "", nil, nil, false)
	deleted, err = CleanupPushedShadowBranches(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
}
