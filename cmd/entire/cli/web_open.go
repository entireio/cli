package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// webFlagName is the uniform flag a view/show command registers to open its
// resource's page in the Entire web app instead of printing it.
const webFlagName = "web"

// webOpenURL opens a URL in the user's browser for --web. A package-level seam
// so tests can observe the URL without launching anything; tests that swap it
// must not run in parallel.
var webOpenURL browserOpenFunc = openBrowser

// addWebFlag registers the standard --web/-w flag on a view/show command.
// Call it after the command's other flags are registered: when the command
// has a --json flag the two are marked mutually exclusive, since --web prints
// a URL rather than the resource.
func addWebFlag(cmd *cobra.Command) {
	cmd.Flags().BoolP(webFlagName, "w", false, "Open in the browser instead of printing")
	if cmd.Flags().Lookup("json") != nil {
		cmd.MarkFlagsMutuallyExclusive(webFlagName, "json")
	}
}

// webRequested reports whether --web was set on cmd or an ancestor. A lookup
// error means the flag isn't defined on this command tree, which is treated
// as "not requested".
func webRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool(webFlagName)
	return err == nil && v
}

// openInWeb serves --web: it prints url on its own line to stdout, then opens
// it in the browser. The printed URL is the result, so a browser that cannot
// be launched (headless host, SSH session) is a stderr note, not a failure.
// An empty url means the resource has no web page to open and is an error.
func openInWeb(cmd *cobra.Command, url string) error {
	if url == "" {
		return errors.New("no web page is available for this resource")
	}
	fmt.Fprintln(cmd.OutOrStdout(), url)
	if err := webOpenURL(cmd.Context(), url); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: could not open a browser; open the URL above manually.")
	}
	return nil
}
