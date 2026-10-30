package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

const trailBranchActionLink = "link"

// trailChangeGatesTimeout bounds the merge gate reads behind `trail show`.
// The trail renders without them, so a slow cell only drops a change's gates.
const trailChangeGatesTimeout = 5 * time.Second

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
		Long: "Show a project trail by its project-local number or ULID, or the trail of <repo>/<number> branch work. Without a selector, follow the current branch's parent. Use --branch to follow another branch's parent. Open changes, and the change the trail was reached through, show their merge gates (the first 20 changes).",
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
			return printProjectTrail(cmd, out, target.Change, lookupProjectTrailGates(cmd, target, out))
		},
	}
	addProjectTrailSelectorFlags(cmd)
	addJSONFlag(cmd)
	return cmd
}

// lookupProjectTrailGates reads the mergeability snapshot of each open change,
// and of the change the trail was reached through whatever its status, from
// its repository trail, keyed by change ID. A change missing from the result
// was not looked up or could not be read; the result is never nil, so every
// change carries the key in --json. Failures are summed into one
// warning so a partial-access reader sees the trail rather than an error.
func lookupProjectTrailGates(cmd *cobra.Command, target *projectTrailTarget, item api.ProjectTrail) map[string]*api.TrailMergeability {
	var changes []api.ChangeSummary
	for _, c := range item.Changes {
		if c.Number <= 0 {
			continue // Not yet a repository trail; there is nothing to read.
		}
		if target.Change != nil && c.ID == target.Change.ID {
			if c.Repository == "" {
				c.Repository = target.Change.Repo
			}
			// First, so the lookup cap never drops the change the user asked about.
			changes = append([]api.ChangeSummary{c}, changes...)
			continue
		}
		if s := trail.Status(c.Status); s == trail.StatusMerged || s == trail.StatusClosed {
			continue
		}
		changes = append(changes, c)
	}
	if len(changes) == 0 {
		return map[string]*api.TrailMergeability{}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), trailChangeGatesTimeout)
	defer cancel()
	// The current change's repository is already open from resolving the
	// trail; reuse it as reviews do (trail_working_context.go).
	var local trailRepoAccess
	if target.Change != nil {
		local = target.Change.Access
	}
	gates, failed := lookupTrailChangeData(ctx, newTrailRepoClientOpener(trailInsecureHTTP(cmd)), target, local, changes, readTrailChangeMergeability)
	if failed > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not load merge gates for %d change(s)\n", failed)
	}
	if skipped := len(changes) - trailChangeReviewLimit; skipped > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Merge gates are shown for the first %d changes; %d more were not read.\n", trailChangeReviewLimit, skipped)
	}
	return gates
}

func readTrailChangeMergeability(ctx context.Context, access *trailRepoAccess, number int) (*api.TrailMergeability, error) {
	detail, err := fetchTrailDetailAtPath(ctx, access.Client, access.BasePath, number)
	if err != nil {
		return nil, err
	}
	mg, err := detail.DecodeMergeability()
	if err != nil {
		return nil, fmt.Errorf("trail %d: %w", number, err)
	}
	return mg, nil
}

// projectTrailJSON is a project trail as `--json` prints it: each change
// gains its page inside the trail, and the change the trail was reached
// through is marked current. URL stays the whole trail's page.
type projectTrailJSON struct {
	api.ProjectTrail

	Changes []projectTrailChangeJSON `json:"changes,omitempty"`
}

