package redact

import (
	"regexp"
	"strings"
)

// cliFlagValue is one password argument on a command line: a quoted string
// that stays on its line, or a bare word that ends at whitespace or a shell
// operator.
const cliFlagValue = `"[^"\r\n]*"|'[^'\r\n]*'|[^\s'"|;&]+`

// cliSameCommand is the text between a command name and one of its flags. It
// never crosses a line break or a shell operator, so a flag is only read as
// belonging to the command that precedes it in the same pipeline segment.
const cliSameCommand = `[^\r\n|;&]*?`

// cliLongPasswordPattern matches a GNU-style password flag on any command line
// (mysql --password=..., mongosh --password ..., az login --password ...).
// Group 1 is "=" or the blank that separates the flag from its value, and
// group 2 the value. --password-stdin and --password-file never match: the
// flag must be followed by "=" or a blank.
var cliLongPasswordPattern = regexp.MustCompile(`(?:^|[\s"'])--(?:password|passwd|pass)(=|[ \t]+)(` + cliFlagValue + `)`)

// cliShortPasswordPatterns match the short password flags of common clients.
// A short flag means different things to different programs (-p is a port to
// ssh and a profile to aws), so each pattern is anchored to the commands that
// define it. Group 1 is the value.
var cliShortPasswordPatterns = []*regexp.Regexp{
	// mysql takes the password glued to -p; a bare -p prompts for it.
	regexp.MustCompile(`\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlimport|mysqlcheck|mysqlpump|mysqlsh)\b` + cliSameCommand + `[ \t]-p(` + cliFlagValue + `)`),
	regexp.MustCompile(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm)\b` + cliSameCommand + `\blogin\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`),
	regexp.MustCompile(`\baz[ \t]+login\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`),
	regexp.MustCompile(`\b(?:mongosh|mongo|mongodump|mongorestore|mongoexport|mongoimport|mongostat|mongotop)\b` + cliSameCommand + `[ \t]-p[ \t]+(` + cliFlagValue + `)`),
	regexp.MustCompile(`\bsshpass\b` + cliSameCommand + `[ \t]-p[ \t]*(` + cliFlagValue + `)`),
	regexp.MustCompile(`\bredis-cli\b` + cliSameCommand + `[ \t]-a[ \t]+(` + cliFlagValue + `)`),
	regexp.MustCompile(`\bopenssl\b` + cliSameCommand + `[ \t]-pass(?:in|out)?[ \t]+pass:(` + cliFlagValue + `)`),
}

// netrcPasswordPattern matches the password of a .netrc entry, written on one
// line or across several. The login token is required, so prose such as
// "machine learning password policy" is not an entry. Group 1 is the value.
var netrcPasswordPattern = regexp.MustCompile(`\b(?:machine[ \t\r\n]+\S+|default)[ \t\r\n]+login[ \t\r\n]+\S+(?:[ \t\r\n]+account[ \t\r\n]+\S+)?[ \t\r\n]+password[ \t\r\n]+(\S+)`)

// detectCommandLinePasswords returns a region for every password passed to a
// client on its command line, or stored in a .netrc entry. Agents run such
// commands all the time, and a human-chosen password (hunter2, Summer2026!)
// is far below entropyThreshold, so no other layer catches it.
//
// A value that starts like another flag or a shell expansion ($PASSWORD,
// $(cat file)) is a reference, not a secret, and stays visible, as do the
// shared placeholders. A long flag followed by a blank instead of "=" is also
// common in prose ("pass --password to set it"), so there the value must
// contain something other than lowercase letters.
func detectCommandLinePasswords(s string) []taggedRegion {
	// Every flag starts with '-', and a .netrc entry names its password.
	hasFlag, hasNetrc := strings.Contains(s, "-"), strings.Contains(s, "password")
	if !hasFlag && !hasNetrc {
		return nil
	}
	var regions []taggedRegion
	add := func(start, end int, requireNonWord bool) {
		start, end, _ = unquoteRangeQuoted(s, start, end)
		value := s[start:end]
		if value == "" || strings.ContainsAny(value[:1], "-$") || isPlaceholderSecretValue(value) {
			return
		}
		if requireNonWord && isLowercaseWord(value) {
			return
		}
		regions = append(regions, taggedRegion{region: region{start, end}})
	}
	if hasFlag {
		for _, loc := range cliLongPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
			add(loc[4], loc[5], s[loc[2]] != '=')
		}
		for _, pat := range cliShortPasswordPatterns {
			for _, loc := range pat.FindAllStringSubmatchIndex(s, -1) {
				add(loc[2], loc[3], false)
			}
		}
	}
	if hasNetrc {
		for _, loc := range netrcPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
			add(loc[2], loc[3], false)
		}
	}
	return regions
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
