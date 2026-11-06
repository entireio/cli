package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSAUnbindCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unbind <service-account> <binding-id>",
		Short:   "Remove a binding from a service account",
		Long:    "Removes the binding, so its CI jobs can no longer act as the account. Removing a missing binding succeeds.",
		Example: "  entire service-account unbind acme/ci eib_01HZX7QABCDEFGHJKMNPQRSTVW",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				// Core answers 204 for an unknown binding, so a lower-case ID
				// would report success and leave the binding in place.
				bindingID, _ := canonicalPrefixedULID(saBindingIDPrefix, strings.TrimSpace(args[1]))
				if err := c.DeleteBinding(ctx, coreapi.DeleteBindingParams{AccountId: sa.ID, BindingId: bindingID}); err != nil {
					// Core answers 404 for a binding on another account.
					if isCoreNotFound(err) {
						return fmt.Errorf("binding %s is not on %s; run entire sa bindings %s", bindingID, sa.label(), args[0])
					}
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed binding %s from %s\n", bindingID, sa.label())
				return nil
			})
		},
	}
}
