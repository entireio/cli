package cli

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

var saBindingColumns = []string{"ID", "PROVIDER", "FILTER"}

// saBinding is a binding plus its provider name.
type saBinding struct {
	coreapi.Binding

	provider string
}

func saBindingRow(b saBinding) []string {
	var filter bytes.Buffer
	if err := json.Compact(&filter, b.AttributeFilter); err != nil {
		return []string{b.ID, b.provider, string(b.AttributeFilter)}
	}
	return []string{b.ID, b.provider, filter.String()}
}

func newSABindingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bindings <service-account>",
		Short: "List a service account's bindings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view := listView[saBinding]{
				table: func([]saBinding) ([]string, func(saBinding) []string) { return saBindingColumns, saBindingRow },
				toJSON: func(items []saBinding) (any, error) {
					wire := make([]coreapi.Binding, 0, len(items))
					for _, b := range items {
						wire = append(wire, b.Binding)
					}
					return wire, nil
				},
			}
			return runCoreListShaped(cmd, "No bindings found.", view, func(ctx context.Context, c *coreapi.Client) ([]saBinding, error) {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				bindings, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Binding, string, error) {
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
				if err != nil || len(bindings) == 0 {
					return nil, err
				}
				return nameBindingProviders(ctx, c, bindings)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// nameBindingProviders swaps provider IDs for names.
func nameBindingProviders(ctx context.Context, c *coreapi.Client, bindings []coreapi.Binding) ([]saBinding, error) {
	providers, err := listSAProviders(ctx, c)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(providers))
	for _, p := range providers {
		names[p.wire.ID] = p.name
	}
	out := make([]saBinding, 0, len(bindings))
	for _, b := range bindings {
		name := names[b.ProviderId]
		if name == "" {
			name = b.ProviderId
		}
		out = append(out, saBinding{Binding: b, provider: name})
	}
	return out, nil
}
