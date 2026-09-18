package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// newRepoCmd is the `entire repo` command group: control-plane
// repository lifecycle (create, list within a project, view, edit, delete),
// the `mirror`, `remote`, `visibility`, `protection` and `grant`
// subtrees, plus the `clone` convenience that resolves a mirror and shells
// out to `git clone`. Other git content operations (log, diff, …) remain
// intentionally out of scope here.
func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdRepo,
		Short: "Manage Entire repositories",
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newRepoCreateCmd())
	cmd.AddCommand(newRepoListCmd())
	cmd.AddCommand(newRepoViewCmd())
	cmd.AddCommand(newRepoEditCmd())
	cmd.AddCommand(newRepoDeleteCmd())
	cmd.AddCommand(newRepoCloneCmd())
	cmd.AddCommand(newRepoMirrorCmd())
	cmd.AddCommand(newRepoRemoteCmd())
	cmd.AddCommand(newRepoVisibilityCmd())
	cmd.AddCommand(newRepoProtectionCmd())
	cmd.AddCommand(newRepoGrantCmd())
	return requireSubcommand(cmd)
}

// repoColumns is the human table/field view of a repo, shared by list and
// view. CLUSTER/STATE come from optional fields, shown as "-" when unset.
var repoColumns = []string{"ID", colHeaderName, "PROJECT", colHeaderCluster, "STATE"}

func repoRow(r coreapi.Repo) []string {
	return []string{r.ID, r.Name, r.OwningProjectId, r.ClusterHost.Or("-"), r.State.Or("-")}
}

// repoDetailColumns / repoDetailRow extend the shared repo view with the
// provisioning reason and entire:// clone URL for the single-repo `view` output.
// The list view stays on the lean repoColumns — a full clone URL per row would
// bloat the table — but a person inspecting one repo wants the URL they can
// paste into `git clone` (COR-699). REMOTE is "-" until the repo is provisioned
// enough to have a resolvable cluster host + path.
var repoDetailColumns = []string{"ID", "NAME", "PROJECT", "CLUSTER", "STATE", "PROVISION REASON", "REMOTE"}

func repoDetailRow(r coreapi.Repo) []string {
	remote := repoRemoteURL(r)
	if remote == "" {
		remote = "-"
	}
	return append(repoRow(r), r.ProvisionReason.Or("-"), remote)
}

// repoRemoteURL synthesizes the entire:// clone/remote URL for a repo from
// its resolved cluster host and path — the form `git clone` and
// `git remote add` accept, which git-remote-entire reads back as the repo
// slug from the URL path. Returns "" when either coordinate is missing (a
// still-provisioning repo may not have them yet) or when the host is not a
// bare host[:port] (validateClusterHost): the URL is pasted straight into
// `git clone`, which reads `real-host@evil.com` as userinfo and sends the repo
// token to evil.com, so a spoofable URL is worse than none. `repo clone`
// refuses the same host at its end; this keeps the printed and --json copies
// from handing out what clone would refuse.
func repoRemoteURL(r coreapi.Repo) string {
	host := strings.TrimSpace(r.ClusterHost.Or(""))
	path := strings.TrimSpace(r.Path.Or(""))
	if path == "" || validateClusterHost(host) != nil {
		return ""
	}
	return "entire://" + host + "/" + strings.TrimPrefix(path, "/")
}

// repoCreateOutput renders a created repo as JSON with a synthesized `remote`
// field merged in — the entire:// URL callers paste into `git clone` or
// `git remote add` (see mergeSynthesizedField for the merge rules). The field
// is omitted when the clone coordinates aren't resolvable yet rather than
// emitted half-formed.
func repoCreateOutput(r *coreapi.Repo) (any, error) {
	if r == nil {
		return nil, errors.New("nil repo")
	}
	return mergeSynthesizedField(r, "remote", func() string { return repoRemoteURL(*r) })
}

// parseObjectFormat maps the CLI flag value to the wire enum, rejecting
// anything other than the two accepted values so a typo fails fast
// client-side rather than as an opaque 422 from the server.
func parseObjectFormat(s string) (coreapi.CreateRepoInputBodyObjectFormat, error) {
	switch s {
	case "sha1":
		return coreapi.CreateRepoInputBodyObjectFormatSHA1, nil
	case "sha256":
		return coreapi.CreateRepoInputBodyObjectFormatSHA256, nil
	default:
		return "", fmt.Errorf("invalid object format %q: must be \"sha1\" or \"sha256\"", s)
	}
}

