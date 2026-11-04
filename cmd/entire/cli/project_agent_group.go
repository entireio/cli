package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

// projectAgentPresetsPath is the preset catalog. It is not project-scoped;
// any cell serves it, so it is read through the project's cell client.
const projectAgentPresetsPath = "/api/v1/agents/presets"

// newProjectAgentCmd is the `entire project agent` group: the review agents a
// project runs on pushes, configured in the API (they replaced the repo-file
// trail runners).
func newProjectAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "List and add the project's review agents",
		Long: `Manage the review agents an Entire project runs on pushes.

Agents are configured per project in Entire, not in repository files. Each
reviews the Changes of the repositories it selects and reports findings and a
signal (risk, confidence, …) on the trail.

Preset agents are maintained by Entire and run on free credit, so they work
before billing is set up. A custom agent runs only once the project's payer is
enrolled in billing.

The project is the current repository's (gh/<owner> or et/<project>) unless
--project names another.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().String("project", "", "Project to manage: gh/<owner> or et/<project> (default: the current repository's)")
	cmd.AddCommand(newProjectAgentListCmd())
	cmd.AddCommand(newProjectAgentAddCmd())
	return cmd
}

// projectAgentTarget is a resolved project: a client for its processing cell
// and the base of its configuration routes.
type projectAgentTarget struct {
	client    *api.Client
	projectID string
	host      string
	project   string
	// base is /api/v1/{host}/{project}; agents and environments hang off it.
	base string
	// repoForge/repoOwner/repoName are the repository the project was
	// derived from; empty when --project named it and no repository was read.
	repoForge, repoOwner, repoName string
}

func (t *projectAgentTarget) label() string { return t.host + "/" + t.project }

func (t *projectAgentTarget) agentsPath() string { return t.base + "/agents" }

func (t *projectAgentTarget) agentPath(id string) string {
	return t.agentsPath() + "/" + url.PathEscape(id)
}

func (t *projectAgentTarget) environmentsPath() string { return t.base + "/environments" }

// resolveProjectAgentTarget resolves --project, or the current repository's
// project, to its processing cell. A test replaces it to point at a fake.
var resolveProjectAgentTarget = func(cmd *cobra.Command) (*projectAgentTarget, error) {
	ctx := cmd.Context()
	t := &projectAgentTarget{}
	if ref := projectAgentProjectFlag(cmd); ref != "" {
		host, project, err := parseTrailProjectRef(ref)
		if err != nil {
			return nil, err
		}
		t.host, t.project = host, project
	} else {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w (or pass --project gh/<owner> or et/<project>)", err)
		}
		t.host, t.project = forge, strings.ToLower(owner)
		t.repoForge, t.repoOwner, t.repoName = forge, owner, repo
	}
	resolved, err := resolveProjectTrailCollectionFor(ctx, t.host, t.project, insecureHTTPRequested(cmd))
	if err != nil {
		return nil, renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), "", err)
	}
	t.client, t.projectID = resolved.Client, resolved.ProjectID
	t.base = "/api/v1/" + url.PathEscape(t.host) + "/" + url.PathEscape(t.project)
	return t, nil
}

// resolveProjectAgentRepoID is the repository id (ULID) of forge/owner/repo,
// which agents select repositories by. A test replaces it.
var resolveProjectAgentRepoID = func(ctx context.Context, insecure bool, forge, owner, repo string) (string, error) {
	_, repoID, err := newTrailAPIClient(ctx, insecure, forge, owner, repo)
	if err != nil {
		return "", err
	}
	if repoID == "" {
		return "", fmt.Errorf("repository %s/%s/%s has no id in Entire", forge, owner, repo)
	}
	return repoID, nil
}

func projectAgentProjectFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("project") //nolint:errcheck // registered on the agent group
	return strings.TrimSpace(v)
}

// currentRepoID is the id of the repository the command runs in, which must
// belong to the target project. Commands that act on "this repo" (add's
// default selection) use it.
func (t *projectAgentTarget) currentRepoID(cmd *cobra.Command) (id, label string, err error) {
	forge, owner, repo := t.repoForge, t.repoOwner, t.repoName
	if repo == "" {
		if forge, owner, repo, err = resolveTrailRemote(cmd.Context()); err != nil {
			return "", "", err
		}
	}
	label = forge + "/" + owner + "/" + repo
	id, err = resolveProjectAgentRepoID(cmd.Context(), insecureHTTPRequested(cmd), forge, owner, repo)
	if err != nil {
		return "", "", renderDataAPIAuthError(cmd.Context(), cmd.ErrOrStderr(), owner+"/"+repo, err)
	}
	// A repository addressed under the project's own namespace belongs to it.
	// Otherwise the namespace proves nothing either way, so the project's own
	// repository list decides; an unreadable list cannot confirm membership.
	if forge == t.host && strings.EqualFold(owner, t.project) {
		return id, label, nil
	}
	if _, ok := resolveProjectRepoNames(cmd.Context(), t.projectID)[id]; !ok {
		return "", "", fmt.Errorf("repository %s is not in project %s (or its repositories could not be listed)", label, t.label())
	}
	return id, label, nil
}

// listAgents reads the project's active agents; repoID, when set, keeps the
// ones whose selection includes that repository.
func (t *projectAgentTarget) listAgents(ctx context.Context, repoID string) ([]api.ProjectAgent, error) {
	q := url.Values{}
	if repoID != "" {
		q.Set("repository_id", repoID)
	}
	agents, err := api.ListProjectConfig[api.ProjectAgent](ctx, t.client, t.agentsPath(), q)
	if err != nil {
		return nil, fmt.Errorf("list agents of %s: %w", t.label(), err)
	}
	return agents, nil
}

func (t *projectAgentTarget) listPresets(ctx context.Context) ([]api.ProjectAgentPreset, error) {
	presets, err := api.ListProjectConfig[api.ProjectAgentPreset](ctx, t.client, projectAgentPresetsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("list agent presets: %w", err)
	}
	return presets, nil
}

func (t *projectAgentTarget) listEnvironments(ctx context.Context) ([]api.ProjectEnvironment, error) {
	envs, err := api.ListProjectConfig[api.ProjectEnvironment](ctx, t.client, t.environmentsPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("list environments of %s: %w", t.label(), err)
	}
	return envs, nil
}

func (t *projectAgentTarget) createAgent(ctx context.Context, body api.ProjectAgentWrite) (api.ProjectAgent, error) {
	var out api.ProjectAgent
	err := t.client.CreateProjectConfig(ctx, t.agentsPath(), body, &out)
	return out, err //nolint:wrapcheck // callers name the agent
}

// replaceAgent returns the agent as the server saved it, with the etag and
// version the next write needs.
func (t *projectAgentTarget) replaceAgent(ctx context.Context, id, etag string, body api.ProjectAgentWrite) (api.ProjectAgent, error) {
	var out api.ProjectAgent
	err := t.client.ReplaceProjectConfig(ctx, t.agentPath(id), etag, body, &out)
	return out, err //nolint:wrapcheck // callers name the agent
}

func (t *projectAgentTarget) createEnvironment(ctx context.Context, body api.ProjectEnvironmentInput) (api.ProjectEnvironment, error) {
	var out api.ProjectEnvironment
	err := t.client.CreateProjectConfig(ctx, t.environmentsPath(), body, &out)
	return out, err //nolint:wrapcheck // callers name the environment
}

// liveRepositoryIDs is an agent's selection without the repositories the
// project no longer owns: the API refuses a write that keeps one.
func liveRepositoryIDs(a api.ProjectAgent) []string {
	out := make([]string, 0, len(a.RepositoryIDs))
	for _, id := range a.RepositoryIDs {
		if !slices.Contains(a.StaleRepositoryIDs, id) {
			out = append(out, id)
		}
	}
	return out
}

// resolveProjectRepoNames maps the project's repository ids to names, so a
// message can say which repositories an agent reviews. Best effort: an id it
// cannot name is shown as the id. A test replaces it.
var resolveProjectRepoNames = func(ctx context.Context, projectID string) map[string]string {
	names := map[string]string{}
	if projectID == "" {
		return names
	}
	c, err := coreapi.New()
	if err != nil {
		return names
	}
	repos, _, err := boundedList(ctx, coreListFetchBudget, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.Repo, coreapi.OptString, error) {
		out, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{ProjectId: projectID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, fmt.Errorf("list project repos: %w", err)
		}
		return out.Repos, out.NextPageToken, nil
	})
	if err != nil {
		return names
	}
	for _, r := range repos {
		names[r.ID] = r.Name
	}
	return names
}

// repoNames renders ids with their names where known, sorted.
func repoNames(names map[string]string, ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := names[id]; n != "" {
			out = append(out, n)
		} else {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// runProjectAgentPrompt runs form where the user can see it — stderr when it
// is a terminal, else the controlling terminal (runPromptFormThen) — so a
// question never lands in --json output or a captured diff. An abort prints
// "<action> cancelled." there and reports cancelled, which the caller must
// treat as "change nothing", not as an answer.
func runProjectAgentPrompt(ctx context.Context, errW io.Writer, action string, form *huh.Form) (cancelled bool, err error) {
	_, err = runPromptFormThen(ctx, errW, form, nil, func(render io.Writer, formErr error) error {
		if formErr == nil {
			return nil
		}
		if cerr := handleFormCancellation(render, action, formErr); cerr != nil {
			return cerr
		}
		cancelled = true
		return nil
	})
	return cancelled, err
}

// projectAgentCustomLabel names an agent that runs no preset.
const projectAgentCustomLabel = "custom"

// projectAgentPresetLabel is how an agent's origin shows in tables.
func projectAgentPresetLabel(a api.ProjectAgent) string {
	if a.Preset == nil {
		return projectAgentCustomLabel
	}
	return fmt.Sprintf("%s@%d", a.Preset.ID, a.Preset.Version)
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}
