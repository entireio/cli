package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
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

// repoColumns is the human table for `repo list`, headed the way `repo mirror
// list` heads the same facts so the two directories read alike.
//
// NAME is the /et/<project>/<repo> path, NOT the bare name: a name is unique
// only inside its project, so the old column printed a string no verb accepts
// on its own — and `repo view` now takes the path and nothing else, which left
// the table with no cell a reader could act on. The ULID and project columns
// went with it; `--json` carries both for anything that needs them.
//
// STATUS speaks the placement vocabulary, as `mirror list`'s does: a repo says
// `active` where a placement says `ready`, and one column should not carry two
// words for one fact (primaryPlacementStatus). It dashes where the listing
// returns no state, which happens per repo rather than per project.
//
// ACCESS is the one column of `mirror list` left out: it is candidate-only,
// describing a GitHub repo not yet onboarded, and nothing in a project listing
// can ever be one — so it would dash on every row, and an always-empty column
// costs every reader something one reader wants.
var repoColumns = []string{
	colName.header, colHeaderCluster, colVisibility.header, colStatus.header,
}

// repoRow renders one row against the project the listing was scoped to. The
// project's NAME comes from resolving the caller's ref once: the repo records
// carry only its ULID, and a path built from a ULID is not a ref.
func repoRow(projectName string, r coreapi.Repo) []string {
	name := r.Name
	if projectName != "" {
		name = nativeRepoPath(projectName + "/" + r.Name)
	}
	return []string{
		name,
		r.ClusterHost.Or("-"),
		visibilityDisplay(visibilityOf(r.Visibility.Or(""))),
		orDash(primaryPlacementStatus(r.State.Or(""))),
	}
}

