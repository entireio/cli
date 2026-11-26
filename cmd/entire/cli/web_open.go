package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
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

// openInWeb serves --web: it prints the page's URL on its own line to stdout,
// then opens it in the browser. The printed URL is the result, so a browser
// that cannot be launched (headless host, SSH session) is a stderr note, not a
// failure. An empty rawURL means the resource has no web page and is an error.
//
// rawURL may be server-supplied, so nothing is printed or opened until it has
// been checked: it must be a control-character-free http(s) URL on the active
// login's web origin (auth.WebBaseURL). That keeps a response from writing
// escape sequences to the terminal or sending the browser to a foreign host.
// What is printed and opened is the parsed URL re-serialized, not the input.
func openInWeb(cmd *cobra.Command, rawURL string) error {
	if rawURL == "" {
		return errors.New("no web page is available for this resource")
	}
	target, err := validatedWebURL(rawURL)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), target)
	if err := webOpenURL(cmd.Context(), target); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: could not open a browser; open the URL above manually.")
	}
	return nil
}

// validatedWebURL returns rawURL re-serialized once it is known to be safe to
// print and open; see openInWeb for the rules. Any URL text in an error is
// sanitized as a one-line message, since the input is untrusted:
// SanitizeDisplayLine rather than SanitizeTerminalText, which keeps newlines.
func validatedWebURL(rawURL string) (string, error) {
	shown := tuiutil.SanitizeDisplayLine(rawURL)
	if strings.ContainsFunc(rawURL, unicode.IsControl) {
		return "", fmt.Errorf("refusing to open a URL containing control characters: %s", shown)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("refusing to open an invalid URL: %s", shown)
	}
	if u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
		return "", fmt.Errorf("refusing to open a non-HTTP URL: %s", shown)
	}
	// Userinfo is refused with the empty host: https://entire.com@evil.example
	// reads as one origin and dials another, and no web app link carries any.
	if u.Host == "" || u.User != nil {
		return "", fmt.Errorf("refusing to open a URL without a plain host: %s", shown)
	}
	base, err := auth.WebBaseURL()
	if err != nil {
		return "", fmt.Errorf("resolve the Entire web app URL: %w", err)
	}
	origin, err := url.Parse(base)
	if err != nil || origin.Host == "" {
		return "", fmt.Errorf("the Entire web app URL is not a valid origin: %s", tuiutil.SanitizeDisplayLine(base))
	}
	if u.Scheme != origin.Scheme || !strings.EqualFold(u.Host, origin.Host) {
		return "", fmt.Errorf("refusing to open %s: it is not on the Entire web app origin %s (set %s to use another web origin)",
			shown, tuiutil.SanitizeDisplayLine(origin.Scheme+"://"+origin.Host), api.WebBaseURLEnvVar)
	}
	return u.String(), nil
}