type projectTrailChangeJSON struct {
	api.ChangeSummary

	URL     string `json:"url,omitempty"`
	Current bool   `json:"current,omitempty"`
	// Mergeability is the change's merge gates snapshot, set only by `trail
	// show`. There it is always present and null when there is no snapshot: a
	// merged or closed change that was not read, a failed read (which also
	// warns on stderr), or a read whose server has no snapshot yet (no
	// warning; the repository trail's own --json prints null for it too). A
	// missing verdict so never reads as "mergeable": false; commands that do
	// not read gates omit the key, which is why it points to a possibly nil
	// snapshot.
	Mergeability **trailMergeabilityJSON `json:"mergeability,omitempty"`

	// gates is the snapshot for the human view; gatesLoaded tells a change
	// whose read returned no snapshot from one that was not read.
	gates       *api.TrailMergeability
	gatesLoaded bool

	// selector is the change as <repo>/<number>, from the summary's repository
	// or, for the current change, the one the CLI resolved when the summary
	// has none; empty when no valid repository name is known.
	selector string
}

// projectTrailChangeURL is a change's page under its trail's server-supplied
// URL, <trail URL>/changes/<repo>/<number>; empty when either is unknown.
// The selector is validated, so it is safe as path segments.
func projectTrailChangeURL(trailURL, selector string) string {
	trailURL = strings.TrimRight(strings.TrimSpace(trailURL), "/")
	if trailURL == "" || selector == "" {
		return ""
	}
	return trailURL + "/changes/" + selector
}

// printProjectTrail renders a project trail. current is the change the trail
// was reached through, or nil when it was selected directly. gates holds the
// changes' mergeability snapshots by change ID; nil when the command does
// not read gates.
func printProjectTrail(cmd *cobra.Command, item api.ProjectTrail, current *projectTrailCurrentChange, gates map[string]*api.TrailMergeability) error {
	out := projectTrailJSON{ProjectTrail: item, Changes: mapSlice(item.Changes, func(c api.ChangeSummary) projectTrailChangeJSON {
		entry := projectTrailChangeJSON{ChangeSummary: c}
		named := c
		if current != nil && c.ID == current.ID {
			entry.Current = true
			if named.Repository == "" {
				named.Repository = current.Repo
			}
		}
		entry.selector, _ = trailChangeSelectorFor(named)
		entry.URL = projectTrailChangeURL(item.URL, entry.selector)
		if gates != nil {
			entry.gates, entry.gatesLoaded = gates[c.ID]
			snapshot := toTrailMergeabilityJSON(entry.gates)
			entry.Mergeability = &snapshot
		}
		return entry
	})}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), out)
	}
	printProjectTrailDetails(cmd.OutOrStdout(), item, out.Changes)
	return nil
}

