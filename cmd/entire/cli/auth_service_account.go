package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// newAuthServiceAccountCmd groups the local half of service-account keys: the
// machine generates its own key and keeps the private half; the public half is
// registered on the service account. With ENTIRE_SERVICE_ACCOUNT set, every
// command and git operation then mints its bearer from the account's active
// key. Keys are stored one file per key id, and switching keys is explicit, so
// a working key is never replaced by one the server does not know yet.
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
			"command signs a short-lived assertion with the account's active key and\n" +
			"exchanges it for a short-lived session token, re-exchanging before it expires.\n\n" +
			"To rotate, enroll or create a second key for the same account, then run\n" +
			"`key activate`: it checks the new key works, switches to it, and has the old\n" +
			"key revoke itself.",
	}
	requireSubcommand(cmd)

	keyCmd := &cobra.Command{
		Use:   "key",
		Short: "Manage this machine's service-account keys",
	}
	requireSubcommand(keyCmd)
	keyCmd.AddCommand(newAuthServiceAccountKeyCreateCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyActivateCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyListCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyShowCmd())
	keyCmd.AddCommand(newAuthServiceAccountKeyRevokeCmd())
	cmd.AddCommand(newAuthServiceAccountEnrollCmd())
	cmd.AddCommand(keyCmd)
	return cmd
}

type serviceAccountKeyOutput struct {
	ServiceAccount string `json:"service_account,omitempty"`
	Server         string `json:"server"`
	KeyID          string `json:"key_id"`
	Status         string `json:"status"`
	Name           string `json:"name,omitempty"`
	CreatedAt      string `json:"created_at"`
	PublicKey      string `json:"public_key"`
	KeyFile        string `json:"key_file"`
}

