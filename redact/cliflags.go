package redact

import (
	"regexp"
	"slices"
	"strings"
)

// cliFlagValue is one password argument on a command line: a quoted string
// that stays on its line, or a bare word that ends at whitespace, a quote or a
// shell metacharacter. Ending at a backtick or a parenthesis keeps the closing
// delimiter of Markdown code (`mysql -pX`) and of $(...) out of the value.
const cliFlagValue = `"[^"\r\n]*"|'[^'\r\n]*'|[^\s'"` + "`" + `|;&()<>]+`

// cliSameCommand is the text between a command name and one of its flags. It
// never crosses a line break, a shell operator or a backtick (which starts a
// command substitution, or ends Markdown code), so a flag is only read as
// belonging to the command that precedes it in the same pipeline segment.
const cliSameCommand = `[^\r\n|;&` + "`" + `]*?`

// cliLongPasswordPattern matches a GNU-style password flag on any command line
// (mysql --password=..., mongosh --password ..., az login --password ...).
// Group 1 is "=" or the blank that separates the flag from its value, and
// group 2 the value. --password-stdin and --password-file never match: the
// flag must be followed by "=" or a blank.
var cliLongPasswordPattern = regexp.MustCompile(`(?:^|[\s"'])--(?:password|passwd|pass)(=|[ \t]+)(` + cliFlagValue + `)`)

// cliShortPasswordFlags match the short password flags of common clients.
// A short flag means different things to different programs (-p is a port to
// ssh and a profile to aws), so each pattern is anchored to the commands that
// define it. Group 1 is the value. A pattern can only match text that holds
// one of its needles, which spares most strings the regex.
var cliShortPasswordFlags = []struct {
	needles []string
	pattern *regexp.Regexp
}{
	// mysql takes the password glued to -p; a bare -p prompts for it.
	{[]string{"mysql", "mariadb"}, regexp.MustCompile(`\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlimport|mysqlcheck|mysqlpump|mysqlsh)\b` + cliSameCommand + `[ \t]-p(` + cliFlagValue + `)`)},
	{[]string{"login"}, regexp.MustCompile(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm)\b` + cliSameCommand + `\blogin\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`)},
	{[]string{"login"}, regexp.MustCompile(`\baz[ \t]+login\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`)},
	{[]string{"mongo"}, regexp.MustCompile(`\b(?:mongosh|mongo|mongodump|mongorestore|mongoexport|mongoimport|mongostat|mongotop)\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`)},
	// sshpass reads its own options only before the command it wraps, so
	// the -p of sshpass -e ssh -p 2222 is ssh's port.
	{[]string{"sshpass"}, regexp.MustCompile(`\bsshpass(?:[ \t]+-(?:v|P[ \t]*\S+))*[ \t]+-p[ \t]*(` + cliFlagValue + `)`)},
	{[]string{"redis-cli"}, regexp.MustCompile(`\bredis-cli\b` + cliSameCommand + `[ \t]-a[ \t]+(` + cliFlagValue + `)`)},
	{[]string{"pass:"}, regexp.MustCompile(`\bopenssl\b` + cliSameCommand + `[ \t]-pass(?:in|out|word)?[ \t]+pass:(` + cliFlagValue + `)`)},
}

// portMappingPattern matches a container port mapping such as 27017:27017 or
// 127.0.0.1:8080:80/tcp. In docker run --name mongo -p 27017:27017 the -p
// after the container's name is docker's, so such a value is not a password.
var portMappingPattern = regexp.MustCompile(`^(?:[0-9.]+:)?[0-9]+(?:-[0-9]+)?:[0-9]+(?:-[0-9]+)?(?:/(?:tcp|udp|sctp))?$`)

// netrcValue is a .netrc token. A quote or backtick ends it when whitespace or
// a shell metacharacter follows, so the closing quote of
// echo "machine h login me password p" > ~/.netrc stays outside the value.
const netrcValue = `"[^"\r\n]*"|'[^'\r\n]*'|[^\s"'` + "`" + `]+(?:["'` + "`" + `]+[^\s"'` + "`" + `;|&()<>][^\s"'` + "`" + `]*)*`

// netrcPasswordPattern matches the password of a .netrc entry, written on one
// line or across several. The login token is required, so prose such as
// "machine learning password policy" is not an entry, and a default entry must
// start a line or a quoted string, so "by default login with password is" is
// not one either. Group 1 is the machine (absent for default), group 2 the
// login and group 3 the value.
var netrcPasswordPattern = regexp.MustCompile(`(?:\bmachine[ \t\r\n]+(\S+)|(?:^|[\r\n"'])[ \t]*default)[ \t\r\n]+login[ \t\r\n]+(\S+)(?:[ \t\r\n]+account[ \t\r\n]+\S+)?[ \t\r\n]+password[ \t\r\n]+(` + netrcValue + `)`)

// detectCommandLinePasswords returns a region for every password passed to a
// client on its command line, or stored in a .netrc entry. Agents run such
// commands all the time, and a human-chosen password (hunter2, Summer2026!)
// is far below entropyThreshold, so no other layer catches it.
func detectCommandLinePasswords(s string) []taggedRegion {
	var regions []taggedRegion
	// Every flag starts with '-', and a .netrc entry names its login and
	// password.
	if strings.Contains(s, "-") {
		regions = appendLongPasswordFlags(regions, s)
		regions = appendShortPasswordFlags(regions, s)
	}
	if strings.Contains(s, "password") && strings.Contains(s, "login") {
		regions = appendNetrcPasswords(regions, s)
	}
	return regions
}

func appendLongPasswordFlags(regions []taggedRegion, s string) []taggedRegion {
	if !strings.Contains(s, "--pass") {
		return regions
	}
	for _, loc := range cliLongPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
		if r, ok := passwordValueRegion(s, loc[4], loc[5], true, s[loc[2]] != '='); ok {
			regions = append(regions, r)
		}
	}
	return regions
}

