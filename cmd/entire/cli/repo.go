package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// newRepoCmd is the hidden `entire repo` command group: control-plane
// repository lifecycle (create, list within a project, get, delete) on the
// Entire control plane. Git content operations (clone, log, diff, …) are
// intentionally out of scope here. Surfaced via `entire labs`.
func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "repo",
		Short:  "Manage Entire repositories",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newRepoCreateCmd())
	cmd.AddCommand(newRepoListCmd())
	cmd.AddCommand(newRepoGetCmd())
	cmd.AddCommand(newRepoDeleteCmd())
	cmd.AddCommand(newRepoMirrorCmd())
	return cmd
}

// repoColumns is the human table/field view of a repo, shared by list and
// get. CLUSTER/STATE come from optional fields, shown as "-" when unset.
var repoColumns = []string{"ID", "NAME", "PROJECT", "CLUSTER", "STATE"}

func repoRow(r coreapi.Repo) []string {
	state := ""
	if v, ok := r.State.Get(); ok {
		state = string(v)
	}
	return []string{r.ID, r.Name, r.OwningProjectId, r.ClusterHost.Or("-"), state}
}

// repoRemoteURL synthesizes the entire:// clone/remote URL for a repo from
// its resolved cluster host and path — the form `git clone` and
// `git remote add` accept, which git-remote-entire reads back as the repo
// slug from the URL path. Returns "" when either coordinate is missing (a
// still-provisioning repo may not have them yet); a half-formed URL is worse
// than none.
func repoRemoteURL(r coreapi.Repo) string {
	host := strings.TrimSpace(r.ClusterHost.Or(""))
	path := strings.TrimSpace(r.Path.Or(""))
	if host == "" || path == "" {
		return ""
	}
	return "entire://" + host + "/" + strings.TrimPrefix(path, "/")
}

// repoCreateOutput renders a created repo as JSON with a synthesized `remote`
// field merged in — the entire:// URL callers paste into `git clone` or
// `git remote add`. The repo carries a custom marshaler plus arbitrary
// additional properties, so it can't simply be embedded in a wrapper struct;
// instead it's round-tripped through its own encoder and the remote is merged
// into the resulting object. The synthesis only fills a gap: if the wire
// object already carries a `remote` (a future first-class field, or one
// arriving via additional properties) it's left untouched, so the
// server-provided value always wins. The field is omitted when the clone
// coordinates aren't resolvable yet rather than emitted half-formed.
func repoCreateOutput(r *coreapi.Repo) (any, error) {
	if r == nil {
		return nil, errors.New("nil repo")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode repo: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode repo: %w", err)
	}
	if _, ok := obj["remote"]; !ok {
		if remote := repoRemoteURL(*r); remote != "" {
			encoded, err := json.Marshal(remote)
			if err != nil {
				return nil, fmt.Errorf("encode remote: %w", err)
			}
			obj["remote"] = encoded
		}
	}
	return obj, nil
}

func newRepoCreateCmd() *cobra.Command {
	var (
		projectID   string
		clusterHost string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a repository in a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				projID, err := resolveProjectRef(ctx, c, projectID)
				if err != nil {
					return nil, err
				}
				body := &coreapi.CreateRepoInputBody{
					Name:      args[0],
					ProjectId: projID,
				}
				if clusterHost != "" {
					body.ClusterHost = coreapi.NewOptString(clusterHost)
				}
				created, err := c.CreateRepo(ctx, body)
				if err != nil {
					return nil, err
				}
				return repoCreateOutput(created)
			})
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "owning project (name or ULID) (required)")
	cmd.Flags().StringVar(&clusterHost, "cluster-host", "", "public host of the cluster to pin the repo to (defaults to the jurisdiction default)")
	markRequired(cmd, "project")
	return cmd
}

func newRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List repositories in a project",
		Long:  "List repositories in a project, addressed by name or ULID.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, repoColumns, repoRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Repo, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				out, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{ProjectId: projID})
				if err != nil {
					return nil, err
				}
				return out.Repos, nil
			})
		},
	}
}

func newRepoGetCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "get <repo>",
		Short: "Show a repository by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, repoColumns, repoRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Repo, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				return c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	return cmd
}

func newRepoDeleteCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "delete <repo>",
		Short: "Delete a repository by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return err
				}
				if err := c.DeleteRepo(ctx, coreapi.DeleteRepoParams{RepoId: repoID}); err != nil {
					return err
				}
				cmd.Printf("Deleted repo %s\n", args[0])
				return nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	return cmd
}

// bindRepoProjectFlag wires the shared --project scope used to resolve a repo
// addressed by name (a repo name is unique only within its project). Ignored
// when the repo arg is already a ULID.
func bindRepoProjectFlag(cmd *cobra.Command, project *string) {
	cmd.Flags().StringVar(project, "project", "", "owning project (name or ULID); required when <repo> is a name")
}
