package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "context", Short: "Share authorized context across repositories"}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newContextEnableCmd())
	cmd.AddCommand(newContextDisableCmd())
	cmd.AddCommand(newContextStatusCmd())
	cmd.AddCommand(newContextQueryCmd())
	cmd.AddCommand(newContextInspectCmd())
	return cmd
}

var contextConsentColumns = []string{"ENABLED", "CROSS JURISDICTION", "LOCAL LIVE", "UPDATED"}

func contextConsentRow(c coreapi.ContextSharingConsent) []string {
	updated := "-"
	if value, ok := c.UpdatedAt.Get(); ok {
		updated = value.Format(time.RFC3339)
	}
	return []string{strconv.FormatBool(c.Enabled), strconv.FormatBool(c.AllowCrossJurisdiction), strconv.FormatBool(c.IncludeLocalLive), updated}
}

func newContextEnableCmd() *cobra.Command {
	var cross, localLive bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable cross-repository context sharing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				out, err := c.PutContextSharingConsent(ctx, &coreapi.PutContextSharingConsentInputBody{
					Enabled: true, AllowCrossJurisdiction: cross, IncludeLocalLive: localLive,
				})
				if err != nil {
					return "", nil, contextSharingCommandError(err)
				}
				return "✓ Enabled cross-repository context sharing", out, nil
			})
		},
	}
	cmd.Flags().BoolVar(&cross, "cross-jurisdiction", false, "Allow sources in other jurisdictions when repository owners also allow it")
	cmd.Flags().BoolVar(&localLive, "local-live", true, "Include eligible local live sessions")
	addJSONFlag(cmd)
	return cmd
}

func newContextDisableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable cross-repository context sharing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				out, err := c.PutContextSharingConsent(ctx, &coreapi.PutContextSharingConsentInputBody{})
				if err != nil {
					return "", nil, contextSharingCommandError(err)
				}
				if err := removeLocalContextNamespace(ctx); err != nil {
					return "", nil, fmt.Errorf("remove local context registry: %w", err)
				}
				return "✓ Disabled cross-repository context sharing", out, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newContextStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cross-repository context sharing status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreObject(cmd, contextConsentColumns, contextConsentRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.ContextSharingConsent, error) {
				out, err := c.GetContextSharingConsent(ctx)
				return out, contextSharingCommandError(err)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newContextQueryCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "query <query>",
		Short: "Query authorized context from other repositories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runContextQueryCommand(cmd, args[0], limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 5, "Maximum evidence items")
	addJSONFlag(cmd)
	return cmd
}

func newContextInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <evidence-id>",
		Short: "Inspect a previously returned context evidence item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return inspectContextEvidence(cmd, args[0])
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newOrgContextSharingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "context-sharing", Short: "Manage organization context-sharing policy"}
	cmd.AddCommand(newOrgContextSharingGetCmd())
	cmd.AddCommand(newOrgContextSharingSetCmd())
	return cmd
}

var orgContextPolicyColumns = []string{"CROSS JURISDICTION", "UPDATED"}

func orgContextPolicyRow(p coreapi.OrgContextSharingPolicy) []string {
	updated := "-"
	if value, ok := p.UpdatedAt.Get(); ok {
		updated = value.Format(time.RFC3339)
	}
	return []string{strconv.FormatBool(p.AllowCrossJurisdiction), updated}
}

func newOrgContextSharingGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <org>",
		Short: "Show an organization's context-sharing policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, orgContextPolicyColumns, orgContextPolicyRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.OrgContextSharingPolicy, error) {
				id, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				out, err := c.GetOrgContextSharingPolicy(ctx, coreapi.GetOrgContextSharingPolicyParams{OrgId: id})
				return out, contextSharingCommandError(err)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newOrgContextSharingSetCmd() *cobra.Command {
	var allow bool
	cmd := &cobra.Command{
		Use:   "set <org>",
		Short: "Set an organization's context-sharing policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				id, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				out, err := c.PutOrgContextSharingPolicy(ctx, &coreapi.PutOrgContextSharingPolicyInputBody{AllowCrossJurisdiction: allow}, coreapi.PutOrgContextSharingPolicyParams{OrgId: id})
				if err != nil {
					return "", nil, contextSharingCommandError(err)
				}
				return "✓ Updated organization context-sharing policy", out, nil
			})
		},
	}
	cmd.Flags().BoolVar(&allow, "allow-cross-jurisdiction", false, "Allow repositories owned by this org to cross jurisdiction boundaries")
	addJSONFlag(cmd)
	return cmd
}

// These are implemented in context_retrieval.go and context_registry.go. The
// declarations here keep command wiring compact and make unsupported old-Core
// errors explicit at the command boundary.
var errContextSharingUnsupported = errors.New("context sharing is not supported by this Entire Core; upgrade the server")

func contextSharingCommandError(err error) error {
	if err == nil || !isCoreNotFound(err) {
		return err
	}
	var statusErr *coreapi.ErrorModelStatusCode
	if errors.As(err, &statusErr) {
		detail, _ := statusErr.Response.Detail.Get()
		switch strings.ToLower(strings.TrimSpace(detail)) {
		case "org not found", "target repo not found":
			return err
		}
	}
	return fmt.Errorf("%w: %w", errContextSharingUnsupported, err)
}
