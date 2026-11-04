package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// githubActionsIssuer identifies core's GitHub Actions provider.
const githubActionsIssuer = "https://token.actions.githubusercontent.com"

// githubRepositoryAttr is the provider's repository pin.
const githubRepositoryAttr = "repository"

func newSABindCmd() *cobra.Command {
	var repository string
	var attrs []string
	cmd := &cobra.Command{
		Use:   "bind <service-account>",
		Short: "Let a GitHub repository's Actions act as a service account",
		Long: "Binds GitHub Actions tokens from one repository to the service account. " +
			"--github-repository takes one exact owner/repo; core refuses globs. " +
			"--attribute adds a pin, such as ref=refs/heads/main.\n\n" +
			"A service account holds one GitHub Actions binding. " +
			"The binding matches the repository name. GitHub can give a renamed or " +
			"deleted repository's name to a new one, so unbind before you rename or delete it.",
		Example: "  entire service-account bind acme/ci --github-repository acme/web\n" +
			"  entire service-account bind acme/ci --github-repository acme/web --attribute ref=refs/heads/main",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			filter, err := githubBindingFilter(repository, attrs)
			if err != nil {
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				providerID, err := githubActionsProviderID(ctx, c)
				if err != nil {
					return err
				}
				b, err := c.CreateBinding(ctx, &coreapi.CreateBindingInputBody{ProviderId: providerID, AttributeFilter: filter},
					coreapi.CreateBindingParams{AccountId: sa.ID})
				if err != nil {
					// The route's only 409 is an existing binding.
					if coreapi.APIErrorStatus(err) == http.StatusConflict {
						return fmt.Errorf("%s already has a GitHub Actions binding; unbind it first", sa.label())
					}
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), b)
				}
				printBindNext(cmd, c.CoreOrigin(), sa, repository, b.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&repository, "github-repository", "", "GitHub repository whose workflows act as the account (owner/repo)")
	cmd.Flags().StringArrayVar(&attrs, "attribute", nil, "Extra token pin as name=value, e.g. ref=refs/heads/main (repeatable)")
	markRequired(cmd, "github-repository")
	addJSONFlag(cmd)
	return cmd
}

// printBindNext names the token exchange inputs.
func printBindNext(cmd *cobra.Command, origin string, sa saRef, repository, bindingID string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Bound %s to GitHub repository %s (%s)\n", sa.label(), repository, bindingID)
	fmt.Fprintln(out, "In the workflow, exchange its OIDC token for this account:")
	fmt.Fprintf(out, "  audience  %s/api/authz/sts\n", origin)
	fmt.Fprintf(out, "  endpoint  %s/oauth/token\n", origin)
	fmt.Fprintf(out, "  resource  %s\n", sa.ID)
}

// githubBindingFilter builds the attribute filter JSON.
func githubBindingFilter(repository string, attrs []string) ([]byte, error) {
	repository = strings.TrimSpace(repository)
	// Core's rule: one exact owner/repo.
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(owner+name, "/*") {
		return nil, fmt.Errorf("--github-repository %q must be one exact owner/repo", repository)
	}
	filter := map[string]string{githubRepositoryAttr: repository}
	for _, a := range attrs {
		key, value, ok := strings.Cut(a, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case !ok || key == "" || value == "":
			return nil, fmt.Errorf("--attribute %q must be name=value", a)
		case key == githubRepositoryAttr:
			return nil, errors.New("set the repository with --github-repository")
		case filter[key] != "":
			return nil, fmt.Errorf("--attribute %s is given twice", key)
		}
		filter[key] = value
	}
	raw, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("encode attribute filter: %w", err)
	}
	return raw, nil
}

// githubActionsProviderID finds the provider by issuer.
func githubActionsProviderID(ctx context.Context, c *coreapi.Client) (string, error) {
	providers, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.OIDCProvider, string, error) {
		params := coreapi.ListOIDCProvidersParams{}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListOIDCProviders(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Providers, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return "", err
	}
	for _, p := range providers {
		if p.Issuer == githubActionsIssuer {
			return p.ID, nil
		}
	}
	return "", errors.New("this server has no GitHub Actions provider")
}

var saBindingColumns = []string{"ID", "PROVIDER", "FILTER"}

func saBindingRow(b coreapi.Binding) []string {
	var filter bytes.Buffer
	if err := json.Compact(&filter, b.AttributeFilter); err != nil {
		return []string{b.ID, b.ProviderId, string(b.AttributeFilter)}
	}
	return []string{b.ID, b.ProviderId, filter.String()}
}

func newSABindingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bindings <service-account>",
		Short: "List a service account's bindings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No bindings found.", saBindingColumns, saBindingRow,
				func(ctx context.Context, c *coreapi.Client) ([]coreapi.Binding, error) {
					sa, err := resolveSARef(ctx, c, args[0])
					if err != nil {
						return nil, err
					}
					return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Binding, string, error) {
						params := coreapi.ListBindingsParams{AccountId: sa.ID}
						if cursor != "" {
							params.PageToken = coreapi.NewOptString(cursor)
						}
						out, err := c.ListBindings(ctx, params)
						if err != nil {
							return nil, "", err
						}
						return out.Bindings, out.NextPageToken.Or(""), nil
					})
				})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newSAUnbindCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unbind <service-account> <binding-id>",
		Short:   "Remove a binding from a service account",
		Long:    "Removes the binding, so its workflows can no longer act as the account. Removing a missing binding succeeds.",
		Example: "  entire service-account unbind acme/ci eib_01HZX7QABCDEFGHJKMNPQRSTVW",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				if err := c.DeleteBinding(ctx, coreapi.DeleteBindingParams{AccountId: sa.ID, BindingId: args[1]}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed binding %s from %s\n", args[1], sa.label())
				return nil
			})
		},
	}
}
