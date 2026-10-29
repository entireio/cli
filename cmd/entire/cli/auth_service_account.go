package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// newAuthServiceAccountCmd groups the local half of service-account keys: the
// machine generates its own key and keeps the private half; the public half is
// registered on the service account. With ENTIRE_SERVICE_ACCOUNT set, every
// command and git operation then mints its bearer from that key.
func newAuthServiceAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service-account",
		Short: "Authenticate this machine as a service account with a local key",
		Long: "Authenticate this machine as a service account without a long-lived token.\n\n" +
			"`enroll` generates a key pair on this machine and asks for approval: an admin\n" +
			"opens the printed link and confirms the code, with nothing to copy. `key create`\n" +
			"is the manual alternative that prints the public key for registration. Either\n" +
			"way the private key stays in the per-user config directory.\n\n" +
			"Then set " + auth.ServiceAccountEnvVar + " to the service account id: each\n" +
			"command signs a short-lived assertion with the key and exchanges it for a\n" +
			"one-hour session token, re-exchanging before it expires.",
	}
	requireSubcommand(cmd)

	keyCmd := &cobra.Command{
		Use:   "key",
		Short: "Manage this machine's service-account key",
	}
	requireSubcommand(keyCmd)
	keyCmd.AddCommand(newAuthServiceAccountKeyCreateCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyShowCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyRevokeCmd())
	cmd.AddCommand(newAuthServiceAccountEnrollCmd())
	cmd.AddCommand(keyCmd)
	return cmd
}

type serviceAccountKeyOutput struct {
	ServiceAccount string `json:"service_account"`
	Server         string `json:"server"`
	KeyID          string `json:"key_id"`
	PublicKey      string `json:"public_key"`
	KeyFile        string `json:"key_file"`
}

