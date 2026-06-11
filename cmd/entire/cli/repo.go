package cli

import (
	"context"

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
				body := &coreapi.CreateRepoInputBody{
					Name:      args[0],
					ProjectId: projectID,
				}
				if clusterHost != "" {
					body.ClusterHost = coreapi.NewOptString(clusterHost)
				}
				return c.CreateRepo(ctx, body)
			})
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "owning project ULID (required)")
	cmd.Flags().StringVar(&clusterHost, "cluster-host", "", "public host of the cluster to pin the repo to (defaults to the jurisdiction default)")
	markRequired(cmd, "project")
	return cmd
}

func newRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List repositories in a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, repoColumns, repoRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Repo, error) {
				out, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{ProjectId: args[0]})
				if err != nil {
					return nil, err
				}
				return out.Repos, nil
			})
		},
	}
}

func newRepoGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <repo>",
		Short: "Show a repository by ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, repoColumns, repoRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Repo, error) {
				sc, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: args[0]})
				if err != nil {
					return nil, err
				}
				return sc, nil
			})
		},
	}
}

func newRepoDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <repo>",
		Short: "Delete a repository by ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				if err := c.DeleteRepo(ctx, coreapi.DeleteRepoParams{RepoId: args[0]}); err != nil {
					return err
				}
				cmd.Printf("Deleted repo %s\n", args[0])
				return nil
			})
		},
	}
}
