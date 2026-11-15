package controlplane

import (
	"regexp"
	"strings"
)

var emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// newRedactor returns a function that strips the login secrets, the test
// user's name in any letter case, and email addresses from text bound for
// logs or the public artifact tree.
func newRedactor(username, password, totpSecret, code, approvalURL string) func(string) string {
	secrets := strings.NewReplacer(code, "[device code]", approvalURL, "[approval url]", password, "[password]", totpSecret, "[totp secret]")
	user := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(username))
	return func(s string) string {
		s = secrets.Replace(s)
		// Emails first: the address may contain the username.
		s = emailAddress.ReplaceAllLiteralString(s, "[email]")
		return user.ReplaceAllLiteralString(s, "[username]")
	}
}