// printProjectTrailDetails renders the human project `trail show` view in the
// legacy view's style: a yellow title, indented fields with aligned yellow
// labels, colored statuses, then the changes and the description.
func printProjectTrailDetails(w io.Writer, item api.ProjectTrail, changes []projectTrailChangeJSON) {
	styles := newStatusStyles(w)
	label := func(s string) string { return styles.render(styles.yellow, s) }
	status := func(s string) string {
		if style, ok := trailStatusColor(styles, trail.Status(s)); ok {
			return styles.render(style, s)
		}
		return s
	}

	project := item.ProjectID
	if item.Project != nil && item.Project.Reference.Forge != "" && item.Project.Reference.Project != "" {
		project = item.Project.Reference.Forge + "/" + item.Project.Reference.Project
	}
	type field struct{ name, value string }
	fields := []field{
		{"ID:", tuiutil.SanitizeTerminalLabel(item.ID)},
		{"Project:", tuiutil.SanitizeTerminalLabel(project)},
		{"Status:", status(tuiutil.SanitizeTerminalLabel(item.Status))},
	}
	if trailURL := strings.TrimSpace(item.URL); trailURL != "" {
		fields = append(fields, field{"URL:", tuiutil.SanitizeTerminalText(trailURL)})
	}
	if len(item.Assignees) > 0 {
		fields = append(fields, field{"Assignees:", tuiutil.SanitizeTerminalLabel(strings.Join(item.Assignees, ", "))})
	}
	if t := strings.TrimSpace(item.Type); t != "" {
		fields = append(fields, field{"Type:", tuiutil.SanitizeTerminalLabel(t)})
	}
	if p := strings.TrimSpace(item.Priority); p != "" && p != string(trail.PriorityNone) {
		fields = append(fields, field{"Priority:", tuiutil.SanitizeTerminalLabel(p)})
	}
	const timeLayout = "2006-01-02T15:04:05Z07:00"
	if !item.CreatedAt.IsZero() {
		fields = append(fields, field{"Created:", item.CreatedAt.Format(timeLayout)})
	}
	if !item.UpdatedAt.IsZero() {
		fields = append(fields, field{"Updated:", item.UpdatedAt.Format(timeLayout)})
	}
	if item.ClosedAt != nil {
		fields = append(fields, field{"Closed:", item.ClosedAt.Format(timeLayout)})
	}
	width := 0
	for _, f := range fields {
		width = max(width, len(f.name))
	}

	fmt.Fprintln(w, label(fmt.Sprintf("Trail #%d: %s", item.Number, tuiutil.SanitizeTerminalLabel(item.Title))))
	for _, f := range fields {
		fmt.Fprintf(w, "  %s %s\n", label(f.name+strings.Repeat(" ", width-len(f.name))), f.value)
	}
	if len(changes) > 0 {
		fmt.Fprintf(w, "  %s\n", label("Changes:"))
		headers := []string{colHeaderChange, colHeaderBranch, colHeaderStatus, colHeaderURL}
		rows := make([][]string, len(changes))
		for i, c := range changes {
			// Without a valid repository name there is no selector to show;
			// the branch still identifies the change.
			change := c.selector
			if change == "" {
				change = "-"
			}
			if c.Current {
				change += " (current)"
			}
			rows[i] = []string{change, tuiutil.SanitizeTerminalLabel(c.Branch), tuiutil.SanitizeTerminalLabel(c.Status), tuiutil.SanitizeTerminalText(c.URL)}
		}
		// Pad on the plain text, then style, so color codes do not skew alignment.
		widths := columnWidths(headers, rows)
		writeRow := func(cells []string, style func(col int, cell string) string) {
			var b strings.Builder
			b.WriteString("    ")
			for i, cell := range cells {
				padded := cell
				if i < len(cells)-1 {
					padded += strings.Repeat(" ", widths[i]-lipgloss.Width(cell)+2)
				}
				b.WriteString(style(i, padded))
			}
			fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
		}
		writeRow(headers, func(_ int, cell string) string { return label(cell) })
		for _, r := range rows {
			writeRow(r, func(col int, cell string) string {
				if col == 2 {
					return strings.Replace(cell, strings.TrimSpace(cell), status(strings.TrimSpace(cell)), 1)
				}
				return cell
			})
		}
	}
	for _, c := range changes {
		if !c.gatesLoaded {
			continue
		}
		name := c.selector
		if name == "" {
			name = tuiutil.SanitizeTerminalLabel(c.Branch)
		}
		fmt.Fprintf(w, "  %s\n", label("Gates for "+name+":"))
		// Indent the single-trail block one level under its change.
		var block strings.Builder
		printTrailMergeability(&block, styles, label, c.gates)
		for line := range strings.Lines(block.String()) {
			fmt.Fprint(w, "  "+line)
		}
	}
	if item.Body != "" {
		fmt.Fprintf(w, "\n%s\n%s\n", label("Description:"), tuiutil.SanitizeTerminalText(item.Body))
	}
}

