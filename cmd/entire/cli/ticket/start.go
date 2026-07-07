package ticket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// startOptions collects the inputs to a start run.
type startOptions struct {
	id         string
	agentName  string
	branchName string
	noBranch   bool
	force      bool
}

// newStartCmd builds `entire ticket start`.
func newStartCmd(deps Deps) *cobra.Command {
	var agentName, branchName string
	var force, noBranch bool
	cmd := &cobra.Command{
		Use:   "start <ticket-id>",
		Short: "Create a branch, link a ticket, and launch an agent to work it",
		Long: `Link a ticket to a branch and launch a coding agent with the ticket's details
as context.

By default this creates (or switches to) a branch for the ticket, prompting for
the name with a suggested default (the platform's own branch name when it has
one). Use --branch to name it non-interactively, or --no-branch to stay on the
current branch.

Agent launch is temporarily disabled: start prepares the branch, link, and
prompt, and prints the prompt instead of spawning an agent.

Examples:
  entire ticket start ENG-142
  entire ticket start ENG-142 --branch amy/eng-142
  entire ticket start ENG-142 --no-branch`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if branchName != "" && noBranch {
				return errors.New("--branch and --no-branch are mutually exclusive")
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runStart(cmd.Context(), cmd.OutOrStdout(), deps, startOptions{
				id:         id,
				agentName:  agentName,
				branchName: branchName,
				noBranch:   noBranch,
				force:      force,
			})
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "agent to launch (default claude-code)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing link on this branch")
	cmd.Flags().StringVar(&branchName, "branch", "", "name of the branch to create/switch to for this ticket")
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "stay on the current branch instead of creating one")
	return cmd
}

func runStart(ctx context.Context, out io.Writer, deps Deps, opts startOptions) error {
	if deps.LaunchFix == nil {
		return errors.New("ticket start is unavailable: no agent launcher configured")
	}

	s, err := settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	tc := s.TicketConfig()
	if tc.IsZero() {
		return errors.New("no ticket platform configured — run `entire ticket setup` first")
	}

	id, err := resolveStartID(ctx, opts.id)
	if err != nil {
		return err
	}
	link := Link{Platform: tc.Platform, ID: id}

	// Best-effort fetch for the prompt and the suggested branch name.
	var task *Task
	if fetched, ferr := fetchTaskForStart(ctx, link); ferr == nil {
		task = fetched
	}

	if !opts.noBranch {
		if err := startBranch(ctx, task, link, opts.branchName); err != nil {
			return err
		}
	}

	branch, err := currentBranch(ctx)
	if err != nil {
		return err
	}
	if err := storeLink(ctx, branch, link, opts.force); err != nil {
		return err
	}

	// Refresh the stored snapshot to the latest ticket and note any drift since
	// it was last seen (best-effort).
	if changes, cerr := refreshSnapshot(ctx, branch, task); cerr == nil && len(changes) > 0 {
		fmt.Fprintf(out, "⚠️  Ticket %s changed since last sync: %s\n", link.ID, strings.Join(changes, "; "))
	}

	// Launching the coding agent is temporarily disabled — spinning up an agent
	// per start is expensive. start prepares the branch, link, and prompt; the
	// launcher (deps.LaunchFix, verified above) stays wired for when launch is
	// re-enabled.
	fmt.Fprintf(out, "✓ Prepared %s on branch %q (agent launch disabled for now).\n\n", link.ID, branch)
	fmt.Fprintln(out, "Prompt prepared for the agent:")
	fmt.Fprintln(out)
	fmt.Fprint(out, composeStartPrompt(link, task))
	return nil
}

// resolveStartID returns the explicit id, or infers one from the current branch.
func resolveStartID(ctx context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		return canonicalID(ctx, id), nil
	}
	cur, err := currentBranch(ctx)
	if err != nil {
		return "", err
	}
	prov, err := activeProvider(ctx)
	if err != nil {
		return "", fmt.Errorf("specify a ticket ID (auto-detect unavailable: %w)", err)
	}
	det, ok := prov.ResolveFromBranch(cur)
	if !ok {
		return "", fmt.Errorf("could not infer a ticket ID from branch %q; specify one, e.g. `entire ticket start ENG-142`", cur)
	}
	return canonicalID(ctx, det), nil
}

// startBranch resolves the branch name (flag, prompt, or default) and switches
// to it, creating it when needed.
func startBranch(ctx context.Context, task *Task, link Link, explicit string) error {
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = defaultBranchName(ctx, task, link)
		if interactive.CanPromptInteractively() {
			prompted, err := promptBranchName(ctx, name)
			if err != nil {
				return err
			}
			name = prompted
		}
	}
	if name == "" {
		return errors.New("branch name is empty (use --no-branch to stay on the current branch)")
	}
	return createOrSwitchBranch(ctx, name)
}

// fetchTaskForStart resolves the linked ticket via the active provider,
// returning a nil task (and the error) when it cannot be fetched.
func fetchTaskForStart(ctx context.Context, link Link) (*Task, error) {
	prov, err := activeProvider(ctx)
	if err != nil {
		return nil, err
	}
	task, err := prov.Fetch(ctx, link.ID)
	if err != nil {
		return nil, fmt.Errorf("fetch ticket %s: %w", link.ID, err)
	}
	return &task, nil
}

// composeStartPrompt renders the ticket as the agent's context. When the ticket
// could not be fetched it falls back to the ticket reference.
func composeStartPrompt(link Link, task *Task) string {
	if task == nil || task.Title == "" {
		return fmt.Sprintf("Ticket %s (%s)\n", link.ID, link.Platform)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Ticket %s: %s\n", link.ID, task.Title)
	if task.URL != "" {
		b.WriteString(task.URL)
		b.WriteString("\n")
	}
	if body := strings.TrimSpace(task.Intent); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if acc := strings.TrimSpace(task.Acceptance); acc != "" {
		b.WriteString("\nAcceptance criteria:\n")
		b.WriteString(acc)
		b.WriteString("\n")
	}
	if len(task.Comments) > 0 {
		b.WriteString("\nDiscussion:\n")
		for _, c := range task.Comments {
			author := c.Author
			if author == "" {
				author = "unknown"
			}
			fmt.Fprintf(&b, "- %s: %s\n", author, strings.TrimSpace(c.Body))
		}
	}
	return b.String()
}
