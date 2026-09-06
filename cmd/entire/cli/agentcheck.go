package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	contextpkg "github.com/entireio/cli/cmd/entire/cli/agentcheck"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

const (
	agentCheckDirName     = ".agentcheck"
	agentCheckReportsName = "reports"
	agentCheckReportName  = "report.html"
)

type agentCheckEvidenceStatus struct {
	CheckpointID string
	BundleDir    string
	BundleExists bool
	ReportPath   string
	ReportExists bool
}

func newAgentCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agentcheck [checkpoint-id | commit-sha]",
		Short: "Evaluate AgentCheck evidence for an Entire checkpoint",
		Long: `AgentCheck evaluates evidence from an Entire checkpoint.

The command builds AgentCheck context from Entire checkpoint evidence, runs the
deterministic evaluator, renders the result, and records repository verification
evidence for the resolved checkpoint.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentCheckTarget(args)
			result, err := runAgentCheckOrchestration(cmd.Context(), agentCheckOrchestrationOptions{
				Target: target,
				ErrW:   cmd.ErrOrStderr(),
			})
			if err != nil {
				cmd.SilenceUsage = true
				if target.defaultHead && isNoAgentCheckCheckpointTarget(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "AgentCheck evaluation is not connected yet.")
					fmt.Fprintln(cmd.OutOrStdout(), "No checkpoint target was supplied, and HEAD does not reference an Entire checkpoint.")
					fmt.Fprintln(cmd.OutOrStdout(), "Run `entire agentcheck <checkpoint-id-or-commit-sha>` after an Entire checkpoint exists.")
					return nil
				}
				return err
			}
			return contextpkg.Render(cmd.OutOrStdout(), result.Render)
		},
	}

	cmd.AddCommand(newAgentCheckStatusCmd())
	cmd.AddCommand(newAgentCheckOpenCmd())
	return cmd
}

func newAgentCheckStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [checkpoint-id | commit-sha]",
		Short: "Show read-only AgentCheck evidence status",
		Long: `Show whether a read-only AgentCheck evidence bundle exists for an Entire checkpoint.

When no target is provided, status checks the checkpoint referenced by HEAD.
This command does not create evidence, run verification, or generate results.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentCheckTarget(args)
			status, ok, err := resolveAgentCheckEvidenceStatus(cmd.Context(), cmd.ErrOrStderr(), target)
			if err != nil {
				cmd.SilenceUsage = true
				if target.defaultHead && isNoAgentCheckCheckpointTarget(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "AgentCheck evidence status unavailable.")
					fmt.Fprintln(cmd.OutOrStdout(), "No checkpoint target was supplied, and HEAD does not reference an Entire checkpoint.")
					fmt.Fprintln(cmd.OutOrStdout(), "Run `entire agentcheck status <checkpoint-id-or-commit-sha>` after an Entire checkpoint exists.")
					return nil
				}
				return err
			}
			if !ok {
				return nil
			}
			writeAgentCheckEvidenceStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	return cmd
}

func newAgentCheckOpenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open [checkpoint-id | commit-sha]",
		Short: "Locate an existing AgentCheck HTML report",
		Long: `Locate an existing AgentCheck HTML report for an Entire checkpoint.

When no target is provided, open checks the checkpoint referenced by HEAD.
This command is read-only: it does not create a report or run evaluation.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentCheckTarget(args)
			status, ok, err := resolveAgentCheckEvidenceStatus(cmd.Context(), cmd.ErrOrStderr(), target)
			if err != nil {
				cmd.SilenceUsage = true
				if target.defaultHead && isNoAgentCheckCheckpointTarget(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "AgentCheck report unavailable.")
					fmt.Fprintln(cmd.OutOrStdout(), "No checkpoint target was supplied, and HEAD does not reference an Entire checkpoint.")
					fmt.Fprintln(cmd.OutOrStdout(), "Run `entire agentcheck open <checkpoint-id-or-commit-sha>` after a report exists.")
					return nil
				}
				return err
			}
			if !ok {
				return nil
			}
			writeAgentCheckOpenStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	return cmd
}

type agentCheckTargetSpec struct {
	value       string
	defaultHead bool
}

func agentCheckTarget(args []string) agentCheckTargetSpec {
	if len(args) == 0 {
		return agentCheckTargetSpec{value: "HEAD", defaultHead: true}
	}
	return agentCheckTargetSpec{value: args[0]}
}

func resolveAgentCheckEvidenceStatus(ctx context.Context, errW io.Writer, target agentCheckTargetSpec) (agentCheckEvidenceStatus, bool, error) {
	cpID, lookup, err := resolveAgentCheckCheckpointID(ctx, errW, target)
	if lookup != nil {
		defer lookup.Close()
	}
	if err != nil {
		return agentCheckEvidenceStatus{}, false, err
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return agentCheckEvidenceStatus{}, false, fmt.Errorf("not a git repository: %w", err)
	}

	status := inspectAgentCheckEvidence(repoRoot, cpID)
	return status, true, nil
}

func resolveAgentCheckCheckpointID(ctx context.Context, errW io.Writer, target agentCheckTargetSpec) (id.CheckpointID, *explainCheckpointLookup, error) {
	opts := explainExportOptions{target: target.value}
	if target.defaultHead {
		opts = explainExportOptions{commitRef: target.value}
	}
	return resolveExplainCheckpointID(ctx, errW, opts)
}

func inspectAgentCheckEvidence(repoRoot string, cpID id.CheckpointID) agentCheckEvidenceStatus {
	bundleDir := filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String())
	reportPath := filepath.Join(bundleDir, agentCheckReportName)
	return agentCheckEvidenceStatus{
		CheckpointID: cpID.String(),
		BundleDir:    bundleDir,
		BundleExists: pathIsDir(bundleDir),
		ReportPath:   reportPath,
		ReportExists: pathIsRegularFile(reportPath),
	}
}

func pathIsDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

func pathIsRegularFile(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular()
}

func isNoAgentCheckCheckpointTarget(err error) bool {
	return errors.Is(err, errExportTargetNotCommit) ||
		errors.Is(err, fs.ErrNotExist) ||
		strings.Contains(err.Error(), "has no Entire-Checkpoint trailer")
}

func writeAgentCheckNotConnected(w io.Writer, status agentCheckEvidenceStatus) {
	fmt.Fprintln(w, "AgentCheck evaluation is not connected yet.")
	fmt.Fprintf(w, "Checkpoint: %s\n", status.CheckpointID)
	if status.BundleExists {
		fmt.Fprintf(w, "Evidence bundle: %s\n", status.BundleDir)
		return
	}
	fmt.Fprintf(w, "Evidence bundle: missing (%s)\n", status.BundleDir)
	fmt.Fprintln(w, "No verdict, score, or findings are available until Owner A/B contracts are connected.")
}

func writeAgentCheckEvidenceStatus(w io.Writer, status agentCheckEvidenceStatus) {
	fmt.Fprintf(w, "Checkpoint: %s\n", status.CheckpointID)
	if status.BundleExists {
		fmt.Fprintf(w, "AgentCheck evidence bundle: found\n")
		fmt.Fprintf(w, "Evidence path: %s\n", status.BundleDir)
	} else {
		fmt.Fprintf(w, "AgentCheck evidence bundle: missing\n")
		fmt.Fprintf(w, "Expected path: %s\n", status.BundleDir)
		fmt.Fprintln(w, "Run AgentCheck after the verification/evaluation pipeline is connected.")
	}
	if status.ReportExists {
		fmt.Fprintf(w, "HTML report: %s\n", status.ReportPath)
	} else {
		fmt.Fprintf(w, "HTML report: missing (%s)\n", status.ReportPath)
	}
}

func writeAgentCheckOpenStatus(w io.Writer, status agentCheckEvidenceStatus) {
	if status.ReportExists {
		fmt.Fprintf(w, "AgentCheck HTML report: %s\n", status.ReportPath)
		fmt.Fprintln(w, "Open the report path in a browser. This command did not generate or modify the report.")
		return
	}
	fmt.Fprintf(w, "AgentCheck HTML report missing for checkpoint %s.\n", status.CheckpointID)
	fmt.Fprintf(w, "Expected path: %s\n", status.ReportPath)
	if status.BundleExists {
		fmt.Fprintf(w, "Evidence bundle: %s\n", status.BundleDir)
	} else {
		fmt.Fprintf(w, "Evidence bundle: missing (%s)\n", status.BundleDir)
	}
	fmt.Fprintln(w, "No report can be opened until the AgentCheck report generator writes one.")
}