func newAuthServiceAccountKeyCreateCmd() *cobra.Command {
	var (
		serviceAccount string
		server         string
		name           string
		jsonOut        bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Generate a key pair for a service account on this machine",
		Long: "Generate an Ed25519 key pair for a service account. The private key is\n" +
			"written 0600 to the per-user config directory and never leaves this machine;\n" +
			"the public key is printed for registration on the service account.\n\n" +
			"If the account has no key on this machine the new key becomes its active key.\n" +
			"Otherwise it is stored inactive: register it, then run `key activate`.\n\n" +
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
			key, err := auth.GenerateServiceAccountKey(serviceAccount, server, defaultKeyName(name), time.Now())
			if err != nil {
				return err //nolint:wrapcheck // already names the bad input
			}
			activated, err := auth.CreateServiceAccountKey(cmd.Context(), key)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			status := auth.KeyInactive
			if activated {
				status = auth.KeyActive
			}
			if jsonOut {
				return printServiceAccountKeyJSON(cmd.OutOrStdout(), auth.StoredServiceAccountKey{Key: key, Status: status})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Created key %s for %s on %s.\n", key.KeyID(), key.ServiceAccount(), key.Server())
			if err := printPublicKey(w, key); err != nil {
				return err
			}
			if activated {
				fmt.Fprintf(w, "Then authenticate as the service account on this machine:\n\n  export %s=%s\n  entire auth status\n", auth.ServiceAccountEnvVar, key.ServiceAccount())
			} else {
				fmt.Fprintf(w, "%s already has an active key here, which keeps working. Once this one is registered, switch to it:\n\n  entire auth service-account key activate --service-account %s --key-id %s\n", key.ServiceAccount(), key.ServiceAccount(), key.KeyID())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().StringVar(&server, "server", "", "Login server that owns the service account (default: the active login's server)")
	cmd.Flags().StringVar(&name, "name", "", "Label for the key (default: this machine's hostname)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

func newAuthServiceAccountKeyActivateCmd() *cobra.Command {
	var (
		serviceAccount string
		keyID          string
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "activate",
		Short: "Switch a service account to another stored key and revoke the old one",
		Long: "Make a stored, registered key the service account's active key.\n\n" +
			"The key is first used for a real token exchange, so nothing changes unless the\n" +
			"server accepts it. Then the account switches to it in one step, and the\n" +
			"previous key revokes itself on the server and is deleted. If that revoke fails\n" +
			"the previous key is kept as retired and the obligation is recorded: running\n" +
			"`key activate` again (with no inactive key left) retries every pending revoke.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			ctx := cmd.Context()
			key, err := selectServiceAccountKey(serviceAccount, keyID, auth.KeyInactive)
			if err != nil && keyID == "" {
				// Nothing to switch to: finish the revokes an earlier
				// activation left behind, if any.
				active, aerr := auth.LoadActiveServiceAccountKey(serviceAccount)
				if aerr != nil {
					return err
				}
				key = active
			} else if err != nil {
				return err
			}
			w, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()
			status, err := auth.ServiceAccountKeyStatusOf(key)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			if status == auth.KeyInactive {
				if err := auth.VerifyServiceAccountKey(ctx, key); err != nil {
					return fmt.Errorf("key %s was not accepted, nothing was changed: %w", key.KeyID(), err)
				}
			}
			retiring, err := auth.ActivateServiceAccountKey(ctx, key)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			if status == auth.KeyInactive {
				fmt.Fprintf(w, "%s now uses key %s.\n", key.ServiceAccount(), key.KeyID())
			} else if len(retiring) == 0 {
				return fmt.Errorf("%s has no inactive key to switch to and no retired key left to revoke", key.ServiceAccount())
			}
			var failed []error
			for _, old := range retiring {
				if err := revokeAndDelete(ctx, errW, w, old); err != nil {
					failed = append(failed, err)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("%w; run `entire auth service-account key activate --service-account %s` again to retry", errors.Join(failed...), key.ServiceAccount())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().StringVar(&keyID, "key-id", "", "Key to activate (default: the account's only inactive key)")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

func newAuthServiceAccountKeyListCmd() *cobra.Command {
	var (
		serviceAccount string
		jsonOut        bool
	)
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List the service-account keys stored on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			keys, err := auth.ListServiceAccountKeys(serviceAccount)
			if err != nil {
				return err //nolint:wrapcheck // already user-facing
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				out := make([]serviceAccountKeyOutput, 0, len(keys))
				for _, k := range keys {
					o, err := serviceAccountKeyJSON(k)
					if err != nil {
						return err
					}
					out = append(out, o)
				}
				return printJSON(w, out)
			}
			if len(keys) == 0 {
				fmt.Fprintln(w, "No service-account keys are stored on this machine.")
				return nil
			}
			for _, k := range keys {
				account := k.Key.ServiceAccount()
				if account == "" {
					account = "(approver chooses)"
				}
				fmt.Fprintf(w, "%-9s  %s  %s  %s  %s\n", k.Status, k.Key.KeyID(), account, k.Key.Server(), k.Key.CreatedAt().Format(time.RFC3339))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Only keys of this service account (svc_...)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func newAuthServiceAccountKeyShowCmd() *cobra.Command {
	var (
		serviceAccount string
		keyID          string
		jsonOut        bool
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the public key of a stored service-account key",
		Long:  "Print the public key of the service account's active key, or of --key-id.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := selectServiceAccountKey(serviceAccount, keyID, auth.KeyActive)
			if err != nil {
				return err
			}
			stored, err := storedStatus(key)
			if err != nil {
				return err
			}
			if jsonOut {
				return printServiceAccountKeyJSON(cmd.OutOrStdout(), stored)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Key %s (%s) for %s on %s\n", key.KeyID(), stored.Status, key.ServiceAccount(), key.Server())
			return printPublicKey(w, key)
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().StringVar(&keyID, "key-id", "", "Key to show (default: the account's active key)")
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

func defaultKeyName(name string) string {
	if name == "" {
		name, _ = os.Hostname() //nolint:errcheck // the label is optional
	}
	return name
}

func newAuthServiceAccountEnrollCmd() *cobra.Command {
	var (
		serviceAccount string
		server         string
		name           string
		keyID          string
		resume         bool
		cancel         bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Generate a key on this machine and wait for an admin to approve it",
		Long: "Generate an Ed25519 key pair on this machine and request approval for it.\n\n" +
			"The command prints a link and a code. Whoever approves opens the link while\n" +
			"logged in, checks that the code matches, and confirms which service account the\n" +
			"key belongs to (--service-account proposes one; without it the approver picks).\n\n" +
			"The key is written to disk before the request is sent, so an interrupted wait\n" +
			"loses nothing: `enroll --resume` keeps waiting, `enroll --cancel` withdraws the\n" +
			"request and deletes the key. An approved key becomes the account's active key\n" +
			"unless the account already has one here; then it is stored inactive for\n" +
			"`key activate`.",
		Example: "  entire auth service-account enroll --server https://us.auth.entire.io\n" +
			"  entire auth service-account enroll --server https://us.auth.entire.io --service-account svc_01HK... --name build-box-3\n" +
			"  entire auth service-account enroll --resume",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			ctx := cmd.Context()
			if resume && cancel {
				return errors.New("--resume and --cancel cannot be combined")
			}
			if resume || cancel {
				key, err := selectEnrollingKey(keyID)
				if err != nil {
					return err
				}
				if cancel {
					return cancelEnrollment(ctx, cmd, key)
				}
				return runEnrollment(ctx, cmd, key)
			}
			if keyID != "" {
				return errors.New("--key-id selects an enrollment for --resume or --cancel")
			}
			server, err := defaultServiceAccountServer(server)
			if err != nil {
				return err
			}
			key, err := auth.NewEnrollmentKey(server, serviceAccount, defaultKeyName(name))
			if err != nil {
				return err //nolint:wrapcheck // already names the bad input
			}
			// Durable before the server sees the public key: whatever happens
			// next, a key the server trusts has its private half on disk. This
			// also refuses a second enrollment while one is in flight.
			if err := auth.SavePendingEnrollmentKey(ctx, key); err != nil {
				if errors.Is(err, auth.ErrEnrollmentInProgress) {
					return fmt.Errorf("%w; run `entire auth service-account enroll --resume` or `--cancel` first", err)
				}
				return err //nolint:wrapcheck // already user-facing
			}
			return runEnrollment(ctx, cmd, key)
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account to propose (svc_...); the approver picks one if omitted")
	cmd.Flags().StringVar(&server, "server", "", "Login server that owns the service account (default: the active login's server)")
	cmd.Flags().StringVar(&name, "name", "", "Label shown to the approver (default: this machine's hostname)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Keep waiting for an enrollment that was interrupted")
	cmd.Flags().BoolVar(&cancel, "cancel", false, "Withdraw a pending enrollment and delete its key")
	cmd.Flags().StringVar(&keyID, "key-id", "", "Pending enrollment to resume or cancel (default: the only one)")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	return cmd
}

// runEnrollment drives key's enrollment from whatever phase it is in:
// (re)submits it, waits for the decision, and binds an approved key. Only a
// definitive outcome deletes the key; anything else keeps it for --resume.
func runEnrollment(ctx context.Context, cmd *cobra.Command, key *auth.ServiceAccountKey) error {
	errW := cmd.ErrOrStderr()
	keep := func(err error) error {
		fmt.Fprintf(errW, "Key %s is kept; continue with `entire auth service-account enroll --resume`, or withdraw it with `--cancel`.\n", key.KeyID())
		return err
	}
	switch key.EnrollmentPhase() {
	case auth.EnrollmentSubmitting:
		enrollment, err := auth.SubmitServiceAccountEnrollment(ctx, key)
		if err != nil {
			if errors.Is(err, auth.ErrEnrollmentRejected) {
				return forgetEnrollment(ctx, key, err)
			}
			return keep(err)
		}
		link := enrollment.VerificationURIComplete
		if link == "" {
			link = enrollment.VerificationURI
		}
		fmt.Fprintf(errW, "To approve this machine, open:\n\n  %s\n\nand check that it shows the code %s (key %s).\n", link, enrollment.UserCode, key.KeyID())
		fmt.Fprintf(errW, "Waiting for approval (expires in %s)...\n", time.Duration(enrollment.ExpiresIn)*time.Second)
	case auth.EnrollmentPending:
		fmt.Fprintf(errW, "Resuming the enrollment of key %s (expires %s).\n", key.KeyID(), key.EnrollmentExpiresAt().Local().Format(time.Kitchen))
	case auth.EnrollmentApproved:
		return bindEnrollment(ctx, cmd, key, key.ServiceAccount())
	default:
		return fmt.Errorf("key %s has no enrollment in progress", key.KeyID())
	}
	approved, err := auth.WaitForServiceAccountEnrollment(ctx, key)
	switch {
	case err == nil:
		return bindEnrollment(ctx, cmd, key, approved)
	case errors.Is(err, auth.ErrEnrollmentDenied) || errors.Is(err, auth.ErrEnrollmentExpired):
		return forgetEnrollment(ctx, key, err)
	default:
		return keep(err)
	}
}

// bindEnrollment records an approval on the key. A failure keeps the key in
// the approved phase, which --resume finishes.
func bindEnrollment(ctx context.Context, cmd *cobra.Command, key *auth.ServiceAccountKey, approved string) error {
	errW := cmd.ErrOrStderr()
	if proposed := key.ServiceAccount(); proposed != "" && proposed != approved {
		fmt.Fprintf(errW, "Note: the approver bound this key to %s, not %s.\n", approved, proposed)
	}
	activated, err := auth.BindEnrolledKey(ctx, key, approved)
	if err != nil {
		return fmt.Errorf("%w; the approved key %s is still on disk, finish with `entire auth service-account enroll --resume`", err, key.KeyID())
	}
	w := cmd.OutOrStdout()
	if !activated {
		fmt.Fprintf(w, "Approved key %s for %s. The account already has an active key here, which keeps working; switch with:\n\n  entire auth service-account key activate --service-account %s --key-id %s\n", key.KeyID(), approved, approved, key.KeyID())
		return nil
	}
	fmt.Fprintf(w, "Approved: this machine can act as %s.\n\n  export %s=%s\n  entire auth status\n", approved, auth.ServiceAccountEnvVar, approved)
	return nil
}

// cancelEnrollment withdraws key's enrollment and deletes the key only when
// the server confirms it can no longer be approved. If the approval won the
// race, the key is kept and bound.
func cancelEnrollment(ctx context.Context, cmd *cobra.Command, key *auth.ServiceAccountKey) error {
	approved, err := auth.CancelServiceAccountEnrollment(ctx, key)
	switch {
	case errors.Is(err, auth.ErrEnrollmentAlreadyApproved):
		fmt.Fprintf(cmd.ErrOrStderr(), "The enrollment of key %s was approved before it could be cancelled; keeping the key. Remove it with `entire auth service-account key revoke --service-account %s --key-id %s`.\n", key.KeyID(), approved, key.KeyID())
		return bindEnrollment(ctx, cmd, key, approved)
	case auth.IsDefinitiveEnrollmentOutcome(err):
		if ferr := auth.ForgetEnrollment(ctx, key, err); ferr != nil {
			return ferr //nolint:wrapcheck // already user-facing
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Withdrew the enrollment of key %s (%v) and deleted the key.\n", key.KeyID(), err)
		return nil
	default:
		return fmt.Errorf("%w; key %s was kept, retry `enroll --cancel` later", err, key.KeyID())
	}
}

// forgetEnrollment deletes a key whose enrollment definitively ended and
// returns that outcome.
func forgetEnrollment(ctx context.Context, key *auth.ServiceAccountKey, outcome error) error {
	if err := auth.ForgetEnrollment(context.WithoutCancel(ctx), key, outcome); err != nil {
		return fmt.Errorf("%w; also could not delete key %s: %w", outcome, key.KeyID(), err)
	}
	return outcome
}

func newAuthServiceAccountKeyRevokeCmd() *cobra.Command {
	var (
		serviceAccount string
		keyID          string
		localOnly      bool
		insecure       bool
	)
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a key of this machine on the server and delete it locally",
		Long: "Revoke one of this machine's service-account keys (the active one unless\n" +
			"--key-id names another), authenticated by the key itself, then delete the key\n" +
			"and its cached session. A key can revoke only itself; revoking other keys of the\n" +
			"service account is an admin action.\n\n" +
			"If the server cannot be reached the key is kept so the command can be retried.\n" +
			"--local-only deletes it without revoking: the key stays valid on the server\n" +
			"until an admin revokes it. A key whose enrollment is still undecided cannot be\n" +
			"revoked or deleted; withdraw it with `enroll --cancel`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applyInsecureHTTPAuth(insecure)
			key, err := selectServiceAccountKey(serviceAccount, keyID, auth.KeyActive)
			if err != nil {
				return err
			}
			errW := cmd.ErrOrStderr()
			if !localOnly {
				return revokeAndDelete(cmd.Context(), errW, cmd.OutOrStdout(), key)
			}
			fmt.Fprintf(errW, "Warning: key %s stays valid on %s until an admin revokes it.\n", key.KeyID(), key.Server())
			if err := auth.DeleteServiceAccountKey(cmd.Context(), key); err != nil {
				return err //nolint:wrapcheck // already prefixed
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted key %s for %s from this machine.\n", key.KeyID(), key.ServiceAccount())
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Service account id (svc_...)")
	cmd.Flags().StringVar(&keyID, "key-id", "", "Key to revoke (default: the account's active key)")
	cmd.Flags().BoolVar(&localOnly, "local-only", false, "Delete the key here without revoking it on the server")
	addInsecureHTTPAuthFlag(cmd, &insecure)
	_ = cmd.MarkFlagRequired("service-account") //nolint:errcheck // flag registered above
	return cmd
}

// revokeAndDelete has key revoke itself, then deletes it locally. A failed
// revoke keeps the key on disk so it can retry.
func revokeAndDelete(ctx context.Context, errW, w io.Writer, key *auth.ServiceAccountKey) error {
	err := auth.RevokeServiceAccountKey(ctx, key)
	switch {
	case errors.Is(err, auth.ErrServiceAccountKeyUnknown):
		fmt.Fprintf(errW, "%s did not know key %s (already revoked?); deleting it locally.\n", key.Server(), key.KeyID())
	case err != nil:
		return fmt.Errorf("%w; key %s was kept, retry with `entire auth service-account key revoke --service-account %s --key-id %s` or pass --local-only", err, key.KeyID(), key.ServiceAccount(), key.KeyID())
	}
	if err := auth.DeleteServiceAccountKey(ctx, key); err != nil {
		return fmt.Errorf("key %s was revoked but not deleted: %w", key.KeyID(), err)
	}
	fmt.Fprintf(w, "Revoked key %s for %s and deleted it from this machine.\n", key.KeyID(), key.ServiceAccount())
	return nil
}

// selectServiceAccountKey resolves --key-id, or the account's default key:
// the active one for want=KeyActive, the only inactive one for KeyInactive.
func selectServiceAccountKey(serviceAccount, keyID string, want auth.ServiceAccountKeyStatus) (*auth.ServiceAccountKey, error) {
	id, err := auth.ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err //nolint:wrapcheck // already names the bad input
	}
	if keyID != "" {
		key, err := auth.LoadServiceAccountKey(keyID)
		if err != nil {
			return nil, err //nolint:wrapcheck // already user-facing
		}
		if key.ServiceAccount() != id {
			return nil, fmt.Errorf("key %s does not belong to %s", keyID, id)
		}
		return key, nil
	}
	if want == auth.KeyActive {
		return auth.LoadActiveServiceAccountKey(id) //nolint:wrapcheck // already user-facing
	}
	keys, err := auth.ListServiceAccountKeys(id)
	if err != nil {
		return nil, err //nolint:wrapcheck // already user-facing
	}
	var ids []string
	var match *auth.ServiceAccountKey
	for _, k := range keys {
		if k.Status == want {
			ids = append(ids, k.Key.KeyID())
			match = k.Key
		}
	}
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("%s has no %s key on this machine", id, want)
	case 1:
		return match, nil
	default:
		return nil, fmt.Errorf("%s has %d %s keys (%s); pick one with --key-id", id, len(ids), want, strings.Join(ids, ", "))
	}
}

// selectEnrollingKey resolves the pending enrollment to resume or cancel.
func selectEnrollingKey(keyID string) (*auth.ServiceAccountKey, error) {
	keys, err := auth.ListServiceAccountKeys("")
	if err != nil {
		return nil, err //nolint:wrapcheck // already user-facing
	}
	var found []*auth.ServiceAccountKey
	for _, k := range keys {
		if k.Status == auth.KeyEnrolling && (keyID == "" || k.Key.KeyID() == keyID) {
			found = append(found, k.Key)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) == 0 && keyID != "":
		return nil, fmt.Errorf("no pending enrollment for key %s on this machine", keyID)
	case len(found) == 0:
		return nil, errors.New("no enrollment is pending on this machine")
	default:
		return nil, errors.New("several enrollments are pending; pick one with --key-id (see `entire auth service-account key list`)")
	}
}

func storedStatus(key *auth.ServiceAccountKey) (auth.StoredServiceAccountKey, error) {
	keys, err := auth.ListServiceAccountKeys(key.ServiceAccount())
	if err != nil {
		return auth.StoredServiceAccountKey{}, err //nolint:wrapcheck // already user-facing
	}
	for _, k := range keys {
		if k.Key.KeyID() == key.KeyID() {
			return k, nil
		}
	}
	return auth.StoredServiceAccountKey{}, fmt.Errorf("key %s is no longer stored", key.KeyID())
}

func serviceAccountKeyJSON(k auth.StoredServiceAccountKey) (serviceAccountKeyOutput, error) {
	pub, err := k.Key.PublicKeyPEM()
	if err != nil {
		return serviceAccountKeyOutput{}, err //nolint:wrapcheck // already prefixed
	}
	// Display only: the file is read and written through the config root.
	dir, err := userdirs.ConfigDirChecked()
	if err != nil {
		return serviceAccountKeyOutput{}, fmt.Errorf("resolve config directory: %w", err)
	}
	return serviceAccountKeyOutput{
		ServiceAccount: k.Key.ServiceAccount(),
		Server:         k.Key.Server(),
		KeyID:          k.Key.KeyID(),
		Status:         string(k.Status),
		Name:           k.Key.Name(),
		CreatedAt:      k.Key.CreatedAt().Format(time.RFC3339),
		PublicKey:      pub,
		KeyFile:        filepath.Join(dir, auth.ServiceAccountKeyFileName(k.Key.KeyID())),
	}, nil
}

func printServiceAccountKeyJSON(w io.Writer, k auth.StoredServiceAccountKey) error {
	out, err := serviceAccountKeyJSON(k)
	if err != nil {
		return err
	}
	return printJSON(w, out)
}

func printPublicKey(w io.Writer, key *auth.ServiceAccountKey) error {
	pub, err := key.PublicKeyPEM()
	if err != nil {
		return err //nolint:wrapcheck // already prefixed
	}
	fmt.Fprintf(w, "Key id: %s\n\nRegister this public key on the service account:\n\n%s\n", key.KeyID(), pub)
	return nil
}
