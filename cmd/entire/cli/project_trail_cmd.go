package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func projectTrailSelector(args []string) string {
	if len(args) == 1 {
		return strings.TrimSpace(args[0])
	}
	return ""
}

func addProjectTrailSelectorFlags(cmd *cobra.Command) {
	cmd.Flags().String("branch", "", "Discover the project trail through this repository branch (defaults to current branch when no trail is given)")
}

func newProjectTrailShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "show [<trail>]", Short: "Show trail intent and its repositories and branches",
		Long: "Show a project trail by its project-local number or ULID. Without a selector, follow the current branch's parent. Use --branch to follow another branch's parent.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveProjectTrail(cmd, projectTrailSelector(args))
			if err != nil {
				return err
			}
			out, _, err := target.read(cmd.Context())
			if err != nil {
				return err
			}
			return printProjectTrail(cmd, out)
		},
	}
	addProjectTrailSelectorFlags(cmd)
	addJSONFlag(cmd)
	return cmd
}

func printProjectTrail(cmd *cobra.Command, item api.ProjectTrail) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), item)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Trail #%d: %s\nID: %s\nProject: %s\nStatus: %s\nType: %s\nPriority: %s\nAssignees: %s\n",
		item.Number, tuiutil.SanitizeTerminalLabel(item.Title), tuiutil.SanitizeTerminalLabel(item.ID), tuiutil.SanitizeTerminalLabel(item.ProjectID), tuiutil.SanitizeTerminalLabel(item.Status),
		tuiutil.SanitizeTerminalLabel(item.Type), tuiutil.SanitizeTerminalLabel(item.Priority), tuiutil.SanitizeTerminalLabel(strings.Join(item.Assignees, ", ")))
	if item.Body != "" {
		fmt.Fprintf(w, "\n%s\n", tuiutil.SanitizeTerminalText(item.Body))
	}
	if item.IsPossiblyPartial {
		fmt.Fprintln(w, "\nRepository and branch information may be incomplete due to access filtering.")
	}
	if len(item.Changes) > 0 {
		fmt.Fprintln(w, "\nRepositories and branches:")
		return printTable(w, []string{"REPOSITORY", colHeaderBranch, colHeaderStatus, colHeaderTitle}, item.Changes, func(c api.ChangeSummary) []string {
			repository := c.Repository
			if repository == "" {
				repository = c.RepositoryID
			}
			return []string{tuiutil.SanitizeTerminalLabel(repository), tuiutil.SanitizeTerminalLabel(c.Branch), tuiutil.SanitizeTerminalLabel(c.Status), tuiutil.SanitizeTerminalLabel(c.Title)}
		})
	}
	return nil
}

func newProjectTrailListCmd() *cobra.Command {
	var status, cursor string
	var pageSize int
	cmd := &cobra.Command{
		Use: "list", Short: "List project trails",
		Long: "List one page of project trails, newest update first. --status filters this page locally; use --page-token to continue. Omitting --status includes all lifecycle states.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if status != "" && !validProjectTrailStatus(status) {
				return errors.New("project trail status must be draft, open, or closed")
			}
			if pageSize < 1 || pageSize > trailListServerMaxLimit {
				return errors.New("--limit must be between 1 and 100")
			}
			target, err := resolveProjectTrailCollection(cmd)
			if err != nil {
				return err
			}
			page, err := target.list(cmd.Context(), pageSize, cursor)
			if err != nil {
				return err
			}
			items := make([]api.ProjectTrail, 0, len(page.Items))
			for _, item := range page.Items {
				if status == "" || status == item.Status {
					items = append(items, item)
				}
			}
			page.Items = items
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), page)
			}
			if err := printTable(cmd.OutOrStdout(), []string{"NUMBER", "ID", colHeaderStatus, colHeaderTitle}, items, func(t api.ProjectTrail) []string {
				return []string{strconv.Itoa(t.Number), t.ID, tuiutil.SanitizeTerminalLabel(t.Status), tuiutil.SanitizeTerminalLabel(t.Title)}
			}); err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching trails on this page.")
			}
			if page.NextPageToken != nil && *page.NextPageToken != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Next page: --page-token %s\n", tuiutil.SanitizeTerminalText(*page.NextPageToken))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter this page by status (draft, open, closed)")
	cmd.Flags().IntVar(&pageSize, "limit", 50, "Page size (1-100)")
	cmd.Flags().StringVar(&cursor, "page-token", "", "Continue from nextPageToken")
	addJSONFlag(cmd)
	return cmd
}