func newProjectTrailListCmd() *cobra.Command {
	var status, cursor string
	var pageSize int
	var mine bool
	cmd := &cobra.Command{
		Use: "list", Short: "List trails in a project",
		Long: "List trails in one explicitly selected --project, newest update first, without requiring a checkout. --project is required even inside a clone; --repo only filters within that project. Requests go directly to the project's assigned cell. --status filters on the server before pagination. Use --page-token with the same project and filters to continue. Omitting --status includes all lifecycle states.\n\n" +
			"--mine instead lists the trails you authored or are assigned, across every project and region, latest activity first. It returns one page of up to --limit trails and takes no --project, --repo, or --page-token; a region that cannot be reached is reported as a warning.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if status != "" && !validProjectTrailStatus(status) {
				return errors.New("project trail status must be open or closed")
			}
			if pageSize < 1 || pageSize > trailListServerMaxLimit {
				return errors.New("--limit must be between 1 and 100")
			}
			if mine {
				if projectTrailProjectFlag(cmd) != "" || trailRepoFlag(cmd) != "" || cursor != "" {
					return errors.New("--mine lists across every project; it cannot be combined with --project, --repo, or --page-token")
				}
				out, err := listMyProjectTrails(cmd, status, pageSize)
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), out)
				}
				return printMyProjectTrails(cmd.OutOrStdout(), cmd.ErrOrStderr(), out)
			}
			page, err := listProjectTrails(cmd, status, pageSize, cursor)
			if err != nil {
				return err
			}
			items := page.Items
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), page)
			}
			project := projectTrailProjectFlag(cmd)
			if err := printTable(cmd.OutOrStdout(), []string{colHeaderProject, "NUMBER", "ID", colHeaderStatus, colHeaderTitle}, items, func(t api.ProjectTrail) []string {
				return []string{tuiutil.SanitizeTerminalLabel(project), strconv.Itoa(t.Number), t.ID, tuiutil.SanitizeTerminalLabel(t.Status), tuiutil.SanitizeTerminalLabel(t.Title)}
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
	cmd.Flags().StringVar(&status, "status", "", "Filter by status before pagination (open, closed)")
	cmd.Flags().IntVar(&pageSize, "limit", 50, "Page size (1-100)")
	cmd.Flags().StringVar(&cursor, "page-token", "", "Continue from nextPageToken")
	cmd.Flags().BoolVar(&mine, "mine", false, "List trails you authored or are assigned, across every project and region")
	addJSONFlag(cmd)
	return cmd
}

func validProjectTrailStatus(status string) bool {
	return status == string(trail.StatusOpen) || status == string(trail.StatusClosed)
}

type projectTrailFields struct {
	Title, Body, Status, Type, Priority string
	Assignees                           []string
}

func (f *projectTrailFields) flags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.Title, "title", "", "Trail title")
	cmd.Flags().StringVar(&f.Body, "body", "", "Trail description (--body= clears it on update)")
	cmd.Flags().StringVar(&f.Status, "status", "", "Lifecycle status (open, closed)")
	cmd.Flags().StringVar(&f.Type, "type", "", fmt.Sprintf("Type (%s)", formatValidTypes()))
	cmd.Flags().StringVar(&f.Priority, "priority", "", fmt.Sprintf("Priority (%s)", formatValidPriorities()))
	cmd.Flags().StringSliceVar(&f.Assignees, "assignee", nil, "Assignee logins; replaces the complete list (--assignee= clears it)")
}

func validateProjectTrailStatusFlag(cmd *cobra.Command, status string) error {
	if cmd.Flags().Changed(cmdStatus) && !validProjectTrailStatus(status) {
		return errors.New("project trail status must be open or closed (merged belongs to a change)")
	}
	return nil
}

