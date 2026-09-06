package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	contextpkg "github.com/entireio/cli/cmd/entire/cli/agentcheck"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

func TestAgentCheckOrchestrationBuildsContextRunsVerificationAndPersists(t *testing.T) {
	cpID := setupAgentCheckOrchestrationRepo(t)
	var buildCalled bool
	var evaluateCalled bool
	var verificationCalled bool

	result, err := runAgentCheckOrchestration(context.Background(), agentCheckOrchestrationOptions{
		Target: agentCheckTargetSpec{value: cpID.String()},
		ResolveCheckpoint: func(_ context.Context, _ io.Writer, target agentCheckTargetSpec) (id.CheckpointID, error) {
			require.Equal(t, cpID.String(), target.value)
			return cpID, nil
		},
		BuildContext: func(_ context.Context, got id.CheckpointID, opts contextpkg.RepositoryBuildOptions) (*contextpkg.Context, error) {
			buildCalled = true
			require.Equal(t, cpID, got)
			require.NotEmpty(t, opts.RepoRoot)
			require.Nil(t, opts.Graph)
			return &contextpkg.Context{CheckpointID: got}, nil
		},
		Evaluate: func(ctx contextpkg.Context) contextpkg.EvaluationResult {
			evaluateCalled = true
			require.Equal(t, cpID, ctx.CheckpointID)
			return contextpkg.EvaluationResult{
				Verdict: contextpkg.Verdict(contextpkg.VerdictTrusted),
				Summary: "All evidence matched.",
			}
		},
		RunVerification: func(_ context.Context, opts agentCheckVerificationOptions) agentCheckVerificationEvidence {
			verificationCalled = true
			require.NotEmpty(t, opts.RepoRoot)
			return sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess)
		},
	})

	require.NoError(t, err)
	require.True(t, buildCalled)
	require.True(t, evaluateCalled)
	require.True(t, verificationCalled)
	require.Equal(t, cpID, result.CheckpointID)
	require.NotNil(t, result.Context)
	require.Equal(t, cpID, result.Context.CheckpointID)
	require.Equal(t, contextpkg.Verdict(contextpkg.VerdictTrusted), result.Evaluation.Verdict)
	require.Equal(t, contextpkg.VerdictTrusted, result.Render.Verdict)
	require.NotNil(t, result.Render.Verification)
	require.Equal(t, agentCheckVerificationSuccess, result.Verification.Status)
	require.Equal(t, filepath.Join(agentCheckDirName, agentCheckReportsName, cpID.String(), agentCheckVerificationEvidenceName), agentCheckRelativePath(t, result.VerificationPath))

	persisted := readPersistedAgentCheckVerificationEvidence(t, result.VerificationPath)
	require.Equal(t, result.Verification, persisted)
}

func TestAgentCheckOrchestrationContextBuildFailureIsSurfaced(t *testing.T) {
	cpID := setupAgentCheckOrchestrationRepo(t)
	var verificationCalled bool

	result, err := runAgentCheckOrchestration(context.Background(), agentCheckOrchestrationOptions{
		Target:            agentCheckTargetSpec{value: cpID.String()},
		ResolveCheckpoint: staticAgentCheckResolver(cpID),
		BuildContext: func(context.Context, id.CheckpointID, contextpkg.RepositoryBuildOptions) (*contextpkg.Context, error) {
			return nil, errors.New("context build failed")
		},
		RunVerification: func(context.Context, agentCheckVerificationOptions) agentCheckVerificationEvidence {
			verificationCalled = true
			return sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess)
		},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "build context")
	require.Contains(t, err.Error(), "context build failed")
	require.False(t, verificationCalled)
	require.Empty(t, result.VerificationPath)
}

func TestAgentCheckOrchestrationVerificationFailurePersistsEvidence(t *testing.T) {
	cpID := setupAgentCheckOrchestrationRepo(t)
	want := sampleAgentCheckVerificationEvidence(agentCheckVerificationFailed)
	want.Stdout = "--- FAIL: TestThing\n"
	want.Stderr = "failure detail\n"

	result, err := runAgentCheckOrchestration(context.Background(), agentCheckOrchestrationOptions{
		Target:            agentCheckTargetSpec{value: cpID.String()},
		ResolveCheckpoint: staticAgentCheckResolver(cpID),
		BuildContext: func(_ context.Context, got id.CheckpointID, _ contextpkg.RepositoryBuildOptions) (*contextpkg.Context, error) {
			return &contextpkg.Context{CheckpointID: got}, nil
		},
		RunVerification: func(context.Context, agentCheckVerificationOptions) agentCheckVerificationEvidence {
			return want
		},
	})

	require.NoError(t, err)
	require.Equal(t, agentCheckVerificationFailed, result.Verification.Status)
	require.Equal(t, 1, result.Verification.ExitCode)

	data, err := os.ReadFile(result.VerificationPath)
	require.NoError(t, err)
	var persisted agentCheckVerificationEvidence
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.Equal(t, want, persisted)
}

func setupAgentCheckOrchestrationRepo(t *testing.T) id.CheckpointID {
	t.Helper()
	repo := setupAgentCheckRepo(t)
	require.NoError(t, repo.Close())
	paths.ClearWorktreeRootCache()
	return id.MustCheckpointID("abc123abc123")
}

func staticAgentCheckResolver(cpID id.CheckpointID) agentCheckCheckpointResolver {
	return func(context.Context, io.Writer, agentCheckTargetSpec) (id.CheckpointID, error) {
		return cpID, nil
	}
}

func agentCheckRelativePath(t *testing.T, path string) string {
	t.Helper()
	repoRoot, err := agentCheckTestRepoRoot()
	require.NoError(t, err)
	rel, err := filepath.Rel(repoRoot, path)
	require.NoError(t, err)
	return rel
}
