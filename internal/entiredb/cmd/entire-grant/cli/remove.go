package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
)

type resolveGrantResourceIDFunc func(*api.Client, string) (string, error)
type grantDeletePathFunc func(resourceID string, target *cliapi.TargetIdentity) string

type providerIdentityRemoveSpec struct {
	Use               string
	Short             string
	Long              string
	Example           string
	ResolveResourceID resolveGrantResourceIDFunc
	DeletePath        grantDeletePathFunc
}

func newProviderIdentityRemoveCmd(client clientFn, spec providerIdentityRemoveSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:     spec.Use,
		Short:   spec.Short,
		Long:    spec.Long,
		Example: spec.Example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderIdentityDelete(cmd, client, args[0], spec.ResolveResourceID, spec.DeletePath)
		},
	}
	cmd.Flags().String("provider", "github", "identity provider")
	cmd.Flags().String("handle", "", "handle to resolve to provider_user_id")
	cmd.Flags().String("provider-user-id", "", "stable provider user id (skips handle resolve)")
	return cmd
}

func runProviderIdentityDelete(cmd *cobra.Command, client clientFn, resourceRef string, resolveResourceID resolveGrantResourceIDFunc, deletePath grantDeletePathFunc) error {
	provider := clicobra.MustGetString(cmd, "provider")
	handle := clicobra.MustGetString(cmd, "handle")
	providerUserID := clicobra.MustGetString(cmd, "provider-user-id")
	if handle == "" && providerUserID == "" {
		return errors.New("either --handle or --provider-user-id is required")
	}
	c, err := client()
	if err != nil {
		return err
	}
	resourceID, err := resolveResourceID(c, resourceRef)
	if err != nil {
		return err
	}
	target, err := cliapi.ResolveTargetIdentity(c, provider, handle, providerUserID)
	if err != nil {
		return err
	}
	data, err := c.Delete(deletePath(resourceID, target))
	if err != nil {
		return err
	}
	if err := clicobra.PrintJSON(data); err != nil {
		return fmt.Errorf("print response: %w", err)
	}
	return nil
}
