package cli

import (
	"fmt"
	"os"
	"sync"
	"time"

	"entire.io/cli/cmd/entire/cli/list"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// spinner frames for loading indicator
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// loadingSpinner displays a loading spinner until stopped.
type loadingSpinner struct {
	message string
	stop    chan struct{}
	done    sync.WaitGroup
}

// newLoadingSpinner creates and starts a loading spinner with the given message.
func newLoadingSpinner(message string) *loadingSpinner {
	s := &loadingSpinner{
		message: message,
		stop:    make(chan struct{}),
	}
	s.done.Add(1)
	go s.run()
	return s
}

// run displays the spinner animation.
func (s *loadingSpinner) run() {
	defer s.done.Done()
	frame := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	// Print initial frame
	fmt.Printf("\r%s %s", spinnerFrames[frame], s.message)

	for {
		select {
		case <-s.stop:
			// Clear the line
			fmt.Print("\r\033[K")
			return
		case <-ticker.C:
			frame = (frame + 1) % len(spinnerFrames)
			fmt.Printf("\r%s %s", spinnerFrames[frame], s.message)
		}
	}
}

// Stop stops the spinner and clears the line.
func (s *loadingSpinner) Stop() {
	close(s.stop)
	s.done.Wait()
}

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
	// Show loading spinner while fetching data
	spinner := newLoadingSpinner("Loading checkpoints...")

	// Fetch latest entire/sessions from origin
	FetchSessionsBranch()

	// Set the strategy for actions
	list.SetStrategy(GetStrategy())

	// Fetch data
	data, err := list.FetchTreeData()
	spinner.Stop()

	if err != nil {
		return fmt.Errorf("failed to fetch data: %w", err)
	}

	if len(data.Branches) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	// Create model with data (enables view mode toggling)
	model := list.NewModelWithData(data)
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
	// Fetch latest entire/sessions from origin
	FetchSessionsBranch()

	// Fetch data
	data, err := list.FetchTreeData()
	if err != nil {
		return fmt.Errorf("failed to fetch data: %w", err)
	}

	// Build JSON output with new hierarchy: Branch → Checkpoint → Sessions
	type sessionJSON struct {
		SessionID   string `json:"session_id"`
		Description string `json:"description,omitempty"`
		IsActive    bool   `json:"is_active,omitempty"`
	}

	type checkpointJSON struct {
		ID         string        `json:"id"`
		CommitHash string        `json:"commit_hash,omitempty"`
		CommitMsg  string        `json:"commit_msg,omitempty"`
		Timestamp  string        `json:"timestamp,omitempty"`
		StepsCount int           `json:"steps_count,omitempty"`
		IsTask     bool          `json:"is_task,omitempty"`
		Sessions   []sessionJSON `json:"sessions"`
	}

	type branchJSON struct {
		Name        string           `json:"name"`
		IsCurrent   bool             `json:"is_current,omitempty"`
		IsMerged    bool             `json:"is_merged,omitempty"`
		Checkpoints []checkpointJSON `json:"checkpoints,omitempty"`
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
			Name:        branch.Name,
			IsCurrent:   branch.IsCurrent,
			IsMerged:    branch.IsMerged,
			Checkpoints: make([]checkpointJSON, 0, len(branch.Checkpoints)),
		}

		for _, cp := range branch.Checkpoints {
			// Build sessions list
			sessions := make([]sessionJSON, 0, len(cp.Sessions))
			for _, sess := range cp.Sessions {
				sessions = append(sessions, sessionJSON{
					SessionID:   sess.SessionID,
					Description: sess.Description,
					IsActive:    sess.IsActive,
				})
			}

			cj := checkpointJSON{
				ID:         cp.CheckpointID,
				CommitHash: cp.CommitHash,
				CommitMsg:  cp.CommitMsg,
				StepsCount: cp.StepsCount,
				IsTask:     cp.IsTask,
				Sessions:   sessions,
			}
			if !cp.CreatedAt.IsZero() {
				cj.Timestamp = cp.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
			}
			bj.Checkpoints = append(bj.Checkpoints, cj)
		}

		output.Branches = append(output.Branches, bj)
	}

	// Print as JSON
	fmt.Printf("%+v\n", output)
	return nil
}
