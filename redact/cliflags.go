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
// skips quoted strings and escaped line breaks (a command continued with a
// trailing backslash), and otherwise never crosses a line break, a shell
// operator or a backtick (which starts a command substitution, or ends
// Markdown code), so a flag is only read as belonging to the command that
// precedes it in the same pipeline segment.
const cliSameCommand = `(?:\\\r?\n|"[^"\r\n]*"|'[^'\r\n]*'|[^\r\n|;&` + "`" + `])`

// cliBlank separates a flag from its value or from the next flag, possibly
// across an escaped line break.
const cliBlank = `(?:[ \t]|\\\r?\n)+`

// cliSegmentRest matches the rest of a pipeline segment after a command name.
var cliSegmentRest = regexp.MustCompile(`^` + cliSameCommand + `*`)

// cliLongPasswordPattern matches a GNU-style password flag on any command line
// (mysql --password=..., mongosh --password ..., az login --password ...).
// Group 1 is "=" or the blank that separates the flag from its value, and
// group 2 the value. --password-stdin and --password-file never match: the
// flag must be followed by "=" or a blank.
var cliLongPasswordPattern = regexp.MustCompile(`(?:^|[\s"'])--(?:password|passwd|pass)(=|` + cliBlank + `)(` + cliFlagValue + `)`)

// cliPasswordFlag is the short password flag of one family of clients. A
// short flag means different things to different programs (-p is a port to
// ssh and a profile to aws), so each flag is only read in the pipeline segment
// of a command that defines it.
type cliPasswordFlag struct {
	// needles are literals command needs, which spare most strings the regex.
	needles []string
	// command matches the client name, up to where its arguments start.
	command *regexp.Regexp
	// flag matches one password flag in the rest of the segment; group 1 is
	// the value.
	flag *regexp.Regexp
	// prefix starts every value and is not part of the password (openssl's
	// pass:); a value without it names another kind of source.
	prefix string
	// optional marks a client that prompts when the flag has no value, so a
	// following word that starts with '-' is the next flag.
	optional bool
	// containerName marks a client name that is also a common container or
	// image name, so a port mapping after it (docker run --name mongo -p
	// 27017:27017) is docker's -p.
	containerName bool
}

