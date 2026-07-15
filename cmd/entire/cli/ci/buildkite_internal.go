//go:build internal

package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// newBuildkiteCmd is the `entire ci buildkite` subgroup: manage a repo's
// Buildkite CI-webhook subscriptions through the core-mediated management API
// (POST/GET/PATCH/DELETE /api/v1/repos/{repoId}/ci-webhooks). Every verb
// addresses a repo by the same reference ResolveNativeRepo accepts (a native
// <project>/<repo> path or a raw repo ULID) and resolves it to a ULID before
// calling the control plane.
func newBuildkiteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buildkite",
		Short: "Manage Buildkite CI webhook integrations",
	}
	cmd.AddCommand(newBuildkiteEnrollCmd())
	cmd.AddCommand(newBuildkiteListCmd())
	cmd.AddCommand(newBuildkiteUpdateCmd())
	cmd.AddCommand(newBuildkiteRemoveCmd())
	cmd.AddCommand(newBuildkiteWatchCmd())
	return cmd
}

func newBuildkiteEnrollCmd() *cobra.Command {
	var (
		bkOrg       string
		bkClusterID string
		refFilter   string
		events      []string
		displayName string
	)
	cmd := &cobra.Command{
		Use:   "enroll <repo> [pipeline]",
		Short: "Enroll a repo for Buildkite CI webhooks",
		Long: "Enroll a native repo for Buildkite CI webhooks.\n\n" +
			"<repo> is a native <project>/<repo> path or a repo ULID. [pipeline] is the\n" +
			"Buildkite pipeline slug; omit it to let the server default to the repo name.\n\n" +
			"The operation is idempotent: a fresh enroll returns 201, re-enrolling an\n" +
			"existing subscription returns 200 with the current config. No Buildkite API\n" +
			"token is passed — it is an operator-seeded, org-level credential.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := ResolveNativeRepo(ctx, c, args[0])
				if err != nil {
					return err
				}
				body := &coreapi.CreateRepoCIWebhookInputBody{
					Provider:       coreapi.CreateRepoCIWebhookInputBodyProviderBuildkite,
					BkOrganization: bkOrg,
					BkClusterID:    bkClusterID,
				}
				if len(args) == 2 {
					body.BkPipeline = coreapi.NewOptString(args[1])
				}
				if cmd.Flags().Changed("ref-filter") {
					body.RefFilter = coreapi.NewOptString(refFilter)
				}
				if len(events) > 0 {
					body.Events = events
				}
				if cmd.Flags().Changed("display-name") {
					body.DisplayName = coreapi.NewOptString(displayName)
				}
				res, err := c.CreateRepoCIWebhook(ctx, body, coreapi.CreateRepoCIWebhookParams{RepoId: repoID})
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					// Pass a pointer so the generated (*CIWebhookView).MarshalJSON
					// is used (a value would fall back to field encoding and break
					// on an unset $schema OptURI field).
					return printJSON(cmd.OutOrStdout(), &res.Response)
				}
				verb := "Enrolled"
				if res.StatusCode == http.StatusOK {
					verb = "Re-enrolled"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s %s for Buildkite (subscription %s)\n", verb, args[0], res.Response.ID)
				return printSubscriptionFields(cmd.OutOrStdout(), res.Response)
			})
		},
	}
	cmd.Flags().StringVar(&bkOrg, "bk-org", "", "Buildkite organization slug (required)")
	// bk-cluster-id is required by the server (the leaf attaches the pipeline to
	// a Buildkite cluster). It is not marked required here because a later PR
	// resolves it via a cluster-discovery picker; until then the operator passes
	// it and the server rejects its absence with a clear validation error.
	cmd.Flags().StringVar(&bkClusterID, "bk-cluster-id", "", "Buildkite cluster ID to attach the pipeline to (required by the server)")
	cmd.Flags().StringVar(&refFilter, "ref-filter", "", "Only fire for refs matching this glob (e.g. refs/heads/main)")
	cmd.Flags().StringSliceVar(&events, "events", nil, "Ref events to subscribe to (comma-separated, e.g. create,update)")
	cmd.Flags().StringVar(&displayName, "display-name", "", "Human-friendly name for the subscription")
	markRequired(cmd, "bk-org")
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List a repo's Buildkite CI webhook subscriptions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := ResolveNativeRepo(ctx, c, args[0])
				if err != nil {
					return err
				}
				out, err := c.ListRepoCIWebhooks(ctx, coreapi.ListRepoCIWebhooksParams{RepoId: repoID})
				if err != nil {
					return err
				}
				subs := out.Subscriptions
				if jsonRequested(cmd) {
					if subs == nil {
						subs = []coreapi.CIWebhookView{} // a nil slice encodes as null; scripts expect []
					}
					return printJSON(cmd.OutOrStdout(), subs)
				}
				if len(subs) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No Buildkite subscriptions for this repo.")
					return nil
				}
				return printSubscriptionsTable(cmd.OutOrStdout(), subs)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteUpdateCmd() *cobra.Command {
	var (
		enabled     bool
		refFilter   string
		events      []string
		displayName string
	)
	cmd := &cobra.Command{
		Use:   "update <repo> <subscriptionId>",
		Short: "Update a Buildkite CI webhook subscription",
		Long: "Update mutable fields of a Buildkite CI webhook subscription.\n\n" +
			"Only the flags you set are sent; the (repo, org, pipeline) key and the\n" +
			"Buildkite token are immutable. Pass at least one field flag.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !anyChanged(cmd, "enabled", "ref-filter", "events", "display-name") {
				cmd.SilenceUsage = true
				return errors.New("nothing to update: set at least one of --enabled, --ref-filter, --events, --display-name")
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := ResolveNativeRepo(ctx, c, args[0])
				if err != nil {
					return err
				}
				body := &coreapi.PatchRepoCIWebhookInputBody{}
				if cmd.Flags().Changed("enabled") {
					body.Enabled = coreapi.NewOptBool(enabled)
				}
				if cmd.Flags().Changed("ref-filter") {
					body.RefFilter = coreapi.NewOptString(refFilter)
				}
				if cmd.Flags().Changed("events") {
					// A non-nil (possibly empty) slice is what the encoder emits,
					// letting `--events ""` clear the subscription's events.
					if events == nil {
						events = []string{}
					}
					body.Events = events
				}
				if cmd.Flags().Changed("display-name") {
					body.DisplayName = coreapi.NewOptString(displayName)
				}
				sub, err := c.PatchRepoCIWebhook(ctx, body, coreapi.PatchRepoCIWebhookParams{RepoId: repoID, ID: args[1]})
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), sub)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Updated subscription %s\n", sub.ID)
				return printSubscriptionFields(cmd.OutOrStdout(), *sub)
			})
		},
	}
	cmd.Flags().BoolVar(&enabled, "enabled", false, "Enable (--enabled) or disable (--enabled=false) the subscription")
	cmd.Flags().StringVar(&refFilter, "ref-filter", "", "Only fire for refs matching this glob (e.g. refs/heads/main)")
	cmd.Flags().StringSliceVar(&events, "events", nil, "Ref events to subscribe to (comma-separated); pass an empty value to clear")
	cmd.Flags().StringVar(&displayName, "display-name", "", "Human-friendly name for the subscription")
	addJSONFlag(cmd)
	return cmd
}

