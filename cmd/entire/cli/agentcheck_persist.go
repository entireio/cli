package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const agentCheckVerificationEvidenceName = "verification.json"

func persistAgentCheckVerificationEvidenceForCurrentWorktree(ctx context.Context, cpID id.CheckpointID, evidence agentCheckVerificationEvidence) (string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	return persistAgentCheckVerificationEvidence(repoRoot, cpID, evidence)
}

func persistAgentCheckVerificationEvidence(repoRoot string, cpID id.CheckpointID, evidence agentCheckVerificationEvidence) (string, error) {
	if err := id.Validate(cpID.String()); err != nil {
		return "", fmt.Errorf("agentcheck verification checkpoint ID: %w", err)
	}
	if repoRoot == "" {
		return "", fmt.Errorf("agentcheck verification repository root is empty")
	}

	reportDir, err := agentCheckVerificationReportDir(repoRoot, cpID)
	if err != nil {
		return "", err
	}
	if err := mkdirAgentCheckReportDir(reportDir); err != nil {
		return "", err
	}

	data, err := jsonutil.MarshalIndentWithNewline(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal AgentCheck verification evidence: %w", err)
	}

	path := filepath.Join(reportDir, agentCheckVerificationEvidenceName)
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write AgentCheck verification evidence: %w", err)
	}
	return path, nil
}

func agentCheckVerificationReportDir(repoRoot string, cpID id.CheckpointID) (string, error) {
	repoRootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	reportDir := filepath.Join(repoRootAbs, agentCheckDirName, agentCheckReportsName, cpID.String())
	rel, err := filepath.Rel(repoRootAbs, reportDir)
	if err != nil {
		return "", fmt.Errorf("resolve AgentCheck report path: %w", err)
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("AgentCheck report path escapes repository root: %s", reportDir)
	}
	return reportDir, nil
}

func mkdirAgentCheckReportDir(reportDir string) error {
	parts := []string{
		filepath.Dir(filepath.Dir(reportDir)),
		filepath.Dir(reportDir),
		reportDir,
	}
	for _, part := range parts {
		info, err := os.Lstat(part)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlinked AgentCheck path: %s", part)
			}
			if !info.IsDir() {
				return fmt.Errorf("AgentCheck path is not a directory: %s", part)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat AgentCheck path %s: %w", part, err)
		}
		if err := os.Mkdir(part, 0o755); err != nil {
			return fmt.Errorf("create AgentCheck path %s: %w", part, err)
		}
	}
	return nil
}