var cliPasswordFlags = []cliPasswordFlag{
	// mysql takes the password glued to -p; a bare -p prompts for it.
	{
		needles:       []string{"mysql", "mariadb"},
		command:       regexp.MustCompile(`\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlimport|mysqlcheck|mysqlpump|mysqlsh)\b`),
		flag:          regexp.MustCompile(`\s-p(` + cliFlagValue + `)`),
		containerName: true,
	},
	{
		needles: []string{"login"},
		command: regexp.MustCompile(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm)\b` + cliSameCommand + `*?\blogin\b`),
		flag:    regexp.MustCompile(`\s-p(?:` + cliBlank + `)?(` + cliFlagValue + `)`),
	},
	{
		needles: []string{"login"},
		command: regexp.MustCompile(`\baz[ \t]+login\b`),
		flag:    regexp.MustCompile(`\s-p(?:` + cliBlank + `)?(` + cliFlagValue + `)`),
	},
	{
		needles:       []string{"mongo"},
		command:       regexp.MustCompile(`\b(?:mongosh|mongo|mongodump|mongorestore|mongoexport|mongoimport|mongostat|mongotop)\b`),
		flag:          regexp.MustCompile(`\s-p` + cliBlank + `(` + cliFlagValue + `)`),
		optional:      true,
		containerName: true,
	},
	// sshpass reads its own options only before the command it wraps, so
	// the -p of sshpass -e ssh -p 2222 is ssh's port.
	{
		needles: []string{"sshpass"},
		command: regexp.MustCompile(`\bsshpass\b`),
		flag:    regexp.MustCompile(`^(?:` + cliBlank + `-(?:v|P[ \t]*(?:` + cliFlagValue + `)))*` + cliBlank + `-p[ \t]*(` + cliFlagValue + `)`),
	},
	{
		needles: []string{"redis-cli"},
		command: regexp.MustCompile(`\bredis-cli\b`),
		flag:    regexp.MustCompile(`\s-a` + cliBlank + `(` + cliFlagValue + `)`),
	},
	{
		needles: []string{"pass:"},
		command: regexp.MustCompile(`\bopenssl\b`),
		flag:    regexp.MustCompile(`\s-pass(?:in|out|word)?` + cliBlank + `(` + cliFlagValue + `)`),
		prefix:  "pass:",
	},
}

// portMappingPattern matches a container port mapping such as 27017:27017 or
// 127.0.0.1:8080:80/tcp.
var portMappingPattern = regexp.MustCompile(`^(?:[0-9.]+:)?[0-9]+(?:-[0-9]+)?:[0-9]+(?:-[0-9]+)?(?:/(?:tcp|udp|sctp))?$`)

// netrcValue is a .netrc token. A quote or backtick ends it when whitespace or
// a shell metacharacter follows, so the closing quote of
// echo "machine h login me password p" > ~/.netrc stays outside the value.
const netrcValue = `"[^"\r\n]*"|'[^'\r\n]*'|[^\s"'` + "`" + `]+(?:["'` + "`" + `]+[^\s"'` + "`" + `;|&()<>][^\s"'` + "`" + `]*)*`

// netrcEntry starts a .netrc entry. A default entry must start a line or a
// quoted string, so "by default login with password is" is not one. Group 1
// is the machine, absent for default.
const netrcEntry = `(?:\bmachine[ \t\r\n]+(\S+)|(?:^|[\r\n"'])[ \t]*default)`

const (
	netrcSep     = `[ \t\r\n]+`
	netrcAccount = `(?:` + netrcSep + `account` + netrcSep + `\S+)?`
)

// netrcPasswordPatterns match the password of a .netrc entry that also names
// its login, in either order, on one line or across several. The login is
// required, so prose such as "machine learning password policy" is not an
// entry.
var netrcPasswordPatterns = []struct {
	pattern *regexp.Regexp
	// login and value are the indexes of the login and password groups.
	login, value int
}{
	{regexp.MustCompile(netrcEntry + netrcSep + `login` + netrcSep + `(\S+)` + netrcAccount + netrcSep + `password` + netrcSep + `(` + netrcValue + `)`), 2, 3},
	{regexp.MustCompile(netrcEntry + netrcAccount + netrcSep + `password` + netrcSep + `(` + netrcValue + `)` + netrcAccount + netrcSep + `login` + netrcSep + `(\S+)`), 3, 2},
}

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
		for _, flag := range cliPasswordFlags {
			regions = flag.appendPasswords(regions, s)
		}
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
		if r, ok := passwordValueRegion(s, shellWordValue(s, loc[4], loc[5]), true, s[loc[2]] != '='); ok {
			regions = append(regions, r)
		}
	}
	return regions
}

// appendPasswords adds the value of every password flag in the segment of
// each command f defines, so a second flag (openssl -passin pass:a -passout
// pass:b) is read too.
func (f cliPasswordFlag) appendPasswords(regions []taggedRegion, s string) []taggedRegion {
	if !slices.ContainsFunc(f.needles, func(n string) bool { return strings.Contains(s, n) }) {
		return regions
	}
	scanned := 0
	for _, cmd := range f.command.FindAllStringIndex(s, -1) {
		if cmd[0] < scanned || f.containerName && isContainerNameArgument(s, cmd[0]) {
			continue
		}
		rest := cliSegmentRest.FindString(s[cmd[1]:])
		scanned = cmd[1] + len(rest)
		for _, loc := range f.flag.FindAllStringSubmatchIndex(rest, -1) {
			if r, ok := f.valueRegion(s, cmd[1]+loc[2], cmd[1]+loc[3]); ok {
				regions = append(regions, r)
			}
		}
	}
	return regions
}

func (f cliPasswordFlag) valueRegion(s string, start, end int) (taggedRegion, bool) {
	v := shellWordValue(s, start, end)
	if f.prefix != "" {
		var ok bool
		if v, ok = v.trimPrefix(s, f.prefix); !ok {
			return taggedRegion{}, false
		}
	}
	if f.containerName && portMappingPattern.MatchString(s[v.start:v.end]) {
		return taggedRegion{}, false
	}
	return passwordValueRegion(s, v, f.optional, false)
}