func (f *projectTrailFields) validate(cmd *cobra.Command, creating bool) error {
	if (creating || cmd.Flags().Changed("title")) && strings.TrimSpace(f.Title) == "" {
		return errors.New("--title is required and must not be empty")
	}
	if err := validateProjectTrailStatusFlag(cmd, f.Status); err != nil {
		return err
	}
	return validateTrailCreateEnums(cmd, f.Type, f.Priority)
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
			if patch == (api.ProjectTrailUpdateRequest{}) && !assigning {
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
				if len(add) > 0 || len(remove) > 0 {
					assignees, err = mergeProjectTrailAssignees(cmd.Context(), current, add, remove)
					if err != nil {
						return err
					}
				}
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
			return printProjectTrail(cmd, out, target.Change, nil)
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

// Local creation publishes the branch, then links it atomically with intent.
// The CLI never compensates a failed request by deleting shared remote work.
func newProjectTrailCreateCmd() *cobra.Command {
	var fields projectTrailFields
	var branch, base, action, key string
	var noBranch bool
	cmd := &cobra.Command{
		Use: cmdCreate, Short: "Create a trail and publish its branch",
		Long: "Create a repository-scoped project trail, then link its branch. In the current repository, publish the local branch first; no separate commit or push is required. Uses the current feature branch, or derives a new branch from the title when on the base branch. Use --branch to choose a branch, or --no-branch for intent only. With --no-branch, the current or --repo repository becomes the Trail scope and requires repository write access; --project without --repo creates unscoped intent instead. Repository writers can create scoped trails and link branches. Unscoped creation requires project management. With a branch, --idempotency-key applies only to linking; if linking fails, retry with trail link rather than creating another trail. With --repo, no local branch is published: link existing remote work or use --branch-action create to create it on the server from --base. This command does not commit changes or change the local checkout. Branches are preserved if creation fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := fields.validate(cmd, true); err != nil {
				return err
			}
			if noBranch && (cmd.Flags().Changed("branch") || cmd.Flags().Changed("base") || cmd.Flags().Changed("branch-action")) {
				return errors.New("--no-branch cannot be combined with --branch, --base, or --branch-action")
			}
			request := api.ProjectTrailCreateRequest{
				Title: fields.Title, Body: fields.Body, Status: fields.Status,
				Type: fields.Type, Priority: fields.Priority, Assignees: fields.Assignees,
			}
			if !noBranch {
				var err error
				branch, base, err = resolveProjectTrailCreateBranch(cmd, fields.Title, branch, base)
				if err != nil {
					return err
				}
			}
			if err := validateProjectTrailBranch(cmd, branch, action); err != nil {
				return err
			}
			if !noBranch || trailRepoFlag(cmd) != "" || projectTrailProjectFlag(cmd) == "" {
				change := projectTrailChangeRequest(fields.Status, "", branch, base, action)
				return createRepositoryScopedProjectTrail(cmd, request, change, key)
			}
			target, err := resolveProjectTrailCollection(cmd)
			if err != nil {
				return err
			}
			key = projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)
			var out api.ProjectTrail
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodPost, target.BasePath, request, http.Header{"Idempotency-Key": {key}}, &out); err != nil {
				return fmt.Errorf("create project trail (retry with --idempotency-key %s and the same fields): %w", key, err)
			}
			if err := target.validateResponse(out); err != nil {
				return err
			}
			return printProjectTrail(cmd, out, nil, nil)
		},
	}
	fields.flags(cmd)
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "Create intent scoped to the current or --repo repository; --project alone creates unscoped intent")
	addProjectTrailCreationFlags(cmd, &branch, &base, &action, &key)
	addJSONFlag(cmd)
	return cmd
}

func addProjectTrailCreationFlags(cmd *cobra.Command, branch, base, action, key *string) {
	cmd.Flags().StringVar(branch, "branch", "", "Branch to use (defaults to the current feature branch; create derives one from the title on the base branch)")
	cmd.Flags().StringVar(base, "base", "", "Base branch for new code work")
	cmd.Flags().StringVar(action, "branch-action", trailBranchActionLink, "Branch action: link existing remote work, or create a new branch")
	cmd.Flags().StringVar(key, "idempotency-key", "", "Retry key for linking a branch; with intent-only creation, requires project management")
}

func validateProjectTrailBranch(cmd *cobra.Command, branch, action string) error {
	if action != trailBranchActionLink && action != cmdCreate {
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

// The branch name is the Change title, so create and its link retry agree.
func projectTrailChangeRequest(status, repoID, branch, base, action string) api.ChangeCreateRequest {
	return api.ChangeCreateRequest{RepositoryID: repoID, Title: branch, BranchName: branch, Base: base, BranchAction: action, Status: status}
}
