//go:build internal

package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// newBuildkiteOrgCmd is the `entire ci buildkite org` subgroup: an org/project
// admin connects their own Buildkite organization (supplying a Buildkite API
// token entire stores encrypted) and registers the Buildkite hosted clusters
// their pipelines run on. Every verb addresses the entire org by an <entire-org>
// reference (an org ULID or an org name) and resolves it to a ULID via ResolveOrg
// before calling the control plane.
//
// These verbs hit the ORG-scoped CI-webhooks endpoints under
// /api/v1/orgs/{orgId}/ci/buildkite/… added by the DRAFT entiredb#2744. They are
// not yet in this repo's generated coreapi spec, so the requests are hand-rolled
// via the coreapi.Client PostJSON/GetJSON/DeleteJSON escape hatch and decoded
// into the local DTOs below. Once that spec regenerates, swap these calls for the
// generated Invoker methods and delete the DTOs + escape-hatch helpers.
func newBuildkiteOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Manage an org's Buildkite credentials and clusters",
	}
	cmd.AddCommand(newBuildkiteOrgConnectCmd())
	cmd.AddCommand(newBuildkiteOrgListCmd())
	cmd.AddCommand(newBuildkiteOrgClusterCmd())
	cmd.AddCommand(newBuildkiteOrgDisconnectCmd())
	return cmd
}

func newBuildkiteOrgConnectCmd() *cobra.Command {
	var (
		bkOrg   string
		bkToken string
	)
	cmd := &cobra.Command{
		Use:   "connect <entire-org>",
		Short: "Connect (or rotate) an org's Buildkite API credential",
		Long: "Connect a Buildkite organization to an Entire org by storing its API token.\n\n" +
			"<entire-org> is an Entire org name or an org ULID. --bk-org is the Buildkite\n" +
			"organization slug. The Buildkite API token is read, in order, from --bk-token,\n" +
			"the BUILDKITE_API_TOKEN env var, or an interactive no-echo prompt. Prefer the\n" +
			"env var or prompt: a token passed via --bk-token lands in your shell history.\n\n" +
			"The token is sent to the server over TLS, stored encrypted, and NEVER echoed\n" +
			"back or logged — only its byte length is reported. Re-running with a new token\n" +
			"rotates the stored one (the server answers 200 instead of 201).",
		Example: "  BUILDKITE_API_TOKEN=bkua_… entire ci buildkite org connect acme --bk-org acme-inc\n" +
			"  entire ci buildkite org connect 01J… --bk-org acme-inc",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := resolveBuildkiteToken(cmd, bkToken)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := ResolveOrg(ctx, c, args[0])
				if err != nil {
					return err
				}
				body := connectOrgCredentialRequest{BKOrganization: bkOrg, BKAPIToken: token}
				var view orgBuildkiteCredentialView
				status, err := c.PostJSON(ctx, orgCredentialPath(orgID), body, &view)
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), view)
				}
				if status == http.StatusOK {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Rotated Buildkite credential for %q (token_len=%d)\n", view.BKOrganization, view.TokenLen)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Connected Buildkite org %q (token_len=%d)\n", view.BKOrganization, view.TokenLen)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&bkOrg, "bk-org", "", "Buildkite organization slug (required)")
	cmd.Flags().StringVar(&bkToken, "bk-token", "", "Buildkite API token (insecure: lands in shell history — prefer BUILDKITE_API_TOKEN or the prompt)")
	markRequired(cmd, "bk-org")
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteOrgListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <entire-org>",
		Short: "List an org's connected Buildkite credentials and clusters",
		Long: "List the Buildkite organizations connected to an Entire org and the hosted\n" +
			"clusters registered for each. No token material is shown — only each\n" +
			"credential's byte length.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := ResolveOrg(ctx, c, args[0])
				if err != nil {
					return err
				}
				var credsResp listOrgCredentialsResponse
				if err := c.GetJSON(ctx, orgCredentialsPath(orgID), nil, &credsResp); err != nil {
					return err
				}
				// Clusters are scoped to a single Buildkite org, so fan out one
				// clusters query per connected credential to assemble the org's
				// full view. No credentials means nothing to enumerate.
				var clusters []orgBuildkiteClusterView
				for _, cred := range credsResp.Credentials {
					cls, err := fetchOrgClusters(ctx, c, orgID, cred.BKOrganization)
					if err != nil {
						return err
					}
					clusters = append(clusters, cls...)
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), orgBuildkiteListView{
						Credentials: nonNilCreds(credsResp.Credentials),
						Clusters:    nonNilClusters(clusters),
					})
				}
				return printOrgBuildkiteList(cmd.OutOrStdout(), credsResp.Credentials, clusters)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteOrgClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage an org's Buildkite hosted clusters",
	}
	cmd.AddCommand(newBuildkiteOrgClusterAddCmd())
	return cmd
}

