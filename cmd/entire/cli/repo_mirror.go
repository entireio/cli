package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// mirrorColumns is the human table/field view of a mirror: the scannable
// repo name, the clone URL you'd copy, and whether the upstream is
// private. The cluster is omitted — it's already embedded in the clone
// URL — and the wire model's internal ids are dropped entirely. The clone
// URL is synthesised from the mirror's coords (the form `git clone`
// accepts), since the list API doesn't return it.
var mirrorColumns = []string{"REPO", "CLONE URL", "PRIVATE"}

func mirrorRow(m coreapi.Mirror) []string {
	repo := m.Owner + "/" + m.Repo
	cloneURL := mirrorCloneURL(m.ClusterHost, m.Owner, m.Repo)
	private := "no"
	if m.IsPrivate.Or(false) {
		private = "yes"
	}
	return []string{repo, cloneURL, private}
}

// availableMirrorColumns is the view of a repo you *could* mirror: the
// scannable repo name, your effective GitHub access, and whether it's
// onboardable. STATUS is "available" (run `entire repo mirror create` to
// onboard), "mirrored" (already done — `entire repo mirror list` shows the
// clone URL), or "owner-only" (a personal repo of another user; only its
// owner may mirror it). No clone URL column: an un-onboarded repo doesn't
// have one yet.
var availableMirrorColumns = []string{"REPO", "ACCESS", "STATUS"}

func availableMirrorRow(m coreapi.AvailableMirror) []string {
	return []string{m.Owner + "/" + m.Repo, string(m.Access), string(m.Status)}
}

// defaultClusterHost is the cluster the positional-arg mirror commands target
// when the caller omits the <cluster-host> argument. The no-arg create wizard
// instead enumerates real clusters from the catalog (GET /api/v1/clusters, see
// availableRegions in repo_mirror_create_wizard.go); this stays as the
// single-region fallback for the explicit `create <github-url>` form.
const defaultClusterHost = "aws-us-east-2.entire.io"

// clusterArg returns the cluster host from the optional second positional
// (after <github-url>), or defaultClusterHost when it was omitted.
func clusterArg(args []string) string {
	return clusterArgAt(args, 1)
}

// clusterArgAt returns the cluster host from the optional positional at idx,
// or defaultClusterHost when it was omitted. Commands with an intervening
// positional (e.g. collaborators add <github-url> <handle> [cluster-host])
// pass the trailing index.
func clusterArgAt(args []string, idx int) string {
	if len(args) > idx {
		return args[idx]
	}
	return defaultClusterHost
}

// clusterHostLabelRe matches one DNS label: alphanumeric, internal hyphens
// allowed, no leading/trailing hyphen.
var clusterHostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// validateClusterHost rejects a cluster host that is anything other than a
// bare DNS name or IP with an optional :port. The host is concatenated as
// "https://"+host into the clone URL and the STS audience
// (auth.RepoScopedToken), so a value carrying URL metacharacters can redirect
// the request — and the repo-scoped basic-auth token it carries — somewhere
// other than the intended cluster. Classic case:
// `aws-us-east-2.entire.io@evil.com`, which Go's URL parser reads as
// host=evil.com with the real cluster demoted to userinfo, leaking the token
// to evil.com. We parse the host the same way the rest of the code does and
// require it to round-trip to a bare host with no userinfo, path, query, or
// fragment, then confirm the hostname is a valid IP or DNS name. This is
// cheap client-side defense-in-depth and doesn't depend on the server's STS
// invalid_target canonicalization catching the trick.
func validateClusterHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("cluster host is empty")
	}
	u, err := url.Parse("https://" + host)
	if err != nil {
		return fmt.Errorf("%q is not a valid host", host)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host != host {
		return fmt.Errorf("%q must be a bare host[:port] (no scheme, userinfo, path, query, or fragment)", host)
	}
	hostname := u.Hostname()
	if net.ParseIP(hostname) != nil {
		return nil
	}
	for _, label := range strings.Split(hostname, ".") {
		if !clusterHostLabelRe.MatchString(label) {
			return fmt.Errorf("%q is not a valid DNS name or IP", host)
		}
	}
	return nil
}

// newRepoMirrorCmd is the `entire repo mirror` subtree: manage EntireDB
// GitHub-mirror placements on a cluster. Mirrors the standalone entiredb
// CLI's `entire repo mirror` surface for the server-side half (create /
// list / get / remove). The local-clone rewrite (`mirror use`) is not
// ported — it's a git-config + git-remote-entire concern outside the
// control-plane API.
func newRepoMirrorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage GitHub-mirror placements on EntireDB clusters",
	}
	cmd.AddCommand(newRepoMirrorCreateCmd())
	cmd.AddCommand(newRepoMirrorListCmd())
	cmd.AddCommand(newRepoMirrorGetCmd())
	cmd.AddCommand(newRepoMirrorRemoveCmd())
	cmd.AddCommand(newRepoMirrorCollaboratorsCmd())
	return cmd
}

