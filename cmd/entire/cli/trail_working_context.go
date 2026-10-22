package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// trailWorkingContext keeps branch identity internal. User selectors always name
// project intent; only repository subresource requests use Work.ID/Number.
type trailWorkingContext struct {
	Target            *projectTrailTarget
	Parent            api.ProjectTrail
	ETag              string
	Client            *api.Client
	BasePath          string
	RepoID            string
	Host, Owner, Repo string
	Work              api.TrailResource
}

func (t *trailWorkingContext) description() string {
	if t.Target == nil {
		return fmt.Sprintf("trail #%d", t.Work.Number)
	}
	return fmt.Sprintf("trail #%d (%s/%s/%s / %s)", t.Parent.Number, t.Host, t.Owner, t.Repo, tuiutil.SanitizeTerminalLabel(changeBranchName(t.Work)))
}

// resolveProjectTrailWorkingContext is projectTrailMode.workingContext.
// localOnly is used by checkout/resume, which act on the local clone: checkout
// rejects --repo and resume ignores it, so neither targets a foreign repository.
func resolveProjectTrailWorkingContext(cmd *cobra.Command, selector, branch string, localOnly bool) (_ *trailWorkingContext, err error) {
	if change, ok := parseProjectTrailChangeSelector(selector); ok {
		if strings.TrimSpace(branch) != "" {
			return nil, errChangeSelectorWithBranch
		}
		return resolveProjectTrailChangeContext(cmd, change, localOnly)
	}
	ctx := cmd.Context()
	if selector != "" {
		if err := validateProjectTrailSelector(selector); err != nil {
			return nil, err
		}
	}
	repoOverride := trailRepoFlag(cmd)
	if localOnly {
		repoOverride = ""
	} else if err := ensureTrailRepoHasTarget(cmd, selector != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
		return nil, err
	}
	host, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return nil, err
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), host, owner, repo)
	if err != nil {
		return nil, renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repo, err)
	}
	// A change in another repository reads nothing about this one, so only
	// lookups that stay here may record this clone's trails enablement.
	foreign := false
	if repoOverride == "" {
		defer func() {
			if !foreign {
				noteTrailCommandEnablement(ctx, client, err)
			}
		}()
	}
	base, err := trailRepoBasePath(host, owner, repo, repoID)
	if err != nil {
		return nil, err
	}

	var target *projectTrailTarget
	discovered := ""
	if selector == "" {
		branch, err = resolveTrailBranch(ctx, branch)
		if err != nil {
			return nil, err
		}
		work, err := findTrailByBranchAtPath(ctx, client, base, branch)
		if err != nil {
			return nil, err
		}
		target, err = openTrailParentTarget(cmd, work, fmt.Sprintf("branch %q", branch))
		if err != nil {
			return nil, err
		}
		discovered = work.ID
	} else {
		forge, project, err := trailProjectReferenceOr(cmd, host, owner)
		if err != nil {
			return nil, err
		}
		target, err = resolveProjectTrailCollectionFor(ctx, forge, project, trailInsecureHTTP(cmd))
		if err != nil {
			return nil, renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), "", err)
		}
		target, err = target.resolveSelector(ctx, selector)
		if err != nil {
			return nil, err
		}
	}
	parent, etag, err := target.read(ctx)
	if err != nil {
		return nil, err
	}
	choice := trailChangeChoice{ID: discovered, Branch: branch, LocalOnly: localOnly}
	if repoOverride == "" {
		choice.Local = repoID
		if !trailChangeWriteCommands[cmd.Name()] {
			choice.Preferred, _ = GetCurrentBranch(ctx) //nolint:errcheck // no preference outside a checkout or on detached HEAD
		}
	}
	if repoOverride != "" || localOnly {
		choice.Scope = repoID
	}
	choice.PreferUnapproved = cmd.Name() == "approve"
	retry := func(change string) string {
		return trailChangeRetryCommand(cmd, selector, change, target.Host+"/"+target.Project)
	}
	reviews := func(changes []api.ChangeSummary) map[string]trailChangeReviews {
		return lookupTrailChangeReviews(ctx, trailInsecureHTTP(cmd), target, trailRepoAccess{Client: client, BasePath: base, RepoID: repoID}, changes)
	}
	selected, err := choice.selectFrom(parent, retry, reviews)
	if err != nil {
		return nil, err
	}
	if selected.RepositoryID == repoID {
		return finishTrailWorkingContext(ctx, target, parent, etag, selected, client, base, host, owner, repo, repoID)
	}
	foreign = true
	return finishForeignTrailWorkingContext(cmd, target, parent, etag, selected)
}