// cliValue is a password on a command line.
type cliValue struct {
	// start and end bound the password, inside its quotes.
	start, end int
	// quoted marks a value that was one quoted string, and singleQuoted one
	// in single quotes, where $ is literal.
	quoted, singleQuoted bool
}

// shellWordValue returns the password in the shell word that starts at start,
// whose first piece cliFlagValue matched up to end. The shell joins quoted
// and bare pieces into one argument ('alpha'hunter2, "abc\"def"), so the
// value is the whole word, without its quotes when it is one quoted string.
func shellWordValue(s string, start, end int) cliValue {
	wordEnd := shellWordEnd(s, start)
	if wordEnd <= start {
		wordEnd = end
	}
	if pieceEnd, quoteLen := quotedPiece(s, start); quoteLen > 0 {
		inner := cliValue{start: start + quoteLen, end: pieceEnd - quoteLen, quoted: true, singleQuoted: s[start] == '\''}
		if pieceEnd >= wordEnd || isLiteralGap(s[inner.start:inner.end]) {
			return inner
		}
	}
	return cliValue{start: start, end: wordEnd}
}

// trimPrefix drops prefix from the start of v, also when v is a word whose
// first quoted piece starts with it ('pass:abc'def).
func (v cliValue) trimPrefix(s, prefix string) (cliValue, bool) {
	skip := 0
	if !v.quoted && v.start < v.end && (s[v.start] == '"' || s[v.start] == '\'') {
		skip = 1
	}
	if !strings.HasPrefix(s[v.start+skip:v.end], prefix) {
		return v, false
	}
	v.start += skip + len(prefix)
	return v, true
}

// shellWordEnd returns the end of the shell word that starts at start. A
// quoted piece is part of the word, and so is an escaped blank. Commands are
// often quoted inside other text, as the argument of sh -c or as a JSON
// string, so a bare piece ends at an escaped quote (\"), and joins a quoted
// piece that follows it only when the word ends right after that piece and
// the piece is neither a gap between string literals nor holds a shell
// operator: the quote after hunter2 in "mysql -phunter2","x" or
// sh -c "mysql -phunter2"; echo " ok" closes the string around the command.
func shellWordEnd(s string, start int) int {
	i, afterBare := start, false
	for i < len(s) {
		if pieceEnd, quoteLen := quotedPiece(s, i); quoteLen == 1 || quoteLen == 2 && i == start {
			inner := s[i+quoteLen : pieceEnd-quoteLen]
			if afterBare && (pieceEnd < len(s) && !isShellWordEnd(s[pieceEnd]) || isLiteralGap(inner) || strings.ContainsAny(inner, ";|&<>()`")) {
				return i
			}
			i, afterBare = pieceEnd, false
			continue
		}
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && strings.IndexByte(" \t'\"", s[i+1]) >= 0:
			if s[i+1] == '"' && i > start {
				return i
			}
			i += 2
		case c == '"' || c == '\'' || isShellWordEnd(c):
			return i
		default:
			i++
		}
		afterBare = true
	}
	return i
}

// quotedPiece returns the end of the quoted string that starts at i, and the
// length of its quote: 1 for ' and ", 2 for \" (a quote inside a JSON
// string). It is 0 when no quoted string starts at i, or when the string does
// not close on its line. A backslash escapes the next character inside double
// quotes.
func quotedPiece(s string, i int) (end, quoteLen int) {
	switch {
	case s[i] == '"' || s[i] == '\'':
		quoteLen = 1
	case strings.HasPrefix(s[i:], `\"`):
		quoteLen = 2
	default:
		return 0, 0
	}
	quote := s[i : i+quoteLen]
	for j := i + quoteLen; j < len(s); j++ {
		switch {
		case s[j] == '\r' || s[j] == '\n':
			return 0, 0
		case strings.HasPrefix(s[j:], quote):
			return j + quoteLen, quoteLen
		case quote == `"` && s[j] == '\\' && j+1 < len(s) && s[j+1] != '\r' && s[j+1] != '\n':
			j++
		}
	}
	return 0, 0
}

// isShellWordEnd reports whether c ends an unquoted shell word.
func isShellWordEnd(c byte) bool {
	return strings.IndexByte(" \t\r\n`|;&()<>", c) >= 0
}