func newRepoMirrorCreateCmd() *cobra.Command {
	var (
		noWait      bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create [github-url] [cluster-host]",
		Short: "Register a GitHub mirror on a cluster",
		Long: "With no arguments, launches an interactive wizard: pick repos to " +
			"mirror, pick one or more regions, then creates every (repo, region) " +
			"mirror in parallel and prints the clone URLs.\n\n" +
			"With a <github-url>, registers a mirror placement for that repo on " +
			"the target cluster, then waits for the initial GitHub→EntireDB clone " +
			"to finish so `git clone` works on return. Pass --no-wait to return " +
			"as soon as the placement is registered. Idempotent on " +
			"(upstream, cluster). The cluster-host defaults to " +
			defaultClusterHost + " when omitted (the interactive wizard, with " +
			"no args, instead lets you pick clusters).",
		Example: "  entire repo mirror create\n" +
			"  entire repo mirror create github.com/octocat/hello-world\n" +
			"  entire repo mirror create github.com/octocat/hello-world aws-us-east-2.entire.io",
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runMirrorCreateWizard(cmd, noWait, waitTimeout)
			}
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			// The non-interactive one-shot keeps a fixed default cluster
			// (defaultClusterHost) when [cluster-host] is omitted — catalog-based
			// cluster guessing is intentionally limited to the interactive
			// wizard (the no-args path above), so scripts get stable behavior.
			clusterHost := clusterArg(args)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				errW := cmd.ErrOrStderr()
				stop := startSpinner(errW, fmt.Sprintf("Cloning %s/%s into %s", owner, repo, clusterHost))
				// nil onStatus: the one-shot's single spinner shows liveness; the
				// per-mirror progress lines are the wizard's concern.
				outcome, err := createAndAwaitMirror(ctx, c, owner, repo, clusterHost, noWait, waitTimeout, nil)
				// Only a confirmed-ready clone earns the ✓; everything else
				// (empty, --no-wait, suspended, failed, timeout) erases the line
				// and lets reportOneShotMirror print the specific outcome.
				stop(err == nil && outcome.polled && outcome.status == coreapi.MirrorStatusReady)
				return reportOneShotMirror(cmd.OutOrStdout(), errW, outcome, err)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the placement is registered, without waiting for the initial clone")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "how long to wait for the initial clone to finish")
	return cmd
}

// mirrorCreateOutcome bundles the create response with the clone status
// observed while waiting. polled is false for --no-wait and for empty upstreams,
// where there is nothing to await; in those cases status is unset.
type mirrorCreateOutcome struct {
	created *coreapi.CreatedMirror
	status  coreapi.MirrorStatus
	polled  bool
}

// createAndAwaitMirror is the single create-then-wait path shared by the
// `repo mirror create <github-url>` one-shot and the onboarding wizard, so both
// report identical lifecycle states. It registers the GitHub mirror on
// clusterHost (idempotent on (upstream, cluster)) and, unless noWait or the
// upstream is empty, polls the control plane until the clone reaches a terminal
// status. The returned error is the create error (when outcome.created is nil)
// or the wait error — a status sentinel (errMirrorCloneFailed /
// errMirrorSuspended) or a timeout; callers read outcome.status for the state.
func createAndAwaitMirror(ctx context.Context, c *coreapi.Client, owner, repo, clusterHost string, noWait bool, timeout time.Duration, onStatus func(coreapi.MirrorStatus)) (mirrorCreateOutcome, error) {
	created, err := c.CreateMirror(ctx, &coreapi.CreateMirrorInputBody{
		Provider:    coreapi.CreateMirrorInputBodyProviderGithub,
		Owner:       owner,
		Repo:        repo,
		ClusterHost: clusterHost,
	})
	if err != nil {
		return mirrorCreateOutcome{}, err
	}
	outcome := mirrorCreateOutcome{created: created}
	if created.Empty {
		// An empty upstream has nothing to clone, so don't poll for "ready" — it
		// never would. But an *existing* placement can be suspended even when
		// empty, and one status read surfaces that (a fresh create can't be
		// suspended — suspension follows upstream access loss). Mirrors the old
		// finishMirrorCreate behavior; the read is best-effort, so a transient
		// GetMirror error just falls through to the benign "nothing to clone".
		if !created.Created {
			if m, gerr := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: created.MirrorId}); gerr == nil {
				if s, ok := m.Status.Get(); ok && s == coreapi.MirrorStatusSuspended {
					outcome.status = s
					outcome.polled = true
					return outcome, errMirrorSuspended
				}
			}
		}
		return outcome, nil
	}
	if noWait {
		return outcome, nil
	}
	status, werr := awaitMirrorReady(ctx, c, created.MirrorId, timeout, onStatus)
	outcome.status = status
	outcome.polled = true
	return outcome, werr
}

