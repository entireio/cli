package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
		objectFormat string
		visibility   string
		noWait       bool
		waitTimeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create [<repo>]",
		Short: "Create a repository in a project",
		Long: `Create a repository and wait for provisioning to become active by
default. Active means provisioning completed; later pushes or mirror
creation can still fail for other reasons.

<repo> is the new repository's /et/<project>/<repo> path, which creates
it directly in that project.

In an interactive terminal, 'entire repo create' or 'entire repo create
<name>' opens a wizard instead. It asks for the project, name,
visibility and advanced options, starting from the given bare name (or
the current folder's), and shows a summary before creating anything. A
bare name never creates on its own: it only starts the wizard. With
--json the prompts stay off stdout, which carries only the repository
object.

--visibility and --object-format are the path form: give either and the
/et/<project>/<repo> path is required.

--visibility is set once the repository is ready, or straight after
creation with --no-wait: public grants read-only (pull) access to any
authenticated Entire user, private restricts it to explicit grantees.
New repositories are private, so omitting it (or the wizard's default)
leaves the repository private. If setting it fails, the command exits
nonzero, keeps the repository and prints the 'entire repo edit' that
finishes the job.

--wait-timeout must be positive. It bounds project resolution, creation,
and readiness polling after client setup, including creation with
--no-wait. The wizard's loading and create share that one budget; time
spent answering the wizard does not count. Use --no-wait
to return without confirming readiness.

If creation succeeds but readiness cannot be confirmed, the command exits
nonzero and preserves the repository result. Do not create again to
recover. With --json, stdout contains one repository object; progress
and recovery instructions go to stderr.`,
		Example: "  entire repo create /et/acme/web\n" +
			"  entire repo create /et/acme/web --visibility public\n" +
			"  entire repo create /et/acme/web --no-wait\n" +
			"  entire repo create /et/acme/web --wait-timeout=5m\n\n" +
			"  # Pick the project and settings interactively\n" +
			"  entire repo create web",
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Invalid flag values are usage errors, including zero/negative
			// durations; match mirror add and Cobra's malformed-value path.
			if waitTimeout <= 0 {
				return errors.New("--wait-timeout must be positive")
			}
			return nil
		},
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := repoCreateRequest{}
			var projectName string
			if len(args) == 1 {
				// Trimmed once for both forms, as the wizard trims what is
				// typed, so ' web ' names the same repo either way and a blank
				// name counts as missing. The trimmed name is what gets checked
				// AND what gets sent.
				project, name, err := parseRepoCreateRef(args[0])
				if err != nil {
					cmd.SilenceUsage = true
					return err
				}
				projectName, req.name = project, name
			}
			if objectFormat != "" {
				parsed, err := parseObjectFormat(objectFormat)
				if err != nil {
					cmd.SilenceUsage = true
					return err
				}
				req.objectFormat = parsed
			}
			if visibility != "" {
				parsed, err := parseVisibility(visibility)
				if err != nil {
					cmd.SilenceUsage = true
					return err
				}
				req.visibility = parsed
			}
			opts := repoCreateOptions{noWait: noWait, waitTimeout: waitTimeout}
			if projectName == "" {
				// Settled from the command line alone, before any request: a
				// run that cannot be prompted must not cost a lookup. Flags
				// mean the path form, so a create flag without a path is
				// refused even in a terminal: the wizard takes only a bare
				// name to start from. --json still prompts, as `grant add`
				// does: the form renders on stderr or the controlling terminal
				// and stdout carries only the result.
				// No terminal is checked first: the flag-form refusal suggests
				// running without flags to be prompted, which nothing can do
				// without one.
				cmd.SilenceUsage = true
				if !interactive.CanPromptInteractively() {
					return repoCreateMissingInput(req.name, errRepoCreateNeedsInput)
				}
				if repoCreateFlagsGiven(cmd) {
					return repoCreateMissingInput(req.name, errRepoCreateFlagsNeedInput)
				}
				return runRepoCreateWizard(cmd, req.name, opts)
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				ctx, cancel := context.WithTimeout(ctx, waitTimeout)
				defer cancel()
				// A path segment is a name by construction, so a project NAMED
				// like a ULID is looked up as the name it is.
				project, err := resolveProjectNamed(ctx, c, projectName)
				if err != nil {
					return err
				}
				req.projectID, req.projectName = project.ID, project.Name
				created, err := createRepo(ctx, c, req)
				if err != nil {
					return err
				}
				return finishRepoCreate(ctx, cmd, c, req, created, opts)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return after creation without confirming provisioning readiness")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute, "Time limit for project resolution, creation, and provisioning readiness")
	cmd.Flags().StringVar(&objectFormat, repoCreateFlagObjectFormat, "", "Git object format for the repository: sha1 or sha256 (defaults to the server default)")
	cmd.Flags().StringVar(&visibility, repoCreateFlagVisibility, "", "Visibility to set after creation: public or private (new repositories are private)")
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
		Long: "List repositories in the project named by --project " + nativeCloneForge + "/<project>, " +
			"the project spelling `entire trail` takes. GitHub mirrors are listed by `entire repo mirror list`.\n\n" +
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
			// Parsed here rather than in PreRunE, which cobra runs before it
			// checks required flags: a missing --project must say so, not
			// read as a malformed empty one.
			projectName, err := parseRepoListProject(project)
			if err != nil {
				return err
			}
			// The row func is built out here, where the real writer decides
			// color, but the project's name it prints is the server's, known
			// only once the resolve inside the core call below has run. The
			// closure reads it at render time, which is always after that.
			// Styled the way `repo mirror list` styles the same table: yellow
			// headers via styledHeaders, cells via this view's own styler. The
			// two directories show the same facts and should not look unalike.
			st := newStatusStyles(cmd.OutOrStdout())
			headers := styledHeaders(st, repoColumns)
			row := func(r coreapi.Repo) []string { return repoRowStyled(st, projectName)(r) }
			if pageModeRequested(cmd) {
				return flushThroughPager(cmd, noPager, func() error {
					return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
						resolved, err := resolveProjectNamed(ctx, c, projectName)
						if err != nil {
							return err
						}
						projectName = resolved.Name
						params := coreapi.ListProjectReposParams{ProjectId: resolved.ID}
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
					resolved, err := resolveProjectNamed(ctx, c, projectName)
					if err != nil {
						return nil, err
					}
					projectName = resolved.Name
					projID := resolved.ID
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
	cmd.Flags().StringVar(&project, projectFlagName, "", "Project to list, as "+nativeCloneForge+"/<project> (required)")
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

// parseRepoListProject reads `repo list --project`, spelled et/<project> as
// `entire trail --project` spells an Entire project: the project declares its
// namespace, so a name is never mistaken for a ULID or for a GitHub owner. It
// is not trail's parser. A leading slash is accepted, as every repo path takes
// one; a gh/ owner is answered with the command that lists GitHub mirrors; and
// the name must have the server's project shape (nativeProjectRe, as in a repo
// path), all before any request. The name is lowercased: the server matches it
// case-insensitively.
func parseRepoListProject(ref string) (string, error) {
	trimmed := trimRefPrefix(ref)
	if forge, _, ok := strings.Cut(trimmed, "/"); ok && forge == mirrorCloneForge {
		return "", fmt.Errorf("--project %q is a GitHub owner, not an Entire project: list GitHub mirrors with `entire repo mirror list`", ref)
	}
	name, ok := strings.CutPrefix(trimmed, nativeCloneForge+"/")
	if !ok || !nativeProjectRe.MatchString(name) {
		return "", fmt.Errorf("invalid project %q: use %s/<project>, where <project> is 3-32 letters, digits and '-', not starting or ending with '-'", ref, nativeCloneForge)
	}
	return strings.ToLower(name), nil
}

// repoEchoName is what `repo edit` and `visibility get` report as `repo`: the
// path the server resolved, or the ref as typed when the resolution echoed
// none. Unlike resolvedRefLabel it carries no ULID, so the value stays one a
// script can pass back to another repo command.
func repoEchoName(ref string, r resolvedRef) string {
	return cmp.Or(r.Name, strings.TrimSpace(ref))
}

func newRepoViewCmd() *cobra.Command {
	var authoritative bool
	cmd := &cobra.Command{
		Use:   "view [repo]",
		Short: "Show a repository and every cluster holding a copy of it",
		Long: "Show a repository: its identity, visibility, and one row per cluster " +
			"it is placed on, with that cluster's clone URL and status.\n\n" +
			"With no repository argument, resolve the current clone's origin remote.\n" +
			"--json includes repoId and apiUrl for the responsible API cell when resolvable.\n\n" +
			"An explicit <repo> names its forge:\n\n" +
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
		Example: "  entire repo view --json\n" +
			"  entire repo view /et/acme/web\n" +
			"  entire repo view /gh/octocat/hello-world\n" +
			"  entire repo view /et/acme/web --json\n" +
			"  entire repo view entire://aws-us-east-2.entire.io/et/acme/web",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			var ref string
			if len(args) == 0 {
				var err error
				ref, err = currentRepoViewRef(cmd.Context())
				if err != nil {
					return err
				}
			} else {
				ref = strings.TrimSpace(args[0])
			}
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

// Keep Entire clone URLs intact so the normal view path selects the login
// server fronting that cluster, including origins in another federation.
func currentRepoViewRef(ctx context.Context) (string, error) {
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		if errors.Is(err, paths.ErrNotARepository) {
			return "", fmt.Errorf("not in a git repository; pass /gh/<owner>/<repo> or /et/<project>/<repo> explicitly: %w", err)
		}
		return "", fmt.Errorf("resolve current git repository: %w", err)
	}
	remoteURL, err := gitremote.GetRemoteURL(ctx, "origin")
	if err != nil {
		return "", fmt.Errorf("%w; pass /gh/<owner>/<repo> or /et/<project>/<repo> explicitly", err)
	}
	if strings.HasPrefix(strings.ToLower(remoteURL), entireCloneURLScheme) {
		return remoteURL, nil
	}
	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", fmt.Errorf("resolve origin remote: %w", err)
	}
	if forge != mirrorCloneForge {
		return "", errors.New("origin must name a GitHub or Entire repository; pass a /gh/ or /et/ repository reference explicitly")
	}
	return "/gh/" + owner + "/" + repo, nil
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
	cmd := &cobra.Command{
		Use:   "get <repo>",
		Short: "Show a repository's visibility (public or private)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, visibilityColumns, visibilityRow, func(ctx context.Context, c *coreapi.Client) (*repoVisibility, error) {
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				out, err := c.GetRepoVisibility(ctx, coreapi.GetRepoVisibilityParams{RepoId: resolved.ID})
				if err != nil {
					return nil, err
				}
				return &repoVisibility{Repo: repoEchoName(args[0], resolved), Visibility: string(out.Visibility)}, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// newRepoEditCmd changes a repository's settings. Visibility is the only one
// today, so --visibility is required; further settings become further flags
// on this command.
func newRepoEditCmd() *cobra.Command {
	var visibility string
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
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				out, err := c.SetRepoVisibility(ctx, &coreapi.SetRepoVisibilityInputBody{Visibility: vis}, coreapi.SetRepoVisibilityParams{RepoId: resolved.ID})
				if err != nil {
					return nil, err
				}
				return &repoVisibility{Repo: repoEchoName(args[0], resolved), Visibility: string(out.Visibility)}, nil
			})
		},
	}
	cmd.Flags().StringVar(&visibility, "visibility", "", "Visibility to set: public or private (required)")
	markRequired(cmd, "visibility")
	addJSONFlag(cmd)
	return cmd
}

// projectFlagName is the --project flag's name, which `repo list` registers,
// requires, groups and reads, and which the other repo verbs pin as absent. The same word is also a NOUN
// in `entire project` and in the grant family's messages; those are a different
// thing that happens to be spelled alike, so they keep their own literals.
const projectFlagName = "project"

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
// The VALUE is what decides, not Changed(): --authoritative=false asks for
// exactly what the GitHub path does, so reporting it as ignored tells the
// caller a flag they turned off was disregarded; false is this flag's default
// and states nothing.
func warnFlagsGitHubViewIgnores(cmd *cobra.Command, authoritative bool) {
	if authoritative {
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: --authoritative is ignored for a GitHub repository; Entire holds no repository record for an upstream, only the mirrors of it.")
	}
}
