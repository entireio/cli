package cli

import (
	"fmt"
	"os"

	"entire.io/cli/cmd/entire/cli/list"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Interactive session browser",
		Long: `Browse branches, sessions, and checkpoints in an interactive tree view.

This command shows a hierarchical view of:
  - Branches with associated sessions
  - Sessions with their checkpoints
  - Options to open, resume, or rewind

Navigation:
  up/k, down/j   Move cursor
  left/h         Collapse / go to parent
  right/l        Expand
  enter          Toggle expand / perform default action

Actions:
  o              Open session (restore logs, show resume command)
  r              Resume (switch branch if needed, restore session)
  w              Rewind to checkpoint (restore code state)

The current branch is expanded by default. Sessions marked as "active"
are currently running in an agent.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			if jsonOutput {
				return runListJSON()
			}
			return runListInteractive()
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON instead of interactive view")

	return cmd
}

func runListInteractive() error {
	// Set the strategy for actions
	list.SetStrategy(GetStrategy())

	// Fetch data
	data, err := list.FetchTreeData()
	if err != nil {
		return fmt.Errorf("failed to fetch data: %w", err)
	}

	// Build tree
	tree := list.BuildTree(data)

	if len(tree) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	// Create and run the TUI
	model := list.NewModel(tree)
	p := tea.NewProgram(model, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("failed to run TUI: %w", err)
	}

	// Handle result if any
	if m, ok := finalModel.(list.Model); ok {
		if result := m.GetResult(); result != nil {
			// Print any output to stderr so it persists after TUI exits
			if result.Error != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", result.Error)
			} else {
				if result.Message != "" {
					fmt.Fprintln(os.Stderr, result.Message)
				}
				if result.ResumeCommand != "" {
					fmt.Fprintln(os.Stderr)
					fmt.Fprintln(os.Stderr, "To continue this session, run:")
					fmt.Fprintf(os.Stderr, "  %s\n", result.ResumeCommand)
				}
			}
		}
	}

	return nil
}

func runListJSON() error {
	// Fetch data
	data, err := list.FetchTreeData()
	if err != nil {
		return fmt.Errorf("failed to fetch data: %w", err)
	}

	// Build JSON output
	type checkpointJSON struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp,omitempty"`
		Message   string `json:"message,omitempty"`
		IsTask    bool   `json:"is_task,omitempty"`
	}

	type sessionJSON struct {
		ID          string           `json:"id"`
		Description string           `json:"description,omitempty"`
		Strategy    string           `json:"strategy,omitempty"`
		StartTime   string           `json:"start_time,omitempty"`
		IsActive    bool             `json:"is_active,omitempty"`
		Checkpoints []checkpointJSON `json:"checkpoints,omitempty"`
	}

	type branchJSON struct {
		Name      string        `json:"name"`
		IsCurrent bool          `json:"is_current,omitempty"`
		IsMerged  bool          `json:"is_merged,omitempty"`
		Sessions  []sessionJSON `json:"sessions,omitempty"`
	}

	type outputJSON struct {
		CurrentBranch string       `json:"current_branch"`
		MainBranch    string       `json:"main_branch"`
		Branches      []branchJSON `json:"branches"`
	}

	output := outputJSON{
		CurrentBranch: data.CurrentBranch,
		MainBranch:    data.MainBranch,
		Branches:      make([]branchJSON, 0, len(data.Branches)),
	}

	for _, branch := range data.Branches {
		bj := branchJSON{
			Name:      branch.Name,
			IsCurrent: branch.IsCurrent,
			IsMerged:  branch.IsMerged,
			Sessions:  make([]sessionJSON, 0, len(branch.Sessions)),
		}

		for _, sess := range branch.Sessions {
			sj := sessionJSON{
				ID:          sess.Session.ID,
				Description: sess.Session.Description,
				Strategy:    sess.Session.Strategy,
				IsActive:    sess.IsActive,
				Checkpoints: make([]checkpointJSON, 0, len(sess.Session.Checkpoints)),
			}

			if !sess.Session.StartTime.IsZero() {
				sj.StartTime = sess.Session.StartTime.Format("2006-01-02T15:04:05Z07:00")
			}

			for _, cp := range sess.Session.Checkpoints {
				cj := checkpointJSON{
					ID:      cp.CheckpointID,
					Message: cp.Message,
					IsTask:  cp.IsTaskCheckpoint,
				}
				if !cp.Timestamp.IsZero() {
					cj.Timestamp = cp.Timestamp.Format("2006-01-02T15:04:05Z07:00")
				}
				sj.Checkpoints = append(sj.Checkpoints, cj)
			}

			bj.Sessions = append(bj.Sessions, sj)
		}

		output.Branches = append(output.Branches, bj)
	}

	// Print as JSON
	fmt.Printf("%+v\n", output)
	return nil
}