func newAuthServiceAccountKeyCreateCmd() *cobra.Command {
	var (
		serviceAccount string
		server         string
		force          bool
		jsonOut        bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Generate a key pair for a service account on this machine",
		Long: "Generate an Ed25519 key pair for a service account. The private key is\n" +
			"written 0600 to the per-user config directory and never leaves this machine;\n" +
			"the public key is printed for registration on the service account.\n\n" +
			"--server is the regional login server that owns the service account. It\n" +
			"defaults to the active login's server.",
		Example: "  entire auth service-account key create --service-account svc_01HK... --server https://us.auth.entire.io",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			server, err := defaultServiceAccountServer(server)
			if err != nil {
				return err
			}
			key, err := auth.GenerateServiceAccountKey(serviceAccount, server, time.Now())
			if err != nil {
				return err //nolint:wrapcheck // already names the bad input
			}
			name, err := auth.SaveServiceAccountKey(key, force)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			return printServiceAccountKey(cmd.OutOrStdout(), key, name, jsonOut, true)
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().StringVar(&server, "server", "", "Login server that owns the service account (default: the active login's server)")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing key for this service account")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

func newAuthServiceAccountKeyShowCmd() *cobra.Command {
	var (
		serviceAccount string
		jsonOut        bool
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the public key and key id of this machine's service-account key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := auth.LoadServiceAccountKey(serviceAccount)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			id, err := auth.ParseServiceAccountID(serviceAccount)
			if err != nil {
				return err //nolint:wrapcheck // already names the bad input
			}
			return printServiceAccountKey(cmd.OutOrStdout(), key, "service-account-"+id+".json", jsonOut, false)
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

// defaultServiceAccountServer returns flag, or the active login's server when
// the flag is empty. Service accounts are regional, so there is no global
// default.
func defaultServiceAccountServer(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	active, ok, err := auth.ActiveContext()
	if err != nil {
		return "", fmt.Errorf("resolve default login server: %w", err)
	}
	if !ok {
		return "", errors.New("--server is required when no login is active (e.g. --server https://us.auth.entire.io)")
	}
	return active.CoreURL, nil
}

func newAuthServiceAccountEnrollCmd() *cobra.Command {
	var (
		serviceAccount string
		server         string
		name           string
		force          bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Generate a key on this machine and wait for an admin to approve it",
		Long: "Generate an Ed25519 key pair on this machine and request approval for it.\n\n" +
			"The command prints a link and a code. Whoever approves opens the link while\n" +
			"logged in, checks that the code matches, and confirms which service account the\n" +
			"key belongs to (--service-account proposes one; without it the approver picks).\n" +
			"The key is stored only once approved; interrupting the wait leaves nothing\n" +
			"behind, and the request expires on the server.",
		Example: "  entire auth service-account enroll --server https://us.auth.entire.io\n" +
			"  entire auth service-account enroll --server https://us.auth.entire.io --service-account svc_01HK... --name build-box-3",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			ctx := cmd.Context()
			errW := cmd.ErrOrStderr()
			server, err := defaultServiceAccountServer(server)
			if err != nil {
				return err
			}
			if serviceAccount != "" && !force {
				if _, err := auth.LoadServiceAccountKey(serviceAccount); err == nil {
					return fmt.Errorf("a key for %s is already stored on this machine; pass --force to replace it", serviceAccount)
				}
			}
			if name == "" {
				name, _ = os.Hostname() //nolint:errcheck // the label is optional
			}
			key, err := auth.NewEnrollmentKey(server)
			if err != nil {
				return err //nolint:wrapcheck // already names the bad input
			}
			enrollment, err := auth.StartServiceAccountEnrollment(ctx, key, serviceAccount, name)
			if err != nil {
				return err //nolint:wrapcheck // already prefixed
			}
			link := enrollment.VerificationURIComplete
			if link == "" {
				link = enrollment.VerificationURI
			}
			fmt.Fprintf(errW, "To approve this machine, open:\n\n  %s\n\nand check that it shows the code %s (key %s).\n", link, enrollment.UserCode, key.KeyID)
			fmt.Fprintf(errW, "Waiting for approval (expires in %s)...\n", time.Duration(enrollment.ExpiresIn)*time.Second)

			approved, err := auth.WaitForServiceAccountEnrollment(ctx, key, enrollment)
			if err != nil {
				return err //nolint:wrapcheck // sentinel errors are user-facing
			}
			if serviceAccount != "" {
				if want, perr := auth.ParseServiceAccountID(serviceAccount); perr == nil && want != approved {
					fmt.Fprintf(errW, "Note: the approver bound this key to %s, not %s.\n", approved, want)
				}
			}
			if _, err := auth.BindEnrolledKey(key, approved, force); err != nil {
				// The server already trusts this key; do not leave it registered
				// with no copy anywhere.
				if rerr := auth.RevokeServiceAccountKey(ctx, key); rerr != nil {
					return fmt.Errorf("%w; the approved key %s could not be revoked either (%w), ask an admin to revoke it", err, key.KeyID, rerr)
				}
				return fmt.Errorf("%w; the approved key %s was revoked again", err, key.KeyID)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Approved: this machine can act as %s.\n\n  export %s=%s\n  entire auth status\n", approved, auth.ServiceAccountEnvVar, approved)
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account to propose (svc_...); the approver picks one if omitted")
	cmd.Flags().StringVar(&server, "server", "", "Login server that owns the service account (default: the active login's server)")
	cmd.Flags().StringVar(&name, "name", "", "Label shown to the approver (default: this machine's hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing key for the approved service account")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	return cmd
}

func newAuthServiceAccountKeyRevokeCmd() *cobra.Command {
	var (
		serviceAccount string
		localOnly      bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke this machine's key on the server and delete it locally",
		Long: "Revoke this machine's service-account key, authenticated by the key itself, then\n" +
			"delete the key and its cached session. A key can revoke only itself; revoking\n" +
			"other keys of the service account is an admin action.\n\n" +
			"If the server cannot be reached the key is kept so the command can be retried.\n" +
			"--local-only deletes it without revoking: the key stays valid on the server\n" +
			"until an admin revokes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			errW := cmd.ErrOrStderr()
			key, err := auth.LoadServiceAccountKey(serviceAccount)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			if localOnly {
				fmt.Fprintf(errW, "Warning: key %s stays valid on %s until an admin revokes it.\n", key.KeyID, key.Server)
			} else {
				err := auth.RevokeServiceAccountKey(cmd.Context(), key)
				switch {
				case errors.Is(err, auth.ErrServiceAccountKeyUnknown):
					fmt.Fprintf(errW, "%s did not know key %s (already revoked?); deleting it locally.\n", key.Server, key.KeyID)
				case err != nil:
					return fmt.Errorf("%w; the key was kept, retry or pass --local-only", err)
				}
			}
			if err := auth.DeleteServiceAccountKey(key.ServiceAccount); err != nil {
				return err //nolint:wrapcheck // already prefixed
			}
			if localOnly {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted key %s for %s from this machine.\n", key.KeyID, key.ServiceAccount)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Revoked key %s for %s and deleted it from this machine.\n", key.KeyID, key.ServiceAccount)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().BoolVar(&localOnly, "local-only", false, "Delete the key here without revoking it on the server")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

func printServiceAccountKey(w io.Writer, key *auth.ServiceAccountKey, fileName string, jsonOut, created bool) error {
	pub, err := key.PublicKeyPEM()
	if err != nil {
		return err //nolint:wrapcheck // already prefixed
	}
	// Display only: the file was written through the config root.
	dir, err := userdirs.ConfigDirChecked()
	if err != nil {
		return fmt.Errorf("resolve config directory: %w", err)
	}
	keyFile := filepath.Join(dir, fileName)
	if jsonOut {
		return printJSON(w, serviceAccountKeyOutput{
			ServiceAccount: key.ServiceAccount,
			Server:         key.Server,
			KeyID:          key.KeyID,
			PublicKey:      pub,
			KeyFile:        keyFile,
		})
	}
	if created {
		fmt.Fprintf(w, "Created a key for %s on %s.\n", key.ServiceAccount, key.Server)
		fmt.Fprintf(w, "Private key: %s (keep it on this machine)\n\n", keyFile)
	} else {
		fmt.Fprintf(w, "Key for %s on %s\nPrivate key: %s\n\n", key.ServiceAccount, key.Server, keyFile)
	}
	fmt.Fprintf(w, "Key id: %s\n\nRegister this public key on the service account:\n\n%s\n", key.KeyID, pub)
	fmt.Fprintf(w, "Then authenticate as the service account on this machine:\n\n  export %s=%s\n  entire auth status\n", auth.ServiceAccountEnvVar, key.ServiceAccount)
	return nil
}
