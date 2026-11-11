package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	saSecretIDPrefix = "sas_"
	// saSecretMaxDays is core's longest secret lifetime.
	saSecretMaxDays = 365
	// saSecretMaxUserAgentRunes bounds the LAST USED client in the table.
	saSecretMaxUserAgentRunes = 80

	// Core's published problem type; never renamed.
	saProblemSecretLimit = "https://entire.com/errors/service-account/secret-limit-reached" //nolint:gosec // G101: a problem type URI, not a credential
)

// newSASecretCmd is the `entire service-account secret` group.
func newSASecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage a service account's secrets",
		Long: "A secret lets a script or agent without a CI provider act as the " +
			"service account: set it as ENTIRE_TOKEN and the CLI, git-remote-entire " +
			"and the Go SDK exchange it for short-lived tokens.\n\n" +
			"Only a person with manage access to the project creates one. A service " +
			"account holds at most two unexpired secrets, so you can roll out a new " +
			"one before deleting the old.",
	}
	cmd.AddCommand(newSASecretCreateCmd(), newSASecretListCmd(), newSASecretDeleteCmd())
	return requireSubcommand(cmd)
}

func newSASecretCreateCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "create <service-account>",
		Short: "Create a secret for a service account",
		Long: "Creates a secret and prints it once. Only the secret goes to stdout, " +
			"so you can redirect it to a file or a secret store; notes go to stderr. " +
			"A secret lasts 90 days unless --expires-in-days sets 1 to 365.",
		Example: "  entire service-account secret create acme/agent > agent.secret\n" +
			"  entire service-account secret create acme/agent --expires-in-days 30",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// An explicit 0 is a mistake, not a request for the default.
			if cmd.Flags().Changed("expires-in-days") && (days < 1 || days > saSecretMaxDays) {
				cmd.SilenceUsage = true
				return fmt.Errorf("--expires-in-days must be between 1 and %d", saSecretMaxDays)
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				var body coreapi.OptCreateServiceAccountSecretBody
				if days > 0 {
					body = coreapi.NewOptCreateServiceAccountSecretBody(coreapi.CreateServiceAccountSecretBody{
						ExpiresInDays: coreapi.NewOptInt64(int64(days)),
					})
				}
				out, err := c.CreateServiceAccountSecret(ctx, body, coreapi.CreateServiceAccountSecretParams{AccountId: sa.ID})
				if err != nil {
					return saSecretCreateErr(err, sa, args[0])
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), out)
				}
				notes := cmd.ErrOrStderr()
				fmt.Fprintf(notes, "✓ Created secret %s for %s, expires %s\n", out.ID, sa.label(), saSecretTime(out.ExpiresAt))
				fmt.Fprintln(notes, "Store it now; it is not shown again. Use it as ENTIRE_TOKEN.")
				fmt.Fprintln(cmd.OutOrStdout(), out.Secret)
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&days, "expires-in-days", 0, "lifetime in days, 1 to 365 (default 90)")
	addJSONFlag(cmd)
	return cmd
}

// saSecretCreateErr names the account in core's refusals.
func saSecretCreateErr(err error, sa saRef, ref string) error {
	if coreapi.APIErrorType(err) == saProblemSecretLimit {
		return fmt.Errorf("%s already holds two unexpired secrets; delete one first (entire service-account secret list %s)", sa.label(), ref)
	}
	if coreapi.APIErrorStatus(err) == http.StatusForbidden {
		// Core refuses a missing manage grant, a service account caller and a
		// delegated token alike; its detail says which.
		return fmt.Errorf("cannot create a secret for %s: %s", sa.label(), coreapi.APIError(err))
	}
	return err
}

var saSecretColumns = []string{"ID", colHeaderStatus, colHeaderCreated, "EXPIRES", "LAST USED", "USER AGENT"}

// saSecretRowAt renders a secret. Core also lists secrets that expired in the
// last 30 days, so STATUS says which still work.
func saSecretRowAt(now time.Time) func(coreapi.ServiceAccountSecret) []string {
	return func(s coreapi.ServiceAccountSecret) []string {
		status := "active"
		if !s.ExpiresAt.After(now) {
			status = "expired"
		}
		lastUsed := "never"
		if t, ok := s.LastUsedAt.Get(); ok {
			lastUsed = saSecretTime(t)
		}
		// The User-Agent is whatever the client that used the secret sent.
		userAgent := terminalText(s.LastUsedUserAgent.Or(""), saSecretMaxUserAgentRunes)
		return []string{s.ID, status, saSecretTime(s.CreatedAt), saSecretTime(s.ExpiresAt), lastUsed, orDash(userAgent)}
	}
}

func saSecretTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04")
}

func newSASecretListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <service-account>",
		Short: "List a service account's secrets",
		Long: "Lists the account's unexpired secrets and those that expired in the " +
			"last 30 days, with when and from which client each was last used. " +
			"Secrets themselves are never shown again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No secrets found.", saSecretColumns, saSecretRowAt(time.Now()),
				func(ctx context.Context, c *coreapi.Client) ([]coreapi.ServiceAccountSecret, error) {
					sa, err := resolveSARef(ctx, c, args[0])
					if err != nil {
						return nil, err
					}
					return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.ServiceAccountSecret, string, error) {
						params := coreapi.ListServiceAccountSecretsParams{AccountId: sa.ID}
						if cursor != "" {
							params.Cursor = coreapi.NewOptString(cursor)
						}
						out, err := c.ListServiceAccountSecrets(ctx, params)
						if err != nil {
							return nil, "", err
						}
						return out.Secrets, out.NextCursor.Or(""), nil
					})
				})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newSASecretDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <service-account> <secret-id>",
		Short: "Delete a service account secret",
		Long: "Deletes the secret, so it can no longer be exchanged. Tokens already " +
			"exchanged from it keep working until they expire, at most 10 minutes. " +
			"Deleting a missing secret succeeds.",
		Example: "  entire service-account secret delete acme/agent sas_01HZX7QABCDEFGHJKMNPQRSTVW",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Core answers 204 for an unknown secret, so a lower-case or
			// mistyped ID would report success and leave the secret working.
			secretID, ok := canonicalPrefixedULID(saSecretIDPrefix, strings.TrimSpace(args[1]))
			if !ok {
				cmd.SilenceUsage = true
				return errors.New("secret ID must look like sas_<id>; run entire service-account secret list " + args[0])
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				if err := c.DeleteServiceAccountSecret(ctx, coreapi.DeleteServiceAccountSecretParams{AccountId: sa.ID, SecretID: secretID}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted secret %s from %s\n", secretID, sa.label())
				return nil
			})
		},
	}
}
