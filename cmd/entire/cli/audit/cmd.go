package audit

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"
)

type auditFlags struct {
	jsonOutput string
	asJSON     bool
	branch     string
	outputFile string
	tuiMode    bool
}

// NewCmd builds the `entire audit` cobra command hierarchy.
func NewCmd() *cobra.Command {
	f := &auditFlags{}

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit checkpoint history for release readiness, intent alignment, and handoffs",
		Long: `Entire Audit evaluates Entire Checkpoints on the current branch to provide
a comprehensive Release Readiness Score, intent verification matrix, risk audit,
and seamless agent/developer handoff context.

Examples:
  entire audit
  entire audit --json
  entire audit intent
  entire audit risks
  entire audit report --output RELEASE_READINESS.md
  entire audit handoff --json
  entire audit tui`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "all")
		},
	}

	cmd.Flags().BoolVar(&f.asJSON, "json", false, "Output audit results as JSON")
	cmd.Flags().StringVar(&f.branch, "branch", "", "Branch to audit (default: current HEAD branch)")
	cmd.Flags().StringVar(&f.outputFile, "output", "", "Write report to file path")
	cmd.Flags().BoolVar(&f.tuiMode, "tui", false, "Launch interactive TUI audit viewer")

	cmd.AddCommand(newIntentCmd(f))
	cmd.AddCommand(newRisksCmd(f))
	cmd.AddCommand(newReportCmd(f))
	cmd.AddCommand(newHandoffCmd(f))
	cmd.AddCommand(newTUICmd(f))

	return cmd
}

func newIntentCmd(f *auditFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "intent",
		Short: "Verify implementation against stated prompt intent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "intent")
		},
	}
}

func newRisksCmd(f *auditFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "risks",
		Short: "Identify unfinished requirements, TODOs, and unresolved risks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "risks")
		},
	}
}

func newReportCmd(f *auditFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate Markdown Release Readiness & Change Risk Report",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "report")
		},
	}
	cmd.Flags().StringVar(&f.outputFile, "output", "", "Write report to file path")
	return cmd
}

func newHandoffCmd(f *auditFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Generate structured agent/developer handoff package",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "handoff")
		},
	}
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "Output handoff package as JSON")
	return cmd
}

func newTUICmd(f *auditFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch interactive TUI audit dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditMain(cmd, f, "tui")
		},
	}
}

func runAuditMain(cmd *cobra.Command, f *auditFlags, mode string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}

	repo, err := git.PlainOpenWithOptions(cwd, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	engine := NewEngine(repo, cwd)
	res, err := engine.Run(cmd.Context(), AuditOptions{
		Branch:       f.branch,
		MaxDepth:     25,
		IncludeGraph: true,
	})
	if err != nil {
		return fmt.Errorf("audit execution failed: %w", err)
	}

	if f.tuiMode || mode == "tui" {
		return RunTUI(res)
	}

	w := cmd.OutOrStdout()

	switch mode {
	case "intent":
		if f.asJSON {
			data, _ := json.MarshalIndent(res.Intents, "", "  ")
			fmt.Fprintln(w, string(data))
		} else {
			fmt.Fprintln(w, "=== INTENT VERIFICATION MATRIX ===")
			for _, item := range res.Intents {
				fmt.Fprintf(w, "[%s] %s: %s (%s)\n", item.Status, item.ID, item.Prompt, item.Reasoning)
			}
		}
	case "risks":
		if f.asJSON {
			data, _ := json.MarshalIndent(res.Risks, "", "  ")
			fmt.Fprintln(w, string(data))
		} else {
			fmt.Fprintln(w, "=== IDENTIFIED RISKS & UNRESOLVED TASKS ===")
			for _, r := range res.Risks {
				fmt.Fprintf(w, "[%s] %s: %s (Loc: %s)\n", r.Severity, r.ID, r.Title, r.Location)
			}
		}
	case "report":
		md := RenderMarkdownReport(res)
		if f.outputFile != "" {
			if err := os.WriteFile(f.outputFile, []byte(md), 0644); err != nil {
				return fmt.Errorf("write output file: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Release Readiness report saved to %s\n", f.outputFile)
		} else {
			fmt.Fprintln(w, md)
		}
	case "handoff":
		payload, err := RenderHandoffPayload(res, f.asJSON)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, payload)
	default:
		if f.asJSON {
			data, _ := json.MarshalIndent(res, "", "  ")
			fmt.Fprintln(w, string(data))
		} else {
			RenderConsoleReport(res, w, true)
		}
	}

	return nil
}