func appendShortPasswordFlags(regions []taggedRegion, s string) []taggedRegion {
	for _, flag := range cliShortPasswordFlags {
		if !slices.ContainsFunc(flag.needles, func(n string) bool { return strings.Contains(s, n) }) {
			continue
		}
		for _, loc := range flag.pattern.FindAllStringSubmatchIndex(s, -1) {
			if portMappingPattern.MatchString(s[loc[2]:loc[3]]) {
				continue
			}
			if r, ok := passwordValueRegion(s, loc[2], loc[3], true, false); ok {
				regions = append(regions, r)
			}
		}
	}
	return regions
}

func appendNetrcPasswords(regions []taggedRegion, s string) []taggedRegion {
	for _, loc := range netrcPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
		if isNetrcDeclaration(s, loc) {
			continue
		}
		if r, ok := passwordValueRegion(s, loc[6], loc[7], false, false); ok {
			regions = append(regions, r)
		}
	}
	return regions
}

// passwordValueRegion returns the region of the password at [start,end),
// inside its quotes, unless the value is not a secret. A flag value that
// starts like another flag, a command substitution ($(cat file)) or one of
// the shared placeholders ($PASSWORD, ${PASSWORD}, <password>) stays visible;
// inside single quotes $ is literal. A long flag followed by a blank instead
// of "=" is also common in prose ("pass --password to set it"), so there
// (requireNonWord) the value must contain something other than lowercase
// letters.
func passwordValueRegion(s string, start, end int, flag, requireNonWord bool) (taggedRegion, bool) {
	valueStart, valueEnd, singleQuoted := unquoteRangeQuoted(s, start, end)
	value := s[valueStart:valueEnd]
	switch {
	case value == "", flag && valueStart == start && value[0] == '-':
		return taggedRegion{}, false
	case singleQuoted:
		if !hasNonPlaceholderQuotedPasswordValue(value, true) {
			return taggedRegion{}, false
		}
	case strings.HasPrefix(s[valueStart:], "$("), !hasNonPlaceholderPasswordValue(value):
		return taggedRegion{}, false
	}
	if requireNonWord && isLowercaseWord(value) {
		return taggedRegion{}, false
	}
	return taggedRegion{region: region{valueStart, valueEnd}}, true
}

// isNetrcDeclaration reports whether a netrcPasswordPattern match gives its
// machine, login and password one token, the shape of a declaration (machine
// string, login string, password string in a Go struct) rather than of an
// entry.
func isNetrcDeclaration(s string, loc []int) bool {
	password := s[loc[6]:loc[7]]
	return loc[2] >= 0 && password == s[loc[2]:loc[3]] && password == s[loc[4]:loc[5]]
}

// isLowercaseWord reports whether s is made only of lowercase ASCII letters.
func isLowercaseWord(s string) bool {
	for i := range len(s) {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}
