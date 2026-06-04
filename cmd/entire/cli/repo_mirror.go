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

	"github.com/entireio/cli/cmd/entire/cli/auth"
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
	cloneURL := fmt.Sprintf("entire://%s/gh/%s/%s", m.ClusterHost, m.Owner, m.Repo)
	private := "no"
	if m.IsPrivate.Or(false) {
		private = "yes"
	}
	return []string{repo, cloneURL, private}
}

// defaultClusterHost is the cluster the mirror commands target when the
// caller omits the <cluster-host> argument. A pragmatic single-region
// default for now — once multi-cluster selection lands this should come
// from config/context rather than a constant.
const defaultClusterHost = "aws-us-east-2.entire.io"

// clusterArg returns the cluster host from the optional second positional
// (after <github-url>), or defaultClusterHost when it was omitted.
func clusterArg(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return defaultClusterHost
}

// clusterHostLabelRe matches one DNS label: alphanumeric, internal hyphens
// allowed, no leading/trailing hyphen.
var clusterHostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// validateClusterHost rejects a cluster host that is anything other than a
// bare DNS name or IP with an optional :port. The host is concatenated as
// "https://"+host both into the smart-HTTP probe URL (waitForMirrorClone) and
// into the STS audience (auth.RepoScopedToken), so a value carrying URL
// metacharacters can redirect the request — and the repo-scoped basic-auth
// token it carries — somewhere other than the intended cluster. Classic case:
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
	return cmd
}

func newRepoMirrorCreateCmd() *cobra.Command {
	var (
		noWait      bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create <github-url> [cluster-host]",
		Short: "Register a GitHub mirror on a cluster",
		Long: "Registers a mirror placement for a GitHub repo on the target " +
			"cluster, then waits for the initial GitHub→EntireDB clone to " +
			"finish so `git clone` works on return. Pass --no-wait to return " +
			"as soon as the placement is registered. Idempotent on " +
			"(upstream, cluster). The cluster-host defaults to " +
			defaultClusterHost + " when omitted.",
		Example: "  entire repo mirror create github.com/octocat/hello-world\n" +
			"  entire repo mirror create github.com/octocat/hello-world eu-west-1.entire.io",
		Args: cobra.RangeArgs(1, 2),
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
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				created, err := c.CreateMirror(ctx, &coreapi.CreateMirrorInputBody{
					Provider:    coreapi.CreateMirrorInputBodyProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: clusterHost,
				})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				repoSlug := "/gh/" + owner + "/" + repo
				return finishMirrorCreate(out, cmd.ErrOrStderr(), created, noWait,
					func() error {
						if _, terr := auth.RepoScopedToken(ctx, "https://"+clusterHost, repoSlug, "pull"); terr != nil {
							return fmt.Errorf("probe mirror for suspension: %w", terr)
						}
						return nil
					},
					func() error {
						return waitForMirrorClone(ctx, out, clusterHost, owner, repo, waitTimeout)
					},
				)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the placement is registered, without waiting for the initial clone")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "how long to wait for the initial clone to finish")
	return cmd
}

// finishMirrorCreate prints the post-create status for `repo mirror create`
// and, unless noWait, makes sure the mirror is usable before returning.
//
// Empty upstream and suspended placement interact. An empty upstream has no
// clone to wait for, so the HEAD-poll loop is skipped — it could only spin to
// the timeout, since an empty repo never advertises a HEAD. But an *existing*
// placement can be suspended even when its upstream is empty, and the
// repo-scoped token exchange is the only signal that surfaces that; a *fresh*
// create can't be suspended (suspension follows upstream access loss), so it
// needs neither the probe nor the wait. Non-empty mirrors take the normal
// clone-wait path.
//
// probeSuspended mints a repo-scoped pull token and returns its error for
// explainSuspendedMirror to classify; waitClone runs the HEAD-poll loop. Both
// are injected so the branching is unit-testable without the auth and
// control-plane stack the production caller wires up.
func finishMirrorCreate(out, errW io.Writer, created *coreapi.CreatedMirror, noWait bool, probeSuspended, waitClone func() error) error {
	if created.Created {
		fmt.Fprintf(out, "Registered mirror %s\n", created.MirrorId)
	} else {
		fmt.Fprintf(out, "Mirror already exists (%s)\n", created.MirrorId)
	}
	fmt.Fprintf(out, "  %s\n", created.MirrorUrl)

	if created.Empty {
		// An existing placement can sit behind a suspension even with an empty
		// upstream, so probe the token exchange to surface it. A fresh create
		// can't be suspended, so skip the probe there.
		if !created.Created {
			if err := probeSuspended(); err != nil {
				if handled, serr := explainSuspendedMirror(errW, created.MirrorId, created.Created, err); handled {
					return serr
				}
				// A non-suspension probe error isn't fatal: the placement exists
				// and the upstream is genuinely empty, so report that rather than
				// failing the create on a transient token hiccup.
			}
		}
		fmt.Fprintln(out, "Upstream has no commits yet — nothing to clone. The mirror will pick up refs once the upstream is pushed to.")
		return nil
	}

	if noWait {
		fmt.Fprintf(out, "Initial clone may still be in progress; `git clone %s` will work once it completes.\n", created.MirrorUrl)
		return nil
	}
	if err := waitClone(); err != nil {
		if handled, serr := explainSuspendedMirror(errW, created.MirrorId, created.Created, err); handled {
			return serr
		}
		return err
	}
	fmt.Fprintf(out, "\nClone it:\n  git clone %s\n", created.MirrorUrl)
	return nil
}

func newRepoMirrorListCmd() *cobra.Command {
	var cluster, provider, owner string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List mirrors you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreList(cmd, mirrorColumns, mirrorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Mirror, error) {
				var params coreapi.ListMirrorsParams
				if cluster != "" {
					params.Cluster = coreapi.NewOptString(cluster)
				}
				if provider != "" {
					params.Provider = coreapi.NewOptString(provider)
				}
				if owner != "" {
					params.Owner = coreapi.NewOptString(owner)
				}
				out, err := c.ListMirrors(ctx, params)
				if err != nil {
					return nil, err
				}
				return out.Mirrors, nil
			})
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "filter by cluster public host")
	cmd.Flags().StringVar(&provider, "provider", "", "filter by upstream provider (e.g. github)")
	cmd.Flags().StringVar(&owner, "owner", "", "filter by upstream owner login")
	return cmd
}

func newRepoMirrorGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <mirror-id>",
		Short: "Show a mirror by ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, mirrorColumns, mirrorRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Mirror, error) {
				sc, err := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: args[0]})
				if err != nil {
					return nil, err
				}
				return sc, nil
			})
		},
	}
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
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
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