func newRepoCreateCmd() *cobra.Command {
	var (
		projectID    string
		clusterHost  string
		objectFormat string
		noWait       bool
		waitTimeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   cmdCreateName,
		Short: "Create a repository in a project",
		Long: `Create a repository and wait for provisioning to become active by
default. Active means provisioning completed; later pushes or mirror
creation can still fail for other reasons.

--wait-timeout must be positive. It bounds project resolution, creation,
and readiness polling after client setup, including creation with
--no-wait. Use --no-wait to return without confirming readiness.

If creation succeeds but readiness cannot be confirmed, the command exits
nonzero and preserves the repository result. Do not create again to
recover. With --json, stdout contains one repository object; progress
and recovery instructions go to stderr.`,
		Example: "  entire repo create web --project acme\n" +
			"  entire repo create web --project acme --no-wait\n" +
			"  entire repo create web --project acme --wait-timeout=5m",
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Invalid flag values are usage errors, including zero/negative
			// durations; match mirror add and Cobra's malformed-value path.
			if waitTimeout <= 0 {
				return errors.New("--wait-timeout must be positive")
			}
			return nil
		},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Refuse a name Entire could not address once it existed: every ref
			// parser drops a trailing `.git` (see gitDirSuffix), so the repo
			// would be reachable only by ULID. The server would accept it —
			// an interior dot is legal — which is exactly why the check is here.
			if name := strings.TrimSpace(args[0]); strings.HasSuffix(name, gitDirSuffix) {
				cmd.SilenceUsage = true
				err := fmt.Errorf("repo name %q must not end in %s: Entire treats that suffix as never part of a name, so the repo could not be addressed by name afterwards", name, gitDirSuffix)
				if trimmed := strings.TrimSuffix(name, gitDirSuffix); trimmed != "" {
					err = fmt.Errorf("%w (use %q)", err, trimmed)
				}
				return err
			}
			if clusterHost != "" {
				if err := validateClusterHost(clusterHost); err != nil {
					cmd.SilenceUsage = true
					return fmt.Errorf("invalid --cluster-host: %w", err)
				}
			}
			var format coreapi.CreateRepoInputBodyObjectFormat
			if objectFormat != "" {
				parsed, err := parseObjectFormat(objectFormat)
				if err != nil {
					cmd.SilenceUsage = true
					return err
				}
				format = parsed
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				ctx, cancel := context.WithTimeout(ctx, waitTimeout)
				defer cancel()
				projID, err := resolveProjectRef(ctx, c, projectID)
				if err != nil {
					return err
				}
				body := &coreapi.CreateRepoInputBody{Name: args[0], ProjectId: projID}
				if clusterHost != "" {
					body.ClusterHost = coreapi.NewOptString(clusterHost)
				}
				if format != "" {
					body.ObjectFormat = coreapi.NewOptCreateRepoInputBodyObjectFormat(format)
				}
				created, err := c.CreateRepo(ctx, body)
				if err != nil {
					return err
				}
				var waitErr error
				if !noWait {
					var finish func(bool)
					waitErr = awaitRepoActive(ctx, c, created, func() {
						finish = startSpinner(cmd.ErrOrStderr(), "Waiting for repository "+created.Name+" to become active")
					})
					if finish != nil {
						finish(waitErr == nil)
					}
				}
				return reportRepoCreation(cmd, created, noWait, waitErr)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return after creation without confirming provisioning readiness")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute, "Time limit for project resolution, creation, and provisioning readiness")
	cmd.Flags().StringVar(&projectID, "project", "", "Owning project (name or ULID) (required)")
	cmd.Flags().StringVar(&clusterHost, "cluster-host", "", "Public host of the cluster to pin the repo to (defaults to the jurisdiction default)")
	cmd.Flags().StringVar(&objectFormat, "object-format", "", "Git object format for the repository: sha1 or sha256 (defaults to the server default)")
	markRequired(cmd, "project")
	addJSONFlag(cmd)
	return cmd
}

func newRepoListCmd() *cobra.Command {
	var limit, pageSize int
	var all, noPager bool
	var pageToken, project string
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List repositories in a project",
		Long: "List repositories in the project named by --project (name or ULID).\n\n" +
			"By default at most " + strconv.Itoa(coreListFetchBudget) + " repositories are fetched; when the project " +
			"has more, a note on stderr says so — pass --all to fetch everything, or " +
			"--limit N for exactly the first N (rows come in server order; this list " +
			"has no local filters or sort).\n\n" +
			"For manual paging, --page-size/--page-token fetch exactly one page and " +
			"report the cursor to resume from (--json wraps rows in an {items, " +
			"nextPageToken} envelope).",
		Args: cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return fmt.Errorf("--limit must be zero or positive, got %d", limit)
			}
			return validatePageSize(cmd, pageSize)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Decide color against the real output writer before
			// flushThroughPager swaps stdout for a buffer that never looks
			// like a TTY; the buffered render passes the pre-styled cells
			// through unchanged (see preStyleTable).
			headers, row := preStyleTable(cmd.OutOrStdout(), repoColumns, repoRow)
			if pageModeRequested(cmd) {
				return flushThroughPager(cmd, noPager, func() error {
					return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
						projID, err := resolveProjectRef(ctx, c, project)
						if err != nil {
							return err
						}
						params := coreapi.ListProjectReposParams{ProjectId: projID}
						if pageToken != "" {
							params.PageToken = coreapi.NewOptString(pageToken)
						}
						if pageSize > 0 {
							params.PageSize = coreapi.NewOptInt32(int32(pageSize)) //nolint:gosec // G115: validatePageSize bounds it
						}
						out, err := c.ListProjectRepos(ctx, params)
						if err != nil {
							return err
						}
						return renderCoreListPage(cmd, "No repositories found in this project.", headers, row, out.Repos, out.NextPageToken.Or(""))
					})
				})
			}
			return flushThroughPager(cmd, noPager, func() error {
				return runCoreList(cmd, "No repositories found in this project.", headers, row, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Repo, error) {
					projID, err := resolveProjectRef(ctx, c, project)
					if err != nil {
						return nil, err
					}
					// Rows render in server order with no local filters or
					// sort, so --limit bounds the fetch directly; without it
					// the default budget bounds the walk instead.
					budget := coreListFetchBudget
					switch {
					case all:
						budget = 0 // unbounded
					case limit > 0:
						budget = limit
					}
					repos, partial, err := fetchPagesBounded(ctx, budget, func(ctx context.Context, cursor string) ([]coreapi.Repo, string, error) {
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
					if err != nil {
						return nil, err
					}
					// An explicit --limit is not a surprise, so only the
					// default budget's stop is disclosed. Printed for --json
					// too: a script acting on silently partial data is the
					// worst outcome, and stderr never corrupts the stdout JSON.
					if partial && limit == 0 {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"Note: the project has more repositories; showing the first %d fetched — pass --all to fetch everything.\n",
							len(repos))
					}
					if limit > 0 && len(repos) > limit {
						repos = repos[:limit] // trim a page overshoot
					}
					return repos, nil
				})
			})
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project to list (name or ULID) (required)")
	markRequired(cmd, "project")
	cmd.Flags().IntVar(&limit, "limit", 0, "Fetch and show only the first N repositories (0 uses the default fetch budget)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch every repository instead of the first "+strconv.Itoa(coreListFetchBudget)+" (slower on large projects)")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "Print directly to stdout instead of a pager for long output")
	pageModeFlags(cmd, &pageSize, &pageToken)
	addJSONFlag(cmd)
	setFlagGroup(cmd, flagGroupScope, "project")
	setFlagGroup(cmd, flagGroupNavigation, "all", "limit", "page-size", "page-token")
	setFlagGroup(cmd, flagGroupFormatting, "json", "no-pager")
	useGroupedFlagHelp(cmd,
		flagGroup{name: flagGroupScope},
		flagGroup{name: flagGroupNavigation},
		flagGroup{name: flagGroupFormatting},
	)
	return cmd
}