// reportOneShotMirror renders the human output for `repo mirror create
// <github-url>` from the shared createAndAwaitMirror result. A nil
// outcome.created means CreateMirror itself failed — surface that error (nothing
// was printed yet). Otherwise echo the placement, then the lifecycle outcome.
func reportOneShotMirror(out, errW io.Writer, outcome mirrorCreateOutcome, err error) error {
	created := outcome.created
	if created == nil {
		return err
	}
	if created.Created {
		fmt.Fprintf(out, "Registered mirror %s\n", created.MirrorId)
	} else {
		fmt.Fprintf(out, "Mirror already exists (%s)\n", created.MirrorId)
	}
	fmt.Fprintf(out, "  %s\n", created.MirrorUrl)

	if !outcome.polled {
		if created.Empty {
			fmt.Fprintln(out, "Upstream has no commits yet — nothing to clone. The mirror will pick up refs once the upstream is pushed to.")
		} else {
			fmt.Fprintf(out, "Initial clone may still be in progress; `git clone %s` will work once it completes.\n", created.MirrorUrl)
		}
		return nil
	}

	switch outcome.status {
	case coreapi.MirrorStatusReady:
		fmt.Fprintf(out, "\nClone it:\n  git clone %s\n", created.MirrorUrl)
		return nil
	case coreapi.MirrorStatusSuspended:
		explainSuspendedMirror(errW, created.MirrorId)
		return NewSilentError(errMirrorSuspended)
	case coreapi.MirrorStatusFailed:
		return fmt.Errorf("initial clone of mirror %s failed", created.MirrorId)
	case coreapi.MirrorStatusProcessing:
		// Still processing when the poll returned: the wait timed out (or a
		// transport error broke the poll). awaitMirrorReady's err carries which.
		return err
	default:
		return err
	}
}

func newRepoMirrorListCmd() *cobra.Command {
	var cluster, provider, owner string
	var showAvailable bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List mirrors you can see (or, with --show-available, repos you could mirror)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showAvailable {
				return runCoreList(cmd, availableMirrorColumns, availableMirrorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.AvailableMirror, error) {
					// Computed live from GitHub using your own login, so name the
					// core being dialled (same rationale as the existing-mirror
					// banner). --cluster/--provider don't apply here: the
					// onboardable set is cluster-agnostic and GitHub-only.
					if !jsonRequested(cmd) {
						fmt.Fprintf(cmd.ErrOrStderr(), "Listing repos you could mirror, via %s\n", c.CoreOrigin())
					}
					var params coreapi.ListAvailableMirrorsParams
					if owner != "" {
						params.Owner = coreapi.NewOptString(owner)
					}
					out, err := c.ListAvailableMirrors(ctx, params)
					if err != nil {
						return nil, err
					}
					return out.Available, nil
				})
			}
			return runCoreList(cmd, mirrorColumns, mirrorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Mirror, error) {
				// mirror list is identity-scoped: it shows the mirrors visible
				// from the active login's federation, so naming that login server
				// makes a surprising empty result legible — e.g. mirrors in a
				// different deployment than the active context (--cluster is a
				// filter, not a router). Name the core the client actually dials
				// (c.CoreOrigin) so the banner can never diverge from where the
				// request goes — in particular it reflects ENTIRE_TOKEN's aud,
				// which a separately-resolved ResolveControlPlaneTarget would miss.
				// On stderr so it never lands in a piped table; skipped for --json
				// to keep machine output clean.
				if !jsonRequested(cmd) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Listing mirrors on %s\n", c.CoreOrigin())
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
					params := coreapi.ListMirrorsParams{}
					if cluster != "" {
						params.Cluster = coreapi.NewOptString(cluster)
					}
					if provider != "" {
						params.Provider = coreapi.NewOptString(provider)
					}
					if owner != "" {
						params.Owner = coreapi.NewOptString(owner)
					}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListMirrors(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Mirrors, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "filter by cluster public host")
	cmd.Flags().StringVar(&provider, "provider", "", "filter by upstream provider (e.g. github)")
	cmd.Flags().StringVar(&owner, "owner", "", "filter by upstream owner login")
	cmd.Flags().BoolVar(&showAvailable, "show-available", false, "instead of existing mirrors, list GitHub repos you could onboard as mirrors (ignores --cluster/--provider)")
	return cmd
}

func newRepoMirrorGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <mirror>",
		Short: "Show a mirror by ULID or clone URL",
		Long: "Show a mirror. <mirror> is either a mirror ULID or an entire:// clone " +
			"URL\n(entire://<cluster>/gh/<owner>/<repo>) — the form `mirror list` " +
			"prints and `git clone` accepts.",
		Example: "  entire repo mirror get 01KS6KFJR2XS6PZ188MVYE07AN\n" +
			"  entire repo mirror get entire://aws-us-east-2.entire.io/gh/octocat/hello-world",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, mirrorColumns, mirrorRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Mirror, error) {
				mirrorID, err := resolveMirrorRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: mirrorID})
			})
		},
	}
}

// resolveMirrorRef turns a mirror reference into its ULID. A ULID passes
// through unchanged. Otherwise the ref is parsed as an entire:// clone URL and
// resolved by listing the caller-visible mirrors for that (cluster, provider,
// owner) and matching the repo — there is no get-by-coords endpoint, only
// GetMirror(ULID). The clone URL carries the cluster, so the match is
// unambiguous even when the same upstream is mirrored on several clusters.
func resolveMirrorRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	clusterHost, provider, owner, repo, err := parseMirrorCloneURL(ref)
	if err != nil {
		return "", fmt.Errorf("%w; pass a mirror ULID or a clone URL (entire://<cluster>/gh/<owner>/<repo>)", err)
	}
	mirrors, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
		params := coreapi.ListMirrorsParams{
			Cluster:  coreapi.NewOptString(clusterHost),
			Provider: coreapi.NewOptString(provider),
			Owner:    coreapi.NewOptString(owner),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, lerr := c.ListMirrors(ctx, params)
		if lerr != nil {
			return nil, "", lerr
		}
		return out.Mirrors, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return "", err
	}
	// ListMirrors has no repo filter, so the owner-scoped page is matched on
	// repo client-side. Owner/repo are stored lowercase; EqualFold guards
	// against a differently-cased clone URL.
	for _, m := range mirrors {
		if strings.EqualFold(m.Repo, repo) {
			return m.MirrorId, nil
		}
	}
	return "", noMirrorErr(ref)
}

// parseMirrorCloneURL decomposes an entire:// mirror clone URL into its
// coordinates:
//
//	entire://<clusterHost>/gh/<owner>/<repo>
//
// Only the github ("gh") provider path is recognized — the only provider
// mirrors support today. The cluster host is validated the same way the
// create/remove verbs validate it, so a host carrying URL metacharacters is
// rejected at the boundary rather than flowing into the list filter.
func parseMirrorCloneURL(raw string) (clusterHost, provider, owner, repo string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || u.Scheme != "entire" {
		return "", "", "", "", fmt.Errorf("%q is not an entire:// clone URL", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "gh" {
		return "", "", "", "", fmt.Errorf("%q must be entire://<cluster>/gh/<owner>/<repo>", raw)
	}
	if verr := validateClusterHost(u.Host); verr != nil {
		return "", "", "", "", verr
	}
	// Trim a trailing .git so a URL pasted from `git remote -v` resolves the
	// same as the bare clone URL (matching gitremote.ParseURL). GitHub repo
	// names can contain dots, so only the suffix is trimmed, not all dots.
	repo = strings.ToLower(strings.TrimSuffix(parts[2], ".git"))
	return u.Host, string(coreapi.CreateMirrorInputBodyProviderGithub), strings.ToLower(parts[1]), repo, nil
}

func noMirrorErr(ref string) error {
	return fmt.Errorf("no mirror matching %q (run `entire repo mirror list` to see clone URLs, or pass a ULID)", ref)
}

func newRepoMirrorRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <github-url> [cluster-host]",
		Short: "Un-register a GitHub mirror from a cluster",
		Long: "Removes a mirror placement for a GitHub repo from the target " +
			"cluster. Other clusters' placements of the same upstream are " +
			"unaffected. The cluster-host defaults to " + defaultClusterHost +
			" when omitted.",
		Example: "  entire repo mirror remove github.com/octocat/hello-world",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			clusterHost := clusterArg(args)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				// Delete by upstream coords in one call. A 404 is a real
				// error here, not idempotent success: the server only
				// answers 204 when it actually removed a placement, so a
				// 404 ("no such mirror / not visible / different cluster")
				// surfaces verbatim via renderCoreError rather than being
				// reported as a successful removal.
				if err := c.DeleteMirror(ctx, coreapi.DeleteMirrorParams{
					Provider:    coreapi.DeleteMirrorProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: clusterHost,
				}); err != nil {
					return err
				}
				cmd.Printf("Removed mirror github.com/%s/%s from %s\n", owner, repo, clusterHost)
				return nil
			})
		},
	}
}