func newBuildkiteOrgClusterAddCmd() *cobra.Command {
	var (
		bkOrg         string
		bkClusterID   string
		authPluginRef string
	)
	cmd := &cobra.Command{
		Use:   "add <entire-org>",
		Short: "Register (or update) a Buildkite hosted cluster for an org",
		Long: "Register a Buildkite hosted cluster so the org's pipelines can run on it.\n\n" +
			"<entire-org> is an Entire org name or an org ULID. The upsert key is\n" +
			"(--bk-org, --bk-cluster-id); re-running updates the row (the server answers\n" +
			"200 instead of 201). --auth-plugin-ref pins the entire-core-auth Buildkite\n" +
			"plugin (org/name#version); omit it to use the server's pinned default.",
		Example: "  entire ci buildkite org cluster add acme --bk-org acme-inc --bk-cluster-id 0195… \\\n" +
			"    --auth-plugin-ref entire-io/entire-core-auth#v1.2.3",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := ResolveOrg(ctx, c, args[0])
				if err != nil {
					return err
				}
				body := registerOrgClusterRequest{
					BKOrganization: bkOrg,
					BKClusterID:    bkClusterID,
					AuthPluginRef:  authPluginRef,
				}
				var view orgBuildkiteClusterView
				status, err := c.PostJSON(ctx, orgClustersPath(orgID), body, &view)
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), view)
				}
				verb := "Registered"
				if status == http.StatusOK {
					verb = "Updated"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s Buildkite cluster %s for org %q\n", verb, view.BKClusterID, view.BKOrganization)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&bkOrg, "bk-org", "", "Buildkite organization slug (required)")
	cmd.Flags().StringVar(&bkClusterID, "bk-cluster-id", "", "Buildkite hosted-cluster ID (required)")
	cmd.Flags().StringVar(&authPluginRef, "auth-plugin-ref", "", "entire-core-auth plugin ref (org/name#version); empty uses the server default")
	markRequired(cmd, "bk-org", "bk-cluster-id")
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteOrgDisconnectCmd() *cobra.Command {
	var bkOrg string
	cmd := &cobra.Command{
		Use:   "disconnect <entire-org>",
		Short: "Disconnect an org's Buildkite credential",
		Long: "Remove the stored Buildkite API credential for --bk-org from an Entire org.\n\n" +
			"<entire-org> is an Entire org name or an org ULID. Registered clusters are\n" +
			"config, not secrets, and are left in place.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := ResolveOrg(ctx, c, args[0])
				if err != nil {
					return err
				}
				if err := c.DeleteJSON(ctx, orgCredentialPath(orgID)+"/"+url.PathEscape(bkOrg)); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Disconnected Buildkite org %q\n", bkOrg)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&bkOrg, "bk-org", "", "Buildkite organization slug to disconnect (required)")
	markRequired(cmd, "bk-org")
	return cmd
}

// orgCredentialPath / orgCredentialsPath / orgClustersPath build the escape-hatch
// request paths (relative to the client's /api/v1 base). Kept as helpers so the
// route strings live in exactly one place.
func orgCredentialPath(orgID string) string  { return "orgs/" + orgID + "/ci/buildkite/credential" }
func orgCredentialsPath(orgID string) string { return "orgs/" + orgID + "/ci/buildkite/credentials" }
func orgClustersPath(orgID string) string    { return "orgs/" + orgID + "/ci/buildkite/clusters" }

// fetchOrgClusters lists the clusters registered for one Buildkite org under the
// Entire org. The server requires the bk_organization query parameter — the
// registry is always org+bk-org-scoped.
func fetchOrgClusters(ctx context.Context, c *coreapi.Client, orgID, bkOrg string) ([]orgBuildkiteClusterView, error) {
	q := url.Values{}
	q.Set("bk_organization", bkOrg)
	var resp listOrgClustersResponse
	if err := c.GetJSON(ctx, orgClustersPath(orgID), q, &resp); err != nil {
		return nil, err
	}
	return resp.Clusters, nil
}

// resolveBuildkiteToken sources the Buildkite API token for `org connect`,
// preferring the safest input the caller supplied. Precedence: the --bk-token
// flag (with a one-line shell-history warning), then the BUILDKITE_API_TOKEN
// env var, then an interactive no-echo prompt when a TTY is available. It never
// logs or echoes the token; the returned value is sent to the server over TLS
// and nowhere else.
func resolveBuildkiteToken(cmd *cobra.Command, flagToken string) (string, error) {
	if strings.TrimSpace(flagToken) != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: --bk-token is visible in your shell history; prefer BUILDKITE_API_TOKEN or the interactive prompt")
		return flagToken, nil
	}
	if env := strings.TrimSpace(os.Getenv("BUILDKITE_API_TOKEN")); env != "" {
		return env, nil
	}
	if interactive.CanPromptInteractively() {
		return promptBuildkiteToken(cmd)
	}
	return "", errors.New("no Buildkite API token: set BUILDKITE_API_TOKEN, pass --bk-token, or run interactively to be prompted")
}

// promptBuildkiteToken reads a Buildkite API token from the terminal without
// echoing it. Only reached when interactive.CanPromptInteractively() is true, so
// os.Stdin is a real terminal.
func promptBuildkiteToken(cmd *cobra.Command) (string, error) {
	fmt.Fprint(cmd.ErrOrStderr(), "Buildkite API token: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // G115: uintptr->int is safe for a stdin fd
	fmt.Fprintln(cmd.ErrOrStderr())                   // terminate the (hidden) input line
	if err != nil {
		return "", fmt.Errorf("read Buildkite API token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("no Buildkite API token entered")
	}
	return token, nil
}

// connectOrgCredentialRequest / registerOrgClusterRequest are the POST bodies for
// the org connect / cluster-add verbs, mirroring entiredb#2744's
// ConnectOrgCIBuildkiteCredentialInput.Body / RegisterOrgCIBuildkiteClusterInput.Body.
type connectOrgCredentialRequest struct {
	BKOrganization string `json:"bk_organization"`
	BKAPIToken     string `json:"bk_api_token"`
}

type registerOrgClusterRequest struct {
	BKOrganization string `json:"bk_organization"`
	BKClusterID    string `json:"bk_cluster_id"`
	Label          string `json:"label,omitempty"`
	AuthPluginRef  string `json:"auth_plugin_ref,omitempty"`
}

// orgBuildkiteCredentialView / orgBuildkiteClusterView mirror entiredb#2744's
// corev1.OrgCIBuildkiteCredentialView / OrgCIBuildkiteClusterView field for
// field. The credential view carries no token — only token_len — matching the
// server, which never returns token material.
type orgBuildkiteCredentialView struct {
	BKOrganization string    `json:"bk_organization"`
	Owner          string    `json:"owner"`
	CipherVersion  int       `json:"cipher_version"`
	TokenLen       int       `json:"token_len"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type orgBuildkiteClusterView struct {
	BKOrganization string    `json:"bk_organization"`
	Owner          string    `json:"owner"`
	BKClusterID    string    `json:"bk_cluster_id"`
	Label          string    `json:"label"`
	AuthPluginRef  string    `json:"auth_plugin_ref"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type listOrgCredentialsResponse struct {
	Credentials []orgBuildkiteCredentialView `json:"credentials"`
}

type listOrgClustersResponse struct {
	Clusters []orgBuildkiteClusterView `json:"clusters"`
}

// orgBuildkiteListView is the combined --json shape for `org list`: the org's
// connected credentials plus every registered cluster across them.
type orgBuildkiteListView struct {
	Credentials []orgBuildkiteCredentialView `json:"credentials"`
	Clusters    []orgBuildkiteClusterView    `json:"clusters"`
}

// printOrgBuildkiteList renders the human view of `org list`: a credentials
// section then a clusters section, each an aligned table. No token material.
func printOrgBuildkiteList(w io.Writer, creds []orgBuildkiteCredentialView, clusters []orgBuildkiteClusterView) error {
	if len(creds) == 0 {
		fmt.Fprintln(w, "No Buildkite credentials connected for this org.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREDENTIALS")
	fmt.Fprintln(tw, "BK_ORG\tTOKEN_LEN\tCIPHER\tUPDATED")
	for _, cr := range creds {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", cr.BKOrganization, cr.TokenLen, cr.CipherVersion, fmtTime(cr.UpdatedAt))
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "CLUSTERS")
	fmt.Fprintln(tw, "BK_ORG\tCLUSTER_ID\tLABEL\tAUTH_PLUGIN_REF\tUPDATED")
	if len(clusters) == 0 {
		fmt.Fprintln(tw, "(none)")
	}
	for _, cl := range clusters {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cl.BKOrganization, cl.BKClusterID, orDash(cl.Label), orDash(cl.AuthPluginRef), fmtTime(cl.UpdatedAt))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render table: %w", err)
	}
	return nil
}

// fmtTime renders a timestamp compactly in UTC for the list tables.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// nonNilCreds / nonNilClusters coerce a nil slice to an empty one so the --json
// output encodes [] rather than null (scripts expect an array).
func nonNilCreds(s []orgBuildkiteCredentialView) []orgBuildkiteCredentialView {
	if s == nil {
		return []orgBuildkiteCredentialView{}
	}
	return s
}

func nonNilClusters(s []orgBuildkiteClusterView) []orgBuildkiteClusterView {
	if s == nil {
		return []orgBuildkiteClusterView{}
	}
	return s
}
