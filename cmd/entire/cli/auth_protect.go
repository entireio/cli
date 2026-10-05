package cli

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore/senclave"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/spf13/cobra"
)

// newAuthProtectCmd seals saved logins to the Secure Enclave.
func newAuthProtectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "protect",
		Short: "Require Touch ID to use saved logins (macOS)",
		Long: "Require Touch ID to use saved logins (macOS).\n\n" +
			"Creates a key in this Mac's Secure Enclave and seals every saved login\n" +
			"token to it. From then on each use of a login, whether by `entire`\n" +
			"commands or by `git push`/`git fetch` on entire:// remotes, shows the\n" +
			"macOS Touch ID (or password) dialog naming the action. An agent or\n" +
			"script cannot use your login without you approving that dialog.\n\n" +
			"Logins and token refreshes stay silent; only reads prompt. The key\n" +
			"blob lives at " + auth.ProtectedKeyFile + " in the Entire config\n" +
			"directory and is useless on any other machine. Run\n" +
			"`entire auth unprotect` to return to plain keychain storage.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				return errors.New("token protection needs macOS with a Secure Enclave")
			}
			sealed, created, err := auth.EnableProtection(userdirs.Config())
			out := cmd.OutOrStdout()
			// Report what already happened even on failure: a key may exist
			// and some logins may already need Touch ID.
			if created {
				fmt.Fprintln(out, "Created Secure Enclave token key.")
			}
			if err != nil {
				for _, name := range sealed {
					fmt.Fprintf(out, "Sealed login %q before the failure.\n", name)
				}
				if errors.Is(err, senclave.ErrUnsupported) {
					return fmt.Errorf("this Mac cannot use the Secure Enclave: %w", err)
				}
				return fmt.Errorf("%w; run `entire auth protect` again to finish", err)
			}
			switch len(sealed) {
			case 0:
				fmt.Fprintln(out, "No plaintext logins to seal. New logins will be sealed.")
			default:
				for _, name := range sealed {
					fmt.Fprintf(out, "Sealed login %q.\n", name)
				}
				fmt.Fprintln(out, "Touch ID is now required to use these logins.")
			}
			return nil
		},
	}
}

// newAuthUnprotectCmd returns saved logins to plain storage.
func newAuthUnprotectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unprotect",
		Short: "Stop requiring Touch ID for saved logins",
		Long: "Stop requiring Touch ID for saved logins.\n\n" +
			"Unseals every saved login back to plain keychain storage, asking for\n" +
			"Touch ID once per login, then removes the Secure Enclave key.\n\n" +
			"A key that cannot be loaded here (a config directory copied from\n" +
			"another machine, or a damaged key file) is removed without unsealing;\n" +
			"logins sealed under it need `entire login` again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			unsealed, dropped, err := auth.DisableProtection(userdirs.Config())
			if err != nil {
				if errors.Is(err, auth.ErrProtectionOff) {
					fmt.Fprintln(cmd.OutOrStdout(), "Token protection is not on.")
					return nil
				}
				return err //nolint:wrapcheck // DisableProtection errors already name the step
			}
			out := cmd.OutOrStdout()
			for _, name := range unsealed {
				fmt.Fprintf(out, "Unsealed login %q.\n", name)
			}
			if dropped != nil {
				fmt.Fprintln(out, "Removed a Secure Enclave key that cannot be loaded here.")
			}
			for _, name := range dropped {
				fmt.Fprintf(out, "Login %q was sealed under it; run `entire login`.\n", name)
			}
			fmt.Fprintln(out, "Token protection is off.")
			return nil
		},
	}
}