func validProjectTrailStatus(status string) bool {
	return status == string(trail.StatusDraft) || status == string(trail.StatusOpen) || status == string(trail.StatusClosed)
}

type projectTrailFields struct {
	Title, Body, Status, Type, Priority string
	Assignees                           []string
}

func (f *projectTrailFields) flags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.Title, "title", "", "Trail title")
	cmd.Flags().StringVar(&f.Body, "body", "", "Trail description (--body= clears it on update)")
	cmd.Flags().StringVar(&f.Status, "status", "", "Lifecycle status (draft, open, closed)")
	cmd.Flags().StringVar(&f.Type, "type", "", fmt.Sprintf("Type (%s)", formatValidTypes()))
	cmd.Flags().StringVar(&f.Priority, "priority", "", fmt.Sprintf("Priority (%s)", formatValidPriorities()))
	cmd.Flags().StringSliceVar(&f.Assignees, "assignee", nil, "Assignee logins; replaces the complete list (--assignee= clears it)")
}

func (f *projectTrailFields) validate(cmd *cobra.Command, creating bool) error {
	if (creating || cmd.Flags().Changed("title")) && strings.TrimSpace(f.Title) == "" {
		return errors.New("--title is required and must not be empty")
	}
	if cmd.Flags().Changed("status") && !validProjectTrailStatus(f.Status) {
		return errors.New("project trail status must be draft, open, or closed (merged belongs to a change)")
	}
	if cmd.Flags().Changed("type") && !trail.Type(f.Type).IsValid() {
		return fmt.Errorf("invalid type: %s", formatValidTypes())
	}
	if cmd.Flags().Changed("priority") && !trail.Priority(f.Priority).IsValid() {
		return fmt.Errorf("invalid priority: %s", formatValidPriorities())
	}
	return nil
}

func newProjectTrailUpdateCmd() *cobra.Command {
	var fields projectTrailFields
	var add, remove []string
	cmd := &cobra.Command{
		Use: "update [<trail>]", Short: "Update project intent and lifecycle",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fields.validate(cmd, false); err != nil {
				return err
			}
			var patch api.ProjectTrailUpdateRequest
			for _, field := range []struct {
				name  string
				value *string
				dest  **string
			}{
				{"title", &fields.Title, &patch.Title}, {"body", &fields.Body, &patch.Body},
				{"status", &fields.Status, &patch.Status}, {"type", &fields.Type, &patch.Type}, {"priority", &fields.Priority, &patch.Priority},
			} {
				if cmd.Flags().Changed(field.name) {
					*field.dest = field.value
				}
			}
			assigning := cmd.Flags().Changed("assignee") || cmd.Flags().Changed("add-assignee") || cmd.Flags().Changed("remove-assignee")
			if patch.Title == nil && patch.Body == nil && patch.Status == nil && patch.Type == nil && patch.Priority == nil && !assigning {
				return errors.New("provide at least one field to update")
			}
			target, err := resolveProjectTrail(cmd, projectTrailSelector(args))
			if err != nil {
				return err
			}
			current, etag, err := target.read(cmd.Context())
			if err != nil {
				return err
			}
			if etag == "" {
				return errors.New("project trail read returned no ETag; refusing an unprotected update")
			}
			if assigning {
				assignees := current.Assignees
				if cmd.Flags().Changed("assignee") {
					assignees = fields.Assignees
				}
				assignees = mergeStringSet(assignees, add, remove)
				if assignees == nil {
					assignees = []string{}
				}
				patch.Assignees = &assignees
			}
			var out api.ProjectTrail
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodPatch, target.path(), patch, http.Header{"If-Match": {etag}}, &out); err != nil {
				return fmt.Errorf("update project trail: %w", err)
			}
			if err := target.validateResponse(out); err != nil {
				return err
			}
			return printProjectTrail(cmd, out)
		},
	}
	fields.flags(cmd)
	cmd.Flags().StringSliceVar(&add, "add-assignee", nil, "Add assignee logins")
	cmd.Flags().StringSliceVar(&remove, "remove-assignee", nil, "Remove assignee logins")
	cmd.MarkFlagsMutuallyExclusive("assignee", "add-assignee")
	cmd.MarkFlagsMutuallyExclusive("assignee", "remove-assignee")
	addProjectTrailSelectorFlags(cmd)
	addJSONFlag(cmd)
	return cmd
}