func newRepoViewCmd() *cobra.Command {
	var project string
	var authoritative bool
	cmd := &cobra.Command{
		Use:   "view <repo>",
		Short: "Show a repository by /et/<project>/<repo> path, name, or ULID",
		Long: `Show repository details. Use --authoritative to also check provisioning
status. This command does not wait: it exits successfully when it can
read the repository, even if provisioning is still in progress or has failed.
Use --authoritative --json and inspect state for scripting; active means
provisioning has completed.

The default read cannot confirm readiness. If the server cannot provide
readiness information, --authoritative reports an error or a missing state;
neither confirms readiness.`,
		Example: "  entire repo view /et/acme/web\n" +
			"  entire repo view /et/acme/web --json\n" +
			"  entire repo view /et/acme/web --authoritative",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, repoDetailColumns, repoDetailRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Repo, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				params := coreapi.GetRepoParams{RepoId: repoID}
				if authoritative {
					params.Authoritative = coreapi.NewOptBool(true)
				}
				repo, err := c.GetRepo(ctx, params)
				if authoritative && readinessCheckUnavailable(err) {
					// A registry-only fallback cannot answer the readiness question.
					// Keep that choice explicit, and print here so renderCoreError
					// cannot strip the recovery hint with the API error wrapper.
					// The plain read is the default, so the hint names no flag: a
					// value the user would have to restate is not a recovery step.
					fmt.Fprintf(cmd.ErrOrStderr(), "%v\nUse entire repo view %s to inspect repository details without a readiness check.\n", renderRepoReadError(err), repoID)
					return nil, NewSilentError(err)
				}
				return repo, err
			})
		},
	}
	cmd.Flags().BoolVar(&authoritative, "authoritative", false, "Check repository provisioning status")
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}

func newRepoDeleteCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "delete <repo>",
		Short: "Delete a repository by /et/<project>/<repo> path, name, or ULID",
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

// repoVisibility is the field/JSON view shared by `visibility get` and
// `edit --visibility`. Repo is the reference the user passed (name or ULID); Visibility is
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

// newRepoVisibilityCmd holds the visibility read verb; the write is
// `repo edit --visibility`. "public" sets the SpiceDB public_viewer wildcard, which grants pull (read)
// to any authenticated account but never push or manage; "private" restricts
// the repo to explicit grantees. The data plane still requires authentication,
// so "public" means read-only-to-any-user, not anonymous/unauthenticated.
func newRepoVisibilityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "visibility",
		Short: "Show a repository's visibility",
	}
	cmd.AddCommand(newRepoVisibilityGetCmd())
	return requireSubcommand(cmd)
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
	addJSONFlag(cmd)
	return cmd
}

// newRepoEditCmd changes a repository's settings. Visibility is the only one
// today, so --visibility is required; further settings become further flags
// on this command.
func newRepoEditCmd() *cobra.Command {
	var project, visibility string
	cmd := &cobra.Command{
		Use:   "edit <repo>",
		Short: "Edit a repository's settings",
		Long: "Edit a repository's settings.\n\n" +
			"--visibility public grants read-only (pull) access to any authenticated Entire user; " +
			"push and management stay restricted to grantees. --visibility private restricts the repo " +
			"to explicit grantees. Requires manage permission on the repo.",
		Example: "  entire repo edit /et/my-project/my-repo --visibility private",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vis, err := parseVisibility(visibility)
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
	cmd.Flags().StringVar(&visibility, "visibility", "", "Visibility to set: public or private (required)")
	markRequired(cmd, "visibility")
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}

// bindRepoProjectFlag wires the shared --project scope used to resolve a repo
// addressed by a BARE NAME. That is the only form it serves: a repo name is
// unique only within its project, and the control plane has no by-name route
// that is not project-scoped (ListProjectRepos takes the project as a path
// segment), so without the flag a bare name is not an address at all.
//
// The other two spellings carry their own project, so the flag cannot change
// what they resolve to — and the two used to disagree about saying so. A
// /et/<project>/<repo> path is checked for agreement by resolveRepoPathRef,
// which costs nothing because it resolves that project anyway. A ULID
// short-circuits resolveRepoRef before projectRef is ever read, so a flatly
// wrong project was accepted in silence; warnRedundantProjectFlag is what ends
// that.
func bindRepoProjectFlag(cmd *cobra.Command, project *string) {
	cmd.Flags().StringVar(project, "project", "", "Owning project (name or ULID); required when <repo> is a bare name, redundant with a /"+nativeCloneForge+"/<project>/<repo> path or a ULID")
	warnRedundantProjectFlag(cmd, project)
}

// warnRedundantProjectFlag reports on stderr that --project cannot affect the
// lookup, when the ref identifies the repo on its own.
//
// It WARNS rather than refusing because addressing a repo by ULID with an
// unconditional --project has been accepted since the flag shipped (ece9fb3dc,
// June 2026) and is plausibly scripted; breaking that to report a flag that was
// already being ignored is a poor trade. It does not VALIDATE because that
// needs GetRepo's owningProjectId, an extra round trip on every command here
// except `repo view` — which alone already fetches the repo and prints its
// project.
//
// Wired as a PreRunE because the answer needs only the flag and args[0]: no
// resolution, no network, and every command binding this flag takes the repo
// ref as args[0]. An existing PreRunE is chained rather than clobbered, so a
// command that grows one later does not silently lose the warning (or its own
// hook).
func warnRedundantProjectFlag(cmd *cobra.Command, project *string) {
	prev := cmd.PreRunE
	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if prev != nil {
			if err := prev(c, args); err != nil {
				return err
			}
		}
		// Changed(), not a non-empty value: an explicit --project "" is still
		// the user saying something, and reporting it is the point.
		if len(args) > 0 && c.Flags().Changed("project") && looksLikeULID(args[0]) {
			fmt.Fprintf(c.ErrOrStderr(), "Note: --project %q is ignored — %s is a repo ULID, which identifies the repo on its own.\n", *project, args[0])
		}
		return nil
	}
}
