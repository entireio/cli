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
// repository lifecycle (create, list within a project, get, delete), the
// `mirror` and `visibility` subtrees, plus the `clone` convenience that
// resolves a mirror and shells out to `git clone`. Other git content
// operations (log, diff, …) remain intentionally out of scope here. Surfaced
// via `entire labs`.
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
	cmd.AddCommand(newRepoCloneCmd())
	cmd.AddCommand(newRepoMirrorCmd())
	cmd.AddCommand(newRepoVisibilityCmd())
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

// repoDetailColumns / repoDetailRow extend the shared repo view with the
// entire:// clone URL for the single-repo `get` output. The list view stays on
// the lean repoColumns — a full clone URL per row would bloat the table — but a
// person inspecting one repo wants the URL they can paste into `git clone`
// (COR-699). REMOTE is "-" until the repo is provisioned enough to have a
// resolvable cluster host + path.
var repoDetailColumns = []string{"ID", "NAME", "PROJECT", "CLUSTER", "STATE", "REMOTE"}

func repoDetailRow(r coreapi.Repo) []string {
	remote := repoRemoteURL(r)
	if remote == "" {
		remote = "-"
	}
	return append(repoRow(r), remote)
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
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				projID, err := resolveProjectRef(ctx, c, projectID)
				if err != nil {
					return "", nil, err
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
					return "", nil, err
				}
				wire, err := repoCreateOutput(created)
				if err != nil {
					return "", nil, err
				}
				msg := fmt.Sprintf("✓ Created repository %s (%s)", created.Name, created.ID)
				if remote := repoRemoteURL(*created); remote != "" {
					msg += "\n  Remote: " + remote
				}
				return msg, wire, nil
			})
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "Owning project (name or ULID) (required)")
	cmd.Flags().StringVar(&clusterHost, "cluster-host", "", "Public host of the cluster to pin the repo to (defaults to the jurisdiction default)")
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
			return runCoreList(cmd, "No repositories found in this project.", repoColumns, repoRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Repo, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Repo, string, error) {
					params := coreapi.ListProjectReposParams{ProjectId: projID}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListProjectRepos(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Repos, out.NextPageToken.Or(""), nil
				})
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
			return runCoreObject(cmd, repoDetailColumns, repoDetailRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Repo, error) {
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
			return runControlPlaneDelete(cmd, "repo", args[0],
				func(ctx context.Context, c *coreapi.Client) (string, error) {
					return resolveRepoRef(ctx, c, args[0], project)
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					return c.DeleteRepo(ctx, coreapi.DeleteRepoParams{RepoId: id})
				})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addForceFlag(cmd)
	return cmd
}

// repoVisibility is the field/JSON view shared by the visibility get/set
// verbs. Repo is the reference the user passed (name or ULID); Visibility is
// the server's authoritative value after the call.
type repoVisibility struct {
	Repo       string `json:"repo"`
	Visibility string `json:"visibility"`
}

var visibilityColumns = []string{"REPO", "VISIBILITY"}

func visibilityRow(v repoVisibility) []string {
	return []string{v.Repo, v.Visibility}
}

// parseVisibility maps the CLI argument to the wire enum, rejecting anything
// other than the two accepted values so a typo fails fast client-side rather
// than as an opaque 422 from the server.
func parseVisibility(s string) (coreapi.SetRepoVisibilityInputBodyVisibility, error) {
	switch s {
	case "public":
		return coreapi.SetRepoVisibilityInputBodyVisibilityPublic, nil
	case "private":
		return coreapi.SetRepoVisibilityInputBodyVisibilityPrivate, nil
	default:
		return "", fmt.Errorf("invalid visibility %q: must be \"public\" or \"private\"", s)
	}
}

// newRepoVisibilityCmd groups the read/write verbs for a repo's visibility.
// "public" sets the SpiceDB public_viewer wildcard, which grants pull (read)
// to any authenticated account but never push or manage; "private" restricts
// the repo to explicit grantees. The data plane still requires authentication,
// so "public" means read-only-to-any-user, not anonymous/unauthenticated.
func newRepoVisibilityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "visibility",
		Short: "Get or set a repository's visibility",
	}
	cmd.AddCommand(newRepoVisibilityGetCmd())
	cmd.AddCommand(newRepoVisibilitySetCmd())
	return cmd
}

func newRepoVisibilityGetCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "get <repo>",
		Short: "Show a repository's visibility (public or private)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, visibilityColumns, visibilityRow, func(ctx context.Context, c *coreapi.Client) (*repoVisibility, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				out, err := c.GetRepoVisibility(ctx, coreapi.GetRepoVisibilityParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return &repoVisibility{Repo: args[0], Visibility: string(out.Visibility)}, nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	return cmd
}

func newRepoVisibilitySetCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "set <repo> <public|private>",
		Short: "Set a repository's visibility",
		Long: "Set a repository's visibility.\n\n" +
			"\"public\" grants read-only (pull) access to any authenticated Entire user; " +
			"push and management stay restricted to grantees. \"private\" restricts the repo " +
			"to explicit grantees. Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vis, err := parseVisibility(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreObject(cmd, visibilityColumns, visibilityRow, func(ctx context.Context, c *coreapi.Client) (*repoVisibility, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				out, err := c.SetRepoVisibility(ctx, &coreapi.SetRepoVisibilityInputBody{Visibility: vis}, coreapi.SetRepoVisibilityParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return &repoVisibility{Repo: args[0], Visibility: string(out.Visibility)}, nil
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
	cmd.Flags().StringVar(project, "project", "", "Owning project (name or ULID); required when <repo> is a name")
}
