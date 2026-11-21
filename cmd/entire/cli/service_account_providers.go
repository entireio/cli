package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// githubActionsName is core's GitHub Actions slug.
const githubActionsName = "github-actions"

// saProvider is a provider as the CLI reads it.
type saProvider struct {
	wire      coreapi.OIDCProvider
	name      string
	label     string
	selfServe bool
}

func newSAProvider(p coreapi.OIDCProvider) saProvider {
	// An old core sends the slug as displayName.
	name := p.Name.Or(p.DisplayName.Or("")) //nolint:staticcheck // old cores send only displayName
	label := p.Label.Or(name)
	// An old core sends no self_serve; core decides.
	selfServe := p.SelfServe.Or(true)
	return saProvider{wire: p, name: name, label: label, selfServe: selfServe}
}

// bindable reports whether the CLI sends a bind.
func (p saProvider) bindable() bool { return p.selfServe }

// described reports whether core sent attribute rules.
func (p saProvider) described() bool { return len(p.wire.Attributes) > 0 }

func (p saProvider) requiredNames() []string {
	var out []string
	for _, a := range p.wire.Attributes {
		if a.Required {
			out = append(out, a.Name)
		}
	}
	return out
}

func listSAProviders(ctx context.Context, c *coreapi.Client) ([]saProvider, error) {
	wire, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.OIDCProvider, string, error) {
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
		return nil, err
	}
	out := make([]saProvider, 0, len(wire))
	for _, p := range wire {
		out = append(out, newSAProvider(p))
	}
	return out, nil
}

// findSAProvider picks a bindable provider by name.
func findSAProvider(ctx context.Context, c *coreapi.Client, name string) (saProvider, error) {
	providers, err := listSAProviders(ctx, c)
	if err != nil {
		return saProvider{}, err
	}
	for _, p := range providers {
		if p.name != name {
			continue
		}
		if !p.bindable() {
			return saProvider{}, fmt.Errorf("%s is operator-only", name)
		}
		return p, nil
	}
	return saProvider{}, fmt.Errorf("no provider named %q; run entire sa providers", name)
}

var saProviderColumns = []string{colHeaderName, "LABEL", "REQUIRED PINS"}

func saProviderRow(p saProvider) []string {
	return []string{p.name, p.label, orDash(strings.Join(p.requiredNames(), ", "))}
}

var saAttributeColumns = []string{colHeaderName, "LABEL", "REQUIRED", "EXACT", "PATTERN", "DESCRIPTION"}

func saAttributeRow(a coreapi.OIDCProviderAttribute) []string {
	return []string{a.Name, a.Label, yesNo(a.Required), yesNo(a.Exact), orDash(a.Pattern.Or("")), orDash(a.Description.Or(""))}
}

const (
	cellYes = "yes"
	cellNo  = "no"
)

func yesNo(v bool) string {
	if v {
		return cellYes
	}
	return cellNo
}

func newSAProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers [<name>]",
		Short: "List the identity providers a service account can bind",
		Long: "Lists the OIDC providers you can bind a service account to. " +
			"Name one to see the attributes a binding can pin, which are required, " +
			"and the provider's setup guide. Operator-only providers are not listed.",
		Example: "  entire service-account providers\n" +
			"  entire service-account providers buildkite",
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
					p, err := findSAProvider(ctx, c, args[0])
					if err != nil {
						return err
					}
					return printSAProvider(cmd, p)
				})
			}
			view := listView[saProvider]{
				table: func([]saProvider) ([]string, func(saProvider) []string) { return saProviderColumns, saProviderRow },
				toJSON: func(items []saProvider) (any, error) {
					wire := make([]coreapi.OIDCProvider, 0, len(items))
					for _, p := range items {
						wire = append(wire, p.wire)
					}
					return wire, nil
				},
			}
			return runCoreListShaped(cmd, "No providers found.", view, func(ctx context.Context, c *coreapi.Client) ([]saProvider, error) {
				providers, err := listSAProviders(ctx, c)
				if err != nil {
					return nil, err
				}
				var out []saProvider
				for _, p := range providers {
					if p.bindable() {
						out = append(out, p)
					}
				}
				return out, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// printSAProvider renders one provider's attributes.
func printSAProvider(cmd *cobra.Command, p saProvider) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), &p.wire)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s (%s)\n", p.label, p.name)
	if docs := p.wire.DocsURL.Or(""); docs != "" {
		fmt.Fprintf(out, "Docs: %s\n", docs)
	}
	fmt.Fprintln(out)
	if !p.described() {
		fmt.Fprintln(out, "Core describes no attributes for this provider.")
		return nil
	}
	return printTable(out, saAttributeColumns, p.wire.Attributes, saAttributeRow)
}
