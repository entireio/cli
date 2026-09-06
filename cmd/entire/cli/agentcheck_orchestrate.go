package cli

import (
	"context"
	"fmt"
	"io"

	contextpkg "github.com/entireio/cli/cmd/entire/cli/agentcheck"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

type agentCheckOrchestrationOptions struct {
	Target agentCheckTargetSpec
	ErrW   io.Writer
	Graph  contextpkg.GraphProvider

	ResolveCheckpoint   agentCheckCheckpointResolver
	BuildContext        agentCheckContextBuilder
	RunVerification     agentCheckVerificationFunc
	PersistVerification agentCheckVerificationPersistenceFunc
	VerificationOptions agentCheckVerificationOptions
}

type agentCheckOrchestrationResult struct {
	CheckpointID     id.CheckpointID
	Context          *contextpkg.Context
	Verification     agentCheckVerificationEvidence
	VerificationPath string
}

type agentCheckCheckpointResolver func(context.Context, io.Writer, agentCheckTargetSpec) (id.CheckpointID, error)
type agentCheckContextBuilder func(context.Context, id.CheckpointID, contextpkg.RepositoryBuildOptions) (*contextpkg.Context, error)
type agentCheckVerificationFunc func(context.Context, agentCheckVerificationOptions) agentCheckVerificationEvidence
type agentCheckVerificationPersistenceFunc func(string, id.CheckpointID, agentCheckVerificationEvidence) (string, error)

func runAgentCheckOrchestration(ctx context.Context, opts agentCheckOrchestrationOptions) (agentCheckOrchestrationResult, error) {
	resolver := opts.ResolveCheckpoint
	if resolver == nil {
		resolver = resolveAgentCheckCheckpointForOrchestration
	}
	buildContext := opts.BuildContext
	if buildContext == nil {
		buildContext = contextpkg.BuildFromRepository
	}
	runVerification := opts.RunVerification
	if runVerification == nil {
		runVerification = runAgentCheckVerification
	}
	persistVerification := opts.PersistVerification
	if persistVerification == nil {
		persistVerification = persistAgentCheckVerificationEvidence
	}

	cpID, err := resolver(ctx, opts.ErrW, opts.Target)
	if err != nil {
		return agentCheckOrchestrationResult{}, err
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return agentCheckOrchestrationResult{}, fmt.Errorf("agentcheck orchestration: resolve worktree root: %w", err)
	}

	acContext, err := buildContext(ctx, cpID, contextpkg.RepositoryBuildOptions{
		RepoRoot: repoRoot,
		Graph:    opts.Graph,
	})
	if err != nil {
		return agentCheckOrchestrationResult{}, fmt.Errorf("agentcheck orchestration: build context: %w", err)
	}

	verificationOpts := opts.VerificationOptions
	verificationOpts.RepoRoot = repoRoot
	verification := runVerification(ctx, verificationOpts)

	verificationPath, err := persistVerification(repoRoot, cpID, verification)
	if err != nil {
		return agentCheckOrchestrationResult{}, fmt.Errorf("agentcheck orchestration: persist verification evidence: %w", err)
	}

	return agentCheckOrchestrationResult{
		CheckpointID:     cpID,
		Context:          acContext,
		Verification:     verification,
		VerificationPath: verificationPath,
	}, nil
}

func resolveAgentCheckCheckpointForOrchestration(ctx context.Context, errW io.Writer, target agentCheckTargetSpec) (id.CheckpointID, error) {
	cpID, lookup, err := resolveAgentCheckCheckpointID(ctx, errW, target)
	if lookup != nil {
		defer lookup.Close()
	}
	return cpID, err
}