// Creation links the current branch unless --no-branch is explicit. The server
// commits intent and membership together; the CLI never compensates a failed
// request by deleting potentially shared remote work.
func newProjectTrailCreateCmd() *cobra.Command {
	var fields projectTrailFields
	var branch, base, action, key string
	var noBranch bool
	cmd := &cobra.Command{
		Use: cmdCreate, Short: "Create a trail and link the current branch",
		Long: "Create project intent and link the current branch atomically. Use --branch to select another remote branch, or --no-branch for intent only. Linking requires an existing remote branch; --branch-action create asks the server to create it from --base. This command does not change the local checkout.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := fields.validate(cmd, true); err != nil {
				return err
			}
			if noBranch && (cmd.Flags().Changed("branch") || cmd.Flags().Changed("base") || cmd.Flags().Changed("branch-action")) {
				return errors.New("--no-branch cannot be combined with --branch, --base, or --branch-action")
			}
			if !noBranch {
				if err := requireTrailWorkingTarget(trailRepoFlag(cmd), "", branch); err != nil {
					return err
				}
				var err error
				branch, err = resolveTrailBranch(cmd.Context(), branch)
				if err != nil {
					return fmt.Errorf("select a branch or use --no-branch for intent only: %w", err)
				}
			}
			if err := validateProjectTrailBranch(cmd, branch, action); err != nil {
				return err
			}
			target, err := resolveProjectTrailCollection(cmd)
			if err != nil {
				return err
			}
			request := api.ProjectTrailCreateRequest{
				Title: fields.Title, Body: fields.Body, Status: fields.Status,
				Type: fields.Type, Priority: fields.Priority, Assignees: fields.Assignees,
			}
			if branch != "" {
				change, err := projectTrailChangeRequest(cmd, fields, branch, base, action)
				if err != nil {
					return err
				}
				request.Changes = []api.ChangeCreateRequest{change}
			}
			key = projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)
			var out api.ProjectTrail
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodPost, target.BasePath, request, http.Header{"Idempotency-Key": {key}}, &out); err != nil {
				return fmt.Errorf("create project trail (retry with --idempotency-key %s and the same fields): %w", key, err)
			}
			if err := target.validateResponse(out); err != nil {
				return err
			}
			return printProjectTrail(cmd, out)
		},
	}
	fields.flags(cmd)
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "Create intent without linking a repository branch")
	addProjectTrailCreationFlags(cmd, &branch, &base, &action, &key)
	addJSONFlag(cmd)
	return cmd
}

func addProjectTrailCreationFlags(cmd *cobra.Command, branch, base, action, key *string) {
	cmd.Flags().StringVar(branch, "branch", "", "Remote branch to link (defaults to the current branch)")
	cmd.Flags().StringVar(base, "base", "", "Base branch for new code work")
	cmd.Flags().StringVar(action, "branch-action", "link", "Branch action: link existing remote work, or create a new branch")
	cmd.Flags().StringVar(key, "idempotency-key", "", "Creation retry key; generated and printed to stderr if omitted (reuse with identical fields)")
}

func validateProjectTrailBranch(cmd *cobra.Command, branch, action string) error {
	if action != "link" && action != cmdCreate {
		return errors.New("--branch-action must be link or create")
	}
	if branch == "" && (cmd.Flags().Changed("base") || cmd.Flags().Changed("branch-action")) {
		return errors.New("--base and --branch-action require --branch")
	}
	return nil
}

func projectTrailIdempotencyKey(w io.Writer, key string) string {
	if key == "" {
		key = uuid.NewString()
	}
	fmt.Fprintf(w, "Idempotency key: %s\n", tuiutil.SanitizeTerminalText(key))
	return key
}

func projectTrailChangeRequest(cmd *cobra.Command, fields projectTrailFields, branch, base, action string) (api.ChangeCreateRequest, error) {
	forge, owner, repo, err := resolveTrailRepoOrRemote(cmd.Context(), trailRepoFlag(cmd))
	if err != nil {
		return api.ChangeCreateRequest{}, err
	}
	placement, err := resolveForgeRepoCellPlacement(cmd.Context(), forge, owner, repo)
	if err != nil {
		return api.ChangeCreateRequest{}, err
	}
	return api.ChangeCreateRequest{
		RepositoryID: placement.RepoID,
		TrailCreateRequest: api.TrailCreateRequest{Title: fields.Title, Body: fields.Body, BranchName: branch, Base: base, BranchAction: action,
			Status: fields.Status, Type: fields.Type, Priority: fields.Priority, Assignees: fields.Assignees},
	}, nil
}
