package cli

import (
	"os/user"
	"slices"
	"strings"
)

// reservedHostSuffixes mirrors regional.reservedHostSuffixes in entiredb
// (core/regional/reserved_host.go). Both must change together; the server
// re-applies its copy, so drift costs a 400 the CLI renders, never a wrong
// declaration.
var reservedHostSuffixes = []string{"local", "localdomain", "localhost", "internal", "lan", "home", "home.arpa", "test", "example", "invalid"}

// normalizeDeclaredEmail is regional.NormalizeDeclaredEmail's twin: lowercase,
// trim, strip one trailing dot.
func normalizeDeclaredEmail(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// reservedHostEmail reports whether the address's host is one nobody owns —
// the shape git synthesizes when user.email is unset. Syntactic only.
func reservedHostEmail(email string) bool {
	email = normalizeDeclaredEmail(email)
	at := strings.IndexByte(email, '@')
	if at <= 0 || strings.Count(email, "@") != 1 {
		return false
	}
	host := email[at+1:]
	if host == "" {
		return false
	}
	for _, r := range host {
		if r >= 0x80 {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "xn--") {
			return false
		}
	}
	for _, s := range reservedHostSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// normalizeOSUsername lowercases and strips a Windows "DOMAIN\" prefix so it
// compares against git's synthesized local part.
func normalizeOSUsername(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// defaultOSUsername is the production value of unattributedAuthorsDeps.username.
// "" when os/user fails → nothing is offered (spec: no user to match).
func defaultOSUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return normalizeOSUsername(u.Username)
}

// authorsFromShortlog extracts the <email> from each `git shortlog -se` line.
func authorsFromShortlog(out []byte) []string {
	var emails []string
	for _, line := range strings.Split(string(out), "\n") {
		lt, gt := strings.LastIndexByte(line, '<'), strings.LastIndexByte(line, '>')
		if lt < 0 || gt <= lt {
			continue
		}
		if email := line[lt+1 : gt]; email != "" {
			emails = append(emails, email)
		}
	}
	return emails
}

// filterCandidateAuthors is the authorship gate (spec: "Doctor offers only
// addresses whose local part is the current OS username"). It returns the
// normalized, deduplicated reserved-host addresses whose local part equals
// username, in first-seen order. An empty username yields nothing.
func filterCandidateAuthors(authors []string, username string) []string {
	username = normalizeOSUsername(username)
	if username == "" {
		return nil
	}
	var out []string
	for _, a := range authors {
		if !reservedHostEmail(a) {
			continue
		}
		n := normalizeDeclaredEmail(a)
		// reservedHostEmail guarantees exactly one '@' at index > 0.
		if n[:strings.IndexByte(n, '@')] != username {
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