func newBuildkiteRemoveCmd() *cobra.Command {
	var teardown bool
	cmd := &cobra.Command{
		Use:   "remove <repo> <subscriptionId>",
		Short: "Remove a Buildkite CI webhook subscription",
		Long: "Remove a Buildkite CI webhook subscription.\n\n" +
			"With --teardown the service also revokes the enrollment identity (OIDC\n" +
			"binding, repo pull grant, and dedicated service account). The Buildkite\n" +
			"pipeline itself is always kept.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := ResolveNativeRepo(ctx, c, args[0])
				if err != nil {
					return err
				}
				err = c.DeleteRepoCIWebhook(ctx, coreapi.DeleteRepoCIWebhookParams{
					RepoId:   repoID,
					ID:       args[1],
					Teardown: coreapi.NewOptBool(teardown),
				})
				if err != nil {
					return err
				}
				msg := "✓ Removed subscription " + args[1]
				if teardown {
					msg += " (enrollment identity torn down)"
				}
				fmt.Fprintln(cmd.OutOrStdout(), msg)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&teardown, "teardown", false, "Also revoke the enrollment identity (OIDC binding, repo grant, service account); the Buildkite pipeline is kept")
	return cmd
}

// activeCoreClient builds the control-plane client for the buildkite verbs. A
// package-level seam (production wiring is coreapi.New) so command-level tests
// can point the command tree at an httptest server without the auth/context/TLS
// stack. Mirrors cli.activeCoreClient; replicated rather than imported because
// the cli package imports this one (root.go calls ci.Register), so importing
// cli back would form an import cycle — the same reason resolve.go replicates
// its control-plane helpers.
var activeCoreClient = func(context.Context) (*coreapi.Client, error) { return coreapi.New() }

