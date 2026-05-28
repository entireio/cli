package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
	"github.com/entireio/cli/internal/entiredb/internal/clidial"
)

// clientFn yields a logged-in *api.Client. Resolved lazily so the
// caller can read --core-url / --context off the persistent flags
// (which cobra populates after AttachLifecycleCmds returns).
type clientFn func() (*api.Client, error)

// AttachLifecycleCmds attaches the control-plane repo verbs
// (list / create / delete) to root. cfg is used to resolve
// <repo> path arguments to repo IDs via the data plane. The `grant`
// verb moved to `entire-grant repo add`.
func AttachLifecycleCmds(root *cobra.Command, cfg *cliauth.Config, client clientFn) {
	root.AddCommand(newListReposCmd(client))
	root.AddCommand(newCreateRepoCmd(client))
	root.AddCommand(newDeleteRepoCmd(cfg, client))
}

func newListReposCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list [<project>]",
		Short: "List repos (in a project, or all you can pull)",
		Long: "Lists repos: with <project> (project name or ULID), every repo " +
			"in that project; without an arg, every repo you have pull on across " +
			"projects.",
		Example: "  # All pullable repos\n" +
			"  entire-repo list\n\n" +
			"  # Repos in one project by project name or ULID\n" +
			"  entire-repo list widgets",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				projectID, err := cliapi.ResolveProjectID(c, args[0])
				if err != nil {
					return err
				}
				data, err := c.GetJSON("/api/v1/projects/" + url.PathEscape(projectID) + "/repos")
				if err != nil {
					return err
				}
				return clicobra.PrintJSON(data)
			}
			// Cross-project path: SpiceDB returns ULIDs only, so chain
			// a /api/lookup call to enrich them with names + URLs.
			return cliapi.ListAccessibleAndResolve(c, "repo", "pull")
		},
	}
}

func newCreateRepoCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a repo",
		Long: "Creates a repo named <name> in --project (project name or ULID; required). " +
			"--cluster pins the repo to a specific cluster within the " +
			"caller's jurisdiction; omit to land on the jurisdiction's " +
			"default cluster. --object-format chooses the Git object hash " +
			"format (sha1 or sha256).\n\n" +
			"To register a GitHub mirror, use `entire-repo mirror create " +
			"<gh-url> <cluster>` — the server rejects mirror blocks on " +
			"POST /api/repos.",
		Example: "  # Plain repo (lands on the jurisdiction's default cluster)\n" +
			"  entire-repo create widgets --project my-project\n\n" +
			"  # SHA-256 repo\n" +
			"  entire-repo create widgets --project my-project --object-format sha256\n\n" +
			"  # Pin to a specific cluster\n" +
			"  entire-repo create widgets --project my-project --cluster purina",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef := clicobra.MustGetString(cmd, "project")
			clusterSlug := clicobra.MustGetString(cmd, "cluster")
			objectFormat := strings.ToLower(strings.TrimSpace(clicobra.MustGetString(cmd, "object-format")))
			switch objectFormat {
			case "sha1", "sha256":
				// ok
			default:
				return fmt.Errorf("--object-format must be sha1 or sha256, got %q", objectFormat)
			}
			c, err := client()
			if err != nil {
				return err
			}
			projectID, err := cliapi.ResolveProjectID(c, projectRef)
			if err != nil {
				return err
			}
			body := map[string]any{
				"projectId":    projectID,
				"name":         args[0],
				"objectFormat": objectFormat,
			}
			if clusterSlug != "" {
				body["clusterSlug"] = clusterSlug
			}
			data, err := c.PostJSON("/api/v1/repos", body)
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
	cmd.Flags().String("project", "", "project name or ULID (required)")
	cmd.Flags().String("cluster", "", "cluster slug to pin the new repo to (e.g. royalcanin); empty falls back to the jurisdiction's default cluster")
	cmd.Flags().String("object-format", "sha1", "Git object hash format for the repo (sha1 or sha256)")
	if err := cmd.MarkFlagRequired("project"); err != nil {
		panic(err)
	}
	return cmd
}

func newDeleteRepoCmd(cfg *cliauth.Config, client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <cluster>/et/<org>/<repo> | <ulid>",
		Short: "Delete a repo",
		Long: "Deletes a repo end-to-end: data-plane teardown via the entire-core " +
			"bridge, then the core row. The positional is either " +
			"<cluster>/et/<org>/<repo> (path form, resolved via the data plane) " +
			"or a repo ULID. Default mode requires the repo to be registered " +
			"in core.\n\n" +
			"--force is a platform-admin escape hatch for cleaning up entiredb-only " +
			"repos that have no core registration. It skips the existence precheck, " +
			"so you must supply --cluster explicitly — there's no regional row to " +
			"read it from, and only a ULID can be passed because path resolution " +
			"needs a working data-plane mapping. Easy to typo a ULID and nuke the " +
			"wrong repo; double-check the ID before running.",
		Example: "  # By path (typical)\n" +
			"  entire-repo delete aws-us-east-2.entire.io/et/alice/widgets\n\n" +
			"  # By ULID\n" +
			"  entire-repo delete 01J0000000000000000000000A\n\n" +
			"  # Force-delete an orphan that exists only in entiredb storage\n" +
			"  entire-repo delete 01J0000000000000000000000A --force --cluster royalcanin",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, err := cmd.Flags().GetBool("force")
			if err != nil {
				return fmt.Errorf("read --force: %w", err)
			}
			cluster := clicobra.MustGetString(cmd, "cluster")
			c, err := client()
			if err != nil {
				return err
			}
			if force {
				if cluster == "" {
					return errors.New("--force requires --cluster")
				}
				if strings.Contains(args[0], "/") {
					return errors.New("--force requires a repo ULID (path resolution needs the core mapping that --force is skipping)")
				}
				path := "/api/admin/repos/" + url.PathEscape(args[0]) +
					"?force=true&cluster=" + url.QueryEscape(cluster)
				if _, err := c.Delete(path); err != nil {
					return fmt.Errorf("force delete repo: %w", err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Force-deleted repo %s on cluster %s\n", args[0], cluster)
				return nil
			}
			if cluster != "" {
				return errors.New("--cluster is only valid with --force")
			}
			repoID, err := clidial.ResolveRepoID(*cfg, args[0])
			if err != nil {
				return err
			}
			if _, err := c.Delete("/api/v1/repos/" + url.PathEscape(repoID)); err != nil {
				return fmt.Errorf("delete repo: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Deleted repo %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "Platform-admin only: skip the core-registration precheck and route the data-plane delete to --cluster. For cleaning up entiredb-only orphans.")
	cmd.Flags().String("cluster", "", "Cluster slug to route the data-plane delete to (required with --force).")
	return cmd
}