// trailChangeWriteCommands act on the selected change itself, so a trail with
// several changes must be disambiguated explicitly rather than by whichever
// branch happens to be checked out.
var trailChangeWriteCommands = map[string]bool{"approve": true, "unlink": true, "merge": true}

// finishForeignTrailWorkingContext opens the repository a sole change in
// another repository belongs to. Repository is the server's basename in the
// trail's project; the opened repository must be the one the change names.
func finishForeignTrailWorkingContext(cmd *cobra.Command, target *projectTrailTarget, parent api.ProjectTrail, etag string, selected api.ChangeSummary) (*trailWorkingContext, error) {
	ctx := cmd.Context()
	if !validTrailChangeRepoName(selected.Repository) {
		return nil, fmt.Errorf("%s's only change is on branch %q in a repository whose name has not resolved yet; retry shortly", target.label(), tuiutil.SanitizeTerminalLabel(selected.Branch))
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), target.Host, target.Project, selected.Repository)
	if err != nil {
		return nil, renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), target.Project+"/"+selected.Repository, err)
	}
	if repoID != selected.RepositoryID {
		return nil, fmt.Errorf("repository %s/%s does not match the trail's change", target.Project, selected.Repository)
	}
	base, err := trailRepoBasePath(target.Host, target.Project, selected.Repository, repoID)
	if err != nil {
		return nil, err
	}
	return finishTrailWorkingContext(ctx, target, parent, etag, selected, client, base, target.Host, target.Project, selected.Repository, repoID)
}