// repoRowStyled colours the cells `mirror list` colours for the same facts: the
// cluster cyan, the visibility by what it says, and the status by its lifecycle
// (repoDirCellsStyled). One directory should not paint a private or a failed
// repo one way and the other another.
func repoRowStyled(st statusStyles, projectName string) func(coreapi.Repo) []string {
	return func(r coreapi.Repo) []string {
		cells := repoRow(projectName, r)
		if !st.colorEnabled {
			return cells
		}
		if cells[1] != "-" {
			cells[1] = st.render(st.cyan, cells[1])
		}
		cells[2] = st.render(visibilityColor(st, visibilityOf(r.Visibility.Or(""))), cells[2])
		if style, ok := repoStatusColor(st, primaryPlacementStatus(r.State.Or(""))); ok {
			cells[3] = st.render(style, cells[3])
		}
		return cells
	}
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

// suggestRepoName returns the name worth recommending in place of one that
// carried the `.git` suffix, and whether there is one at all. It answers only
// the question "is this advice the user can act on?".
//
// Lowercased, because `repo create` is the one path where the server does NOT
// fold case: resolution folds (which is why nativeRepoRe accepts uppercase),
// but an uppercase name is refused outright at create time. So the answer to
// "WEB.git" is "web". Suggesting "WEB" — the typed string minus four bytes —
// earned the user a second refusal naming a rule the first message had not
// mentioned.
//
// The shape checks are nativeRepoRe's, which already carries the server's
// accepted name shape, plus the two rules a regexp cannot: no consecutive
// dots (RE2 has no negative lookahead, so parseNativeCloneRef checks it
// separately too) and no raw ULID.
//
// This gates only whether the CLI SPEAKS, never whether it refuses, and that
// is the whole reason it is safe to run locally. nativeRepoRe drifts one way
// (see its comment): if the server loosens, a local check refuses names that
// would in fact work. A ref survives that — a ULID or a full entire:// URL
// gets past it — but a refused `create` has no such escape hatch, so the name
// itself stays the server's to judge. Going quiet costs a hint; guessing wrong
// costs a name the user cannot create.
func suggestRepoName(rest string) (string, bool) {
	s := strings.ToLower(rest)
	// A doubled suffix is the one case the shape checks below cannot catch,
	// because there is nothing malformed about what it leaves. The cut runs
	// exactly once (see gitremote.CutGitDirSuffix), so "widgets.git.git" leaves
	// "widgets.git" — an interior dot, which nativeRepoRe rightly allows.
	// Recommending it would send the user straight back into the guard that
	// called this, refused a second time by the rule they had just been told.
	if _, stillCarriesSuffix := gitremote.CutGitDirSuffix(s); stillCarriesSuffix {
		return "", false
	}
	if s == "" || !nativeRepoRe.MatchString(s) || strings.Contains(s, "..") || looksLikeULID(s) {
		return "", false
	}
	return s, true
}

func newRepoCreateCmd() *cobra.Command {
	var (
		projectID    string
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
			// Refuse a name that ends in `.git`, whatever its case. The suffix
			// is never part of a repo name (see gitDirSuffix): every ref
			// parser drops it, so the name would round-trip to a different
			// string than the one typed. The server refuses it too; saying so
			// here costs no round trip and names the spelling to use instead.
			//
			// The case-insensitive cut is what makes that promise hold. A
			// case-sensitive check let ".GIT" through to the server, which
			// rejects it for carrying uppercase — a true statement about a
			// different problem, leaving the user to discover the suffix rule
			// on a second attempt.
			//
			// The trimmed name is what gets checked AND what gets sent
			// (see body below): a guard reading one value while another
			// travels is a disagreement waiting for the server to stop
			// covering for it.
			name := strings.TrimSpace(args[0])
			if rest, had := gitremote.CutGitDirSuffix(name); had {
				cmd.SilenceUsage = true
				err := fmt.Errorf("repo name %q must not end in %s, in any case: the suffix is never part of a repo name, so Entire could not address the repo by the name you typed", name, gitDirSuffix)
				if use, ok := suggestRepoName(rest); ok {
					err = fmt.Errorf("%w (use %q)", err, use)
				}
				return err
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
				body := &coreapi.CreateRepoInputBody{Name: name, ProjectId: projID}
				if format != "" {
					body.ObjectFormat = coreapi.NewOptCreateRepoInputBodyObjectFormat(format)
				}
				response, err := c.CreateRepo(ctx, body)
				if err != nil {
					return err
				}
				created, err := createdRepoAsRepo(&response.Response)
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
	cmd.Flags().StringVar(&projectID, projectFlagName, "", "Owning project (name or ULID) (required)")
	cmd.Flags().StringVar(&objectFormat, "object-format", "", "Git object format for the repository: sha1 or sha256 (defaults to the server default)")
	markRequired(cmd, projectFlagName)
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
			// The project's name is known only once the ref is resolved, which
			// happens inside the core call below — but the row func has to be
			// built out here, where the real writer decides color. The closure
			// reads it at render time, which is always after the resolve.
			// Styled the way `repo mirror list` styles the same table: yellow
			// headers via styledHeaders, cells via this view's own styler. The
			// two directories show the same facts and should not look unalike.
			var projectName string
			st := newStatusStyles(cmd.OutOrStdout())
			headers := styledHeaders(st, repoColumns)
			row := func(r coreapi.Repo) []string { return repoRowStyled(st, projectName)(r) }
			if pageModeRequested(cmd) {
				return flushThroughPager(cmd, noPager, func() error {
					return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
						projID, name, err := resolveProjectRefNamed(ctx, c, project)
						if err != nil {
							return err
						}
						projectName = name
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
					projID, name, err := resolveProjectRefNamed(ctx, c, project)
					if err != nil {
						return nil, err
					}
					projectName = name
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
	cmd.Flags().StringVar(&project, projectFlagName, "", "Project to list (name or ULID) (required)")
	markRequired(cmd, projectFlagName)
	cmd.Flags().IntVar(&limit, "limit", 0, "Fetch and show only the first N repositories (0 uses the default fetch budget)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch every repository instead of the first "+strconv.Itoa(coreListFetchBudget)+" (slower on large projects)")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "Print directly to stdout instead of a pager for long output")
	pageModeFlags(cmd, &pageSize, &pageToken)
	addJSONFlag(cmd)
	setFlagGroup(cmd, flagGroupScope, projectFlagName)
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
	var authoritative bool
	cmd := &cobra.Command{
		Use:   "view <repo>",
		Short: "Show a repository and every cluster holding a copy of it",
		Long: "Show a repository: its identity, visibility, and one row per cluster " +
			"it is placed on, with that cluster's clone URL and status.\n\n" +
			"<repo> names its forge, and nothing else addresses a repository here:\n\n" +
			"  - /et/<project>/<repo> — an Entire-native repo, shown with its\n" +
			"    primary cluster and each mirror of it, plus how far a seed in\n" +
			"    progress has got\n" +
			"  - /gh/<owner>/<repo> — a GitHub upstream, shown with its mirror on\n" +
			"    every cluster\n" +
			"  - an entire:// clone URL, as `git clone` takes it. A trailing .git\n" +
			"    is decoration on either forge — pasting one from `git remote -v`\n" +
			"    resolves the same repository as the bare URL\n\n" +
			"A clone URL is looked up on the login server fronting its cluster, so it " +
			"resolves even when that cluster belongs to a federation other than the " +
			"active auth context; every other form is looked up on the active " +
			"context's login server.\n\n" +
			"A native repo's state is read authoritatively, so the primary's STATUS " +
			"says whether it is usable. Pass --authoritative to fail rather than " +
			"dash that cell when the server cannot answer.",
		Example: "  entire repo view /et/acme/web\n" +
			"  entire repo view /gh/octocat/hello-world\n" +
			"  entire repo view /et/acme/web --json\n" +
			"  entire repo view entire://aws-us-east-2.entire.io/et/acme/web",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			ref := strings.TrimSpace(args[0])
			// A clone URL names its own cluster, so it is looked up on the core
			// fronting that cluster. Recognised by its scheme so a malformed one
			// keeps the clone-URL parser's reason instead of being reported as a
			// bad repository reference.
			if strings.HasPrefix(strings.ToLower(ref), entireCloneURLScheme) {
				clusterHost, target, err := parseEntireCloneURL(ref)
				if err != nil {
					return badRepoRefErr(err)
				}
				if target.forge == nativeCloneForge {
					return runNativeRepoView(cmd, target.qualified(), clusterHost, authoritative)
				}
				warnFlagsGitHubViewIgnores(cmd, authoritative)
				return runRepoMirrorViewByName(cmd, target.owner+"/"+target.repo, clusterHost)
			}
			// A repository is named /<forge>/<a>/<b> and no other way, so a
			// bare `acme/web` is REFUSED — with both spellings suggested, since
			// this verb serves both forges and naming one would send the reader
			// to a ref the other half of the time.
			//
			// A /gh/ ref is a GitHub upstream: Entire holds no repo record for
			// it, only the mirrors of it, so it takes the directory lookup
			// rather than the repo resolver the native path uses.
			target, err := parseMirrorRepoRef(ref)
			if err != nil {
				return err
			}
			if target.forge == mirrorCloneForge {
				warnFlagsGitHubViewIgnores(cmd, authoritative)
				return runRepoMirrorViewByName(cmd, target.owner+"/"+target.repo, "")
			}
			return runNativeRepoView(cmd, target.qualified(), "", authoritative)
		},
	}
	cmd.Flags().BoolVar(&authoritative, "authoritative", false, "Fail if the server cannot confirm provisioning state")
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
				func(ctx context.Context, c *coreapi.Client) (resolvedRef, error) {
					return resolveRepoRefResolved(ctx, c, args[0], project)
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					_, err := c.DeleteRepo(ctx, coreapi.DeleteRepoParams{RepoId: id})
					return err
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

var visibilityColumns = []string{colHeaderRepo, "VISIBILITY"}

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

// projectFlagName is the --project flag's name, in the one place the repo
// commands register, require, group and read it. The same word is also a NOUN
// in `entire project` and in the grant family's messages; those are a different
// thing that happens to be spelled alike, so they keep their own literals.
const projectFlagName = "project"

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
	cmd.Flags().StringVar(project, projectFlagName, "", "Owning project (name or ULID); required when <repo> is a bare name, redundant with a /"+nativeCloneForge+"/<project>/<repo> path or a ULID")
	warnRedundantProjectFlag(cmd, project)
}

// warnRedundantProjectFlag reports on stderr that --project cannot affect the
// lookup, when the ref identifies the repo on its own.
//
// It WARNS rather than refusing because addressing a repo by ULID with an
// unconditional --project has been accepted since the flag shipped (ece9fb3dc,
// June 2026) and is plausibly scripted; breaking that to report a flag that was
// already being ignored is a poor trade. It does not VALIDATE because that
// needs GetRepo's owningProjectId, an extra round trip on every command that
// binds this flag. `repo view` used to be the exemption, since it fetches the
// repo anyway — it no longer binds the flag at all, taking the
// /et/<project>/<repo> path that names its own project instead.
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
		if len(args) > 0 && c.Flags().Changed(projectFlagName) && looksLikeULID(args[0]) {
			fmt.Fprintf(c.ErrOrStderr(), "Note: --project %q is ignored — %s is a repo ULID, which identifies the repo on its own.\n", *project, args[0])
		}
		return nil
	}
}

// warnFlagsGitHubViewIgnores reports --authoritative reaching the GitHub half of
// `repo view`, where it means nothing: Entire holds no repo record for an
// upstream, so there is no provisioning state to confirm and the directory
// lookup takes no such value.
//
// It matters because the flag's help promises to FAIL if the server cannot
// confirm that state, so a script gating on the guarantee would otherwise get
// exit 0 with no check performed. Warning rather than erroring keeps a
// `for repo in ...` loop over mixed forges working.
//
// --project is not checked: `repo view` does not register it, so Changed()
// could only ever answer false.
//
// The VALUE is what decides, not Changed(): --authoritative=false asks for
// exactly what the GitHub path does, so reporting it as ignored tells the
// caller a flag they turned off was disregarded. warnRedundantProjectFlag tests
// Changed() alone on purpose — an explicit --project "" still states a scope —
// but false is this flag's default and states nothing.
func warnFlagsGitHubViewIgnores(cmd *cobra.Command, authoritative bool) {
	if authoritative {
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: --authoritative is ignored for a GitHub repository; Entire holds no repository record for an upstream, only the mirrors of it.")
	}
}