// runCore owns the control-plane preamble shared by every buildkite verb:
// silence usage, opt into plain-HTTP token exchange when --insecure-http-auth
// is set, build the client via the activeCoreClient seam, run fn, and map API
// errors to the server's problem-detail message. Mirrors cli.runCore.
func runCore(cmd *cobra.Command, fn func(ctx context.Context, c *coreapi.Client) error) error {
	cmd.SilenceUsage = true
	if insecureHTTPRequested(cmd) {
		auth.EnableInsecureHTTP()
	}
	client, err := activeCoreClient(cmd.Context())
	if err != nil {
		return fmt.Errorf("connect to Entire control plane: %w", err)
	}
	if err := fn(cmd.Context(), client); err != nil {
		return renderCoreError(err)
	}
	return nil
}

// renderCoreError converts a Core API error into the server's problem-detail
// message (so users see the server's reason rather than ogen's decode-wrapped
// string), falling back to the raw error for transport/local failures. Mirrors
// cli.renderCoreError.
func renderCoreError(err error) error {
	if err == nil {
		return nil
	}
	if msg := coreapi.APIError(err); msg != "" {
		return errors.New(msg)
	}
	return err
}

// insecureHTTPRequested reports whether --insecure-http-auth was set on cmd or
// an ancestor (it is a hidden persistent flag on the `ci` parent).
func insecureHTTPRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("insecure-http-auth")
	return err == nil && v
}

// addJSONFlag registers the local --json flag on a verb that renders a wire
// payload. Local, not persistent, so only the read/mutation verbs advertise it.
func addJSONFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("json", false, "Output raw JSON instead of a table")
}

// jsonRequested reports whether --json was set. A lookup error (flag undefined
// on this command) is treated as "not requested".
func jsonRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

// anyChanged reports whether the user set any of the named flags.
func anyChanged(cmd *cobra.Command, names ...string) bool {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// markRequired marks a flag required, panicking on a typo (a wiring-time bug,
// never a runtime one). Mirrors cli.markRequired.
func markRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("mark flag %q required: %v", name, err))
		}
	}
}

// printJSON writes v as indented JSON — the --json view for the buildkite verbs.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

// subscriptionColumns is the human view of one CI-webhook subscription, shared
// by the list table and the single-subscription field view so the two never
// drift. subscriptionValues maps a view to its cells in the same order.
var subscriptionColumns = []string{"ID", "PROVIDER", "BK_ORG", "PIPELINE", "ENABLED", "REF_FILTER", "EVENTS"}

func subscriptionValues(s coreapi.CIWebhookView) []string {
	return []string{
		s.ID,
		s.Provider,
		orDash(s.BkOrganization),
		orDash(s.BkPipeline),
		strconv.FormatBool(s.Enabled),
		orDash(s.RefFilter),
		orDash(strings.Join(s.Events, ",")),
	}
}

// printSubscriptionsTable renders the subscriptions as an aligned table with
// subscriptionColumns as the header. Callers handle the empty case.
func printSubscriptionsTable(w io.Writer, subs []coreapi.CIWebhookView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(subscriptionColumns, "\t"))
	for _, s := range subs {
		fmt.Fprintln(tw, strings.Join(subscriptionValues(s), "\t"))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render table: %w", err)
	}
	return nil
}

// printSubscriptionFields renders one subscription as aligned "FIELD value"
// lines, the single-object counterpart of the list table.
func printSubscriptionFields(w io.Writer, s coreapi.CIWebhookView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	vals := subscriptionValues(s)
	for i, col := range subscriptionColumns {
		fmt.Fprintf(tw, "%s\t%s\n", col, vals[i])
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render fields: %w", err)
	}
	return nil
}

// orDash returns s, or "-" when s is empty/blank, so empty cells read clearly
// in the human table/field views.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