// resolveProjectTrailChangeContext selects branch work by its <repo>/<number>
// and reaches its project trail through the change's parent, so it needs no
// collection lookup and works for merged work whose branch is gone.
func resolveProjectTrailChangeContext(cmd *cobra.Command, sel projectTrailChangeSelector, localOnly bool) (*trailWorkingContext, error) {
	ctx := cmd.Context()
	change, err := resolveProjectTrailChange(cmd, sel, localOnly)
	if err != nil {
		return nil, err
	}
	target, err := openTrailParentTarget(cmd, change.Work, "change "+sel.String())
	if err != nil {
		return nil, err
	}
	parent, etag, err := target.read(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range parent.Changes {
		if item.ID == change.Work.ID && item.RepositoryID == change.RepoID {
			return finishTrailWorkingContext(ctx, target, parent, etag, item, change.Client, change.BasePath, change.Host, change.Owner, change.Repo, change.RepoID)
		}
	}
	return nil, fmt.Errorf("trail #%d does not list %s (it may be hidden by access filtering)", parent.Number, sel)
}

// finishTrailWorkingContext rereads the selected work through the owning
// project route to recheck containment, not a repo-local number that could
// name unrelated work. The repo client is retained for reviews.
func finishTrailWorkingContext(ctx context.Context, target *projectTrailTarget, parent api.ProjectTrail, etag string, selected api.ChangeSummary, client *api.Client, base, host, owner, repo, repoID string) (*trailWorkingContext, error) {
	if !looksLikeULID(selected.ID) {
		return nil, errors.New("selected branch has no valid backing identity")
	}
	var out api.ChangeResource
	_, err := target.Client.ProjectTrailRequest(ctx, http.MethodGet, target.path()+"/changes/"+url.PathEscape(selected.ID), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("read trail branch: %w", err)
	}
	if out.ID != selected.ID || out.TrailID != parent.ID || out.RepositoryID != repoID || out.Number <= 0 ||
		// An unlinked change may list no branch on its trail; the identity
		// checks above prove containment then.
		(selected.Branch != "" && changeBranchName(out.TrailResource) != selected.Branch) {
		return nil, errors.New("trail branch response does not match the selected repository/branch")
	}
	out.Parent = &api.TrailParentReference{ID: parent.ID, Number: parent.Number, ProjectID: parent.ProjectID,
		Host: target.Host, Project: target.Project, Path: target.path()}
	client.SetTrailRoute(out.ID, trailNumberPathForBase(base, out.Number))
	return &trailWorkingContext{Target: target, Parent: parent, ETag: etag, Client: client, BasePath: base, RepoID: repoID,
		Host: host, Owner: owner, Repo: repo, Work: out.TrailResource}, nil
}

// changeBranchName is the branch a change summary lists: a merged change's
// read returns branch null and keeps the name in original_branch.
func changeBranchName(work api.TrailResource) string {
	if work.Branch != "" {
		return work.Branch
	}
	return work.OriginalBranch
}

// trailChangeChoice picks the one change a branch-level command acts on from
// a project trail's open (branch-bearing) changes.
type trailChangeChoice struct {
	// ID is the change branch discovery already found in this repository; a
	// same-named branch elsewhere in the trail must not make it ambiguous.
	ID string
	// Scope limits candidates to one repository ID: an explicit --repo, or
	// this clone for local-only commands. Empty means every repository.
	Scope string
	// Local is this clone's repository ID; empty under --repo.
	Local string
	// LocalOnly commands act on this clone, so changes elsewhere are listed
	// as needing another clone rather than silently left out.
	LocalOnly bool
	// Branch is an explicit --branch.
	Branch string
	// PreferUnapproved makes the suggested command name a change nobody has
	// approved yet, when review state is known (approve).
	PreferUnapproved bool
	// Preferred is the checked-out branch; it settles ambiguity only for a
	// change in this clone's repository.
	Preferred string
}

// selectFrom applies, in order: the discovered change, --branch, the checked-out branch, then a sole
// open change. Anything else lists the candidates as <repo>/<number> with their
// review state and names the command to re-run; retry renders that command for
// one change selector. reviews is called only when listing, and may be nil.
func (c trailChangeChoice) selectFrom(parent api.ProjectTrail, retry func(change string) string, reviews func([]api.ChangeSummary) map[string]trailChangeReviews) (api.ChangeSummary, error) {
	label := "trail #" + strconv.Itoa(parent.Number)
	if c.ID != "" {
		for _, item := range parent.Changes {
			if item.ID == c.ID {
				return item, nil
			}
		}
		return api.ChangeSummary{}, fmt.Errorf("%s does not list branch %q (it may be hidden by access filtering)", label, tuiutil.SanitizeTerminalLabel(c.Branch))
	}
	var candidates, elsewhere []api.ChangeSummary
	for _, item := range parent.Changes {
		switch {
		case item.Branch == "":
			// Merged or detached work has no branch left to act on.
		case c.Scope == "" || item.RepositoryID == c.Scope:
			candidates = append(candidates, item)
		case c.LocalOnly:
			elsewhere = append(elsewhere, item)
		}
	}
	if c.Branch != "" {
		// Narrow both lists, so a local-only miss still shows the branch's
		// change in another repository rather than reporting nothing.
		candidates, elsewhere = changesOnBranch(candidates, c.Branch), changesOnBranch(elsewhere, c.Branch)
	} else if c.Preferred != "" && c.Local != "" {
		for _, item := range candidates {
			if item.RepositoryID == c.Local && item.Branch == c.Preferred {
				return item, nil
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	state := map[string]trailChangeReviews{}
	if reviews != nil {
		state = reviews(append(slices.Clone(candidates), elsewhere...))
	}
	line := func(item api.ChangeSummary) string {
		if r, ok := state[item.ID]; ok {
			return trailChangeListing(item) + " (" + r.String() + ")"
		}
		return trailChangeListing(item)
	}
	var b strings.Builder
	switch {
	case len(candidates) == 0 && c.Branch != "":
		fmt.Fprintf(&b, "%s has no open change on branch %q%s", label, tuiutil.SanitizeTerminalLabel(c.Branch), c.scopeSuffix())
	case len(candidates) == 0:
		fmt.Fprintf(&b, "%s has no open changes%s", label, c.scopeSuffix())
	default:
		fmt.Fprintf(&b, "%s has %d open changes%s; choose one:", label, len(candidates), c.scopeSuffix())
	}
	for _, item := range candidates {
		b.WriteString("\n  " + line(item))
	}
	if len(elsewhere) > 0 {
		b.WriteString("\nChanges in other repositories need a clone of that repository:")
		for _, item := range elsewhere {
			b.WriteString("\n  " + line(item))
		}
	}
	if example, ok := c.exampleChange(candidates, state); ok {
		fmt.Fprintf(&b, "\nRun, for example: %s", retry(example))
	}
	return api.ChangeSummary{}, errors.New(b.String())
}

// exampleChange is the change the suggested command names: for approve, the
// first one with known state and no approvals; otherwise the first selectable.
func (c trailChangeChoice) exampleChange(candidates []api.ChangeSummary, state map[string]trailChangeReviews) (string, bool) {
	if c.PreferUnapproved {
		for _, item := range candidates {
			if r, ok := state[item.ID]; ok && len(r.Approved) == 0 {
				if selector, ok := trailChangeSelectorFor(item); ok {
					return selector, true
				}
			}
		}
	}
	for _, item := range candidates {
		if selector, ok := trailChangeSelectorFor(item); ok {
			return selector, true
		}
	}
	return "", false
}

func changesOnBranch(changes []api.ChangeSummary, branch string) []api.ChangeSummary {
	var out []api.ChangeSummary
	for _, item := range changes {
		if item.Branch == branch {
			out = append(out, item)
		}
	}
	return out
}

// trailChangeSelectorFor is the <repo>/<number> selector for a change, when
// both parts have resolved; otherwise it can only be named by its branch.
func trailChangeSelectorFor(item api.ChangeSummary) (string, bool) {
	if !validTrailChangeRepoName(item.Repository) || item.Number <= 0 {
		return "", false
	}
	return item.Repository + "/" + strconv.Itoa(item.Number), true
}

func (c trailChangeChoice) scopeSuffix() string {
	switch {
	case c.Scope == "":
		return ""
	case c.LocalOnly && c.Scope == c.Local:
		return " in this clone's repository"
	default:
		return " in the selected repository"
	}
}

// trailChangeListing is one candidate line: "<repo>/<number> - <title>". A
// repository whose name has not resolved yet can only be named by its branch.
func trailChangeListing(item api.ChangeSummary) string {
	line, ok := trailChangeSelectorFor(item)
	if !ok {
		return "branch " + tuiutil.SanitizeTerminalLabel(item.Branch) + " (repository not resolved yet; select it with --branch)"
	}
	if title := strings.TrimSpace(tuiutil.SanitizeTerminalLabel(item.Title)); title != "" {
		return line + " - " + title
	}
	return line + " - branch " + tuiutil.SanitizeTerminalLabel(item.Branch)
}

// trailChangeRetryCommand is the invocation with change in place of the trail
// selector. Positional arguments and the flags the user set are kept, so
// --project, -m, and --json still apply; --trail and --branch are dropped,
// since the change replaces them and cannot be combined with --branch.
// project is the trail's <forge>/<project>: a change selector resolves in
// --project or origin's owner, never --repo's, so a project the user implied
// through --repo is spelled out.
func trailChangeRetryCommand(cmd *cobra.Command, selector, change, project string) string {
	args := slices.Clone(cmd.Flags().Args())
	replaced := false
	for i, arg := range args {
		if !replaced && strings.TrimSpace(arg) == selector {
			args[i], replaced = change, true
		}
	}
	if !replaced {
		args = append([]string{change}, args...)
	}
	parts := []string{cmd.CommandPath()}
	for _, arg := range args {
		parts = append(parts, shellArg(arg))
	}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch {
		case f.Name == cmdTrail || f.Name == "branch": // --trail is named after the command
		case f.Value.Type() == "bool" && f.Value.String() == "true":
			parts = append(parts, "--"+f.Name)
		default:
			values := []string{f.Value.String()}
			if slice, ok := f.Value.(pflag.SliceValue); ok {
				values = slice.GetSlice()
			}
			if len(values) == 0 {
				// An explicitly empty list (--assignee=) still clears.
				parts = append(parts, "--"+f.Name+"=")
			}
			for _, value := range values {
				parts = append(parts, "--"+f.Name+"="+shellArg(value))
			}
		}
	})
	if !cmd.Flags().Changed("project") && trailRepoFlag(cmd) != "" {
		parts = append(parts, "--project="+shellArg(project))
	}
	return strings.Join(parts, " ")
}

func trailForDisplay(work api.TrailResource) api.TrailResource {
	if work.Parent != nil {
		work.ID, work.Number = work.Parent.ID, work.Parent.Number
	}
	return work
}
