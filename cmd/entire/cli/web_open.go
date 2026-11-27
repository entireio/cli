package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
// been checked: it must be valid UTF-8 with only printable runes (no control
// or Bidi_Control runes), and an http(s) URL on the active login's web origin
// (auth.WebBaseURL). That keeps a response from writing escape sequences or
// reordering text on the terminal, or sending the browser to a foreign host.
// What is printed and opened is the parsed URL re-serialized with its query
// re-encoded, not the input.
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
		logging.Debug(cmd.Context(), "open browser failed", "error", err)
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: could not open a browser; open the URL above manually.")
	}
	return nil
}

// validatedWebURL returns rawURL re-serialized once it is known to be safe to
// print and open; see openInWeb for the rules. Any URL text in an error goes
// through webErrorText, since the input is untrusted.
func validatedWebURL(rawURL string) (string, error) {
	origin, err := webAppOrigin()
	if err != nil {
		return "", err
	}
	shown := webErrorText(rawURL)
	if !utf8.ValidString(rawURL) || strings.ContainsFunc(rawURL, isUnsafeURLRune) {
		return "", fmt.Errorf("refusing to open a URL containing control or non-printable characters: %s", shown)
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
	// Both sides are normalized, so host case and an explicit default port
	// (https://entire.com:443) do not make the same origin compare unequal.
	if api.NormalizeOriginURL(u.Scheme+"://"+u.Host) != origin {
		return "", fmt.Errorf("refusing to open %s: it is not on the Entire web app origin %s (set %s to use another web origin)",
			shown, webErrorText(origin), api.WebBaseURLEnvVar)
	}
	// Re-encode the query so what is printed and opened carries no raw spaces
	// or other characters a terminal or launcher could split on.
	u.RawQuery = u.Query().Encode()
	return u.String(), nil
}

// webAppOrigin returns the normalized origin of the active login's web app,
// refusing a base that is not an http(s) origin before any URL is compared
// against it. The error names where the bad value came from: the
// ENTIRE_WEB_BASE_URL override when it is set, otherwise the login's data host.
func webAppOrigin() (string, error) {
	base, err := auth.WebBaseURL()
	if err != nil {
		return "", fmt.Errorf("resolve the Entire web app URL: %w", err)
	}
	b, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (b.Scheme != schemeHTTPS && b.Scheme != schemeHTTP) || b.Host == "" || b.User != nil {
		shown := webErrorText(base)
		if _, overridden := api.WebBaseURLOverride(); overridden {
			return "", fmt.Errorf("%s is not a valid http(s) origin: %s", api.WebBaseURLEnvVar, shown)
		}
		return "", fmt.Errorf("the Entire web app URL is not a valid http(s) origin: %s (set %s to a valid origin)", shown, api.WebBaseURLEnvVar)
	}
	return api.NormalizeOriginURL(base), nil
}

// webErrorText makes untrusted URL text safe to quote in a one-line error.
// SanitizeDisplayLine (rather than SanitizeTerminalText, which keeps newlines)
// strips escape sequences, controls and bidi controls; the remaining
// non-printable runes it lets through, such as U+200B, are dropped as well so
// the message holds nothing the URL itself was refused for.
func webErrorText(s string) string {
	return strings.Map(func(r rune) rune {
		if isUnsafeURLRune(r) {
			return -1
		}
		return r
	}, tuiutil.SanitizeDisplayLine(strings.ToValidUTF8(s, "")))
}

// isUnsafeURLRune reports runes a printed or opened URL must not contain:
// controls, Bidi_Control format runes (terminal spoofing), and anything else
// that is not printable.
func isUnsafeURLRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || !unicode.IsPrint(r)
}