// isLiteralGap reports whether the inside of a quoted piece is the gap
// between two string literals rather than a value: JSON separators
// ({"cmd":"mysql -p","x":1}), or code joined by blanks
// ("mysql -p" + password + " db").
func isLiteralGap(s string) bool {
	return strings.Trim(s, ",:[]{} \t") == "" || strings.TrimSpace(s) != s
}

// isContainerNameArgument reports whether the client name at start is the
// value of --name (docker run --name mysql -p3306 mysql:8), not a command.
func isContainerNameArgument(s string, start int) bool {
	before := s[:start]
	if strings.HasSuffix(before, "--name=") {
		return true
	}
	trimmed := strings.TrimRight(before, " \t")
	return len(trimmed) < len(before) && strings.HasSuffix(trimmed, "--name")
}

func appendNetrcPasswords(regions []taggedRegion, s string) []taggedRegion {
	for _, p := range netrcPasswordPatterns {
		for _, loc := range p.pattern.FindAllStringSubmatchIndex(s, -1) {
			machine := ""
			if loc[2] >= 0 {
				machine = s[loc[2]:loc[3]]
			}
			login := s[loc[2*p.login]:loc[2*p.login+1]]
			start, end := loc[2*p.value], loc[2*p.value+1]
			if isNetrcDeclaration(machine, login, s[start:end]) {
				continue
			}
			valueStart, valueEnd, singleQuoted := unquoteRangeQuoted(s, start, end)
			v := cliValue{start: valueStart, end: valueEnd, quoted: valueStart != start, singleQuoted: singleQuoted}
			if r, ok := passwordValueRegion(s, v, false, false); ok {
				regions = append(regions, r)
			}
		}
	}
	return regions
}

// passwordValueRegion returns the region of the password v, unless it is not
// a secret. The value stays visible when it is one of the shared placeholders
// ($PASSWORD, ${PASSWORD}, <password>) or a command substitution that fetches
// the password from elsewhere ($(cat file)); inside single quotes $ is
// literal. When dashFlag is set, an unquoted value that starts like another
// flag is that flag. A long flag followed by a blank instead of "=" is also
// common in prose ("pass --password to set it"), so there (requireNonWord) an
// unquoted value must contain something other than lowercase letters.
func passwordValueRegion(s string, v cliValue, dashFlag, requireNonWord bool) (taggedRegion, bool) {
	value := s[v.start:v.end]
	switch {
	case value == "", dashFlag && !v.quoted && value[0] == '-', v.quoted && isLiteralGap(value):
		return taggedRegion{}, false
	case v.singleQuoted:
		if !hasNonPlaceholderQuotedPasswordValue(value, true) {
			return taggedRegion{}, false
		}
	case strings.HasPrefix(s[v.start:], "$("):
		substitutionEnd, recipe := commandSubstitution(s, v.start)
		if recipe && substitutionEnd >= v.end {
			return taggedRegion{}, false
		}
		v.end = max(v.end, substitutionEnd)
		if !v.quoted {
			// A literal suffix joined to the substitution is part of the word.
			v.end = max(v.end, shellWordEnd(s, v.end))
		}
	case !hasNonPlaceholderPasswordValue(value):
		return taggedRegion{}, false
	}
	if requireNonWord && !v.quoted && isLowercaseWord(s[v.start:v.end]) {
		return taggedRegion{}, false
	}
	return taggedRegion{region: region{v.start, v.end}}, true
}

// commandSubstitution returns the end of the $(...) that starts at start, and
// whether it is the whole word and only runs a command that fetches a secret
// from elsewhere. $(printf hunter2) carries the password itself.
func commandSubstitution(s string, start int) (int, bool) {
	body, _, closed := strings.Cut(s[start+2:], ")")
	if !closed {
		return start + 1, false
	}
	end := start + 2 + len(body) + 1
	wordEnds := end == len(s) || strings.ContainsRune(" \t\r\n\"'`|;&()<>", rune(s[end]))
	return end, wordEnds && isSecretRecipeCommand(body)
}

// isNetrcDeclaration reports whether a .netrc match gives its machine, login
// and password one token, the shape of a declaration (machine string, login
// string, password string in a Go struct) rather than of an entry.
func isNetrcDeclaration(machine, login, password string) bool {
	return machine == password && login == password
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
