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

// cliSameCommandChar is one piece of the text between a command name and one
// of its flags. It skips quoted strings, escaped characters (\;) and escaped
// line breaks (a command continued with a trailing backslash), and otherwise
// never crosses a line break, a shell operator or a backtick (which starts a
// command substitution, or ends Markdown code), so a flag is only read as
// belonging to the command that precedes it in the same pipeline segment.
const cliSameCommandChar = `\\\r?\n|\\[^\r\n]|"[^"\r\n]*"|'[^'\r\n]*'|[^\r\n|;&` + "`" + `]`

const cliSameCommand = `(?:` + cliSameCommandChar + `)`

// cliBlank separates a flag from its value or from the next flag, possibly
// across an escaped line break.
const cliBlank = `(?:[ \t]|\\\r?\n)+`

// cliSegmentRest matches the rest of a pipeline segment after a command name.
// cliEscapedSegmentRest also skips strings in escaped double quotes, which
// quote for a command inside a double-quoted string
// (sh -c "mysql -e \"select 1; select 2\" -pX", a command in a JSON string);
// elsewhere \" is a literal quote.
var (
	cliSegmentRest        = regexp.MustCompile(`^` + cliSameCommand + `*`)
	cliEscapedSegmentRest = regexp.MustCompile(`^(?:\\"[^"\r\n]*?\\"|` + cliSameCommandChar + `)*`)
)

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
	// leading marks a flag that is only read right after the command name,
	// so every occurrence of the name is tried (docker exec sshpass sshpass
	// -p ...).
	leading bool
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
		leading: true,
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

// netrcValue is a .netrc token. A double-quoted one may hold escaped quotes
// ("abc\"def", as curl reads it). Outside quotes, a quote or backtick ends it
// when whitespace or a shell metacharacter follows, so the closing quote of
// echo "machine h login me password p" > ~/.netrc stays outside the value.
const netrcValue = `"(?:[^"\\\r\n]|\\[^\r\n])*"|'[^'\r\n]*'|[^\s"'` + "`" + `]+(?:["'` + "`" + `]+[^\s"'` + "`" + `;|&()<>][^\s"'` + "`" + `]*)*`

// netrcName is a .netrc machine, login or account name, quoted when it
// holds blanks (login "alice smith").
const netrcName = `"(?:[^"\\\r\n]|\\[^\r\n])*"|\S+`

// netrcEntry starts a .netrc entry. A default entry must start a line or a
// quoted string, so "by default login with password is" is not one. Group 1
// is the machine, absent for default.
const netrcEntry = `(?:\bmachine[ \t\r\n]+(` + netrcName + `)|(?:^|[\r\n"'])[ \t]*default)`

const (
	// netrcSep separates .netrc tokens: blanks, line breaks and comment
	// lines.
	netrcSep     = `(?:[ \t\r\n]+|[\r\n][ \t]*#[^\r\n]*)+`
	netrcAccount = `(?:` + netrcSep + `account` + netrcSep + `(?:` + netrcName + `))?`
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
	{regexp.MustCompile(netrcEntry + netrcAccount + netrcSep + `login` + netrcSep + `(` + netrcName + `)` + netrcAccount + netrcSep + `password` + netrcSep + `(` + netrcValue + `)`), 2, 3},
	{regexp.MustCompile(netrcEntry + netrcAccount + netrcSep + `password` + netrcSep + `(` + netrcValue + `)` + netrcAccount + netrcSep + `login` + netrcSep + `(` + netrcName + `)`), 3, 2},
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
	lines := quoteScan{s: s, prose: true}
	for _, loc := range cliLongPasswordPattern.FindAllStringSubmatchIndex(s, -1) {
		// After "=" a leading '-' is part of the value; after a blank it
		// starts the next flag.
		blank := s[loc[2]] != '='
		v := shellWordValue(s, loc[4], loc[5], lines.stringEnd(loc[2]-1))
		if r, ok := passwordValueRegion(s, v, blank, blank); ok {
			regions = append(regions, r)
		}
	}
	return regions
}

// appendPasswords adds the value of every password flag in the segment of
// each command f defines. A command inside a quoted argument of an earlier
// one (docker exec db sh -c 'mysql -pX') runs on its own, so its segment ends
// with that argument; any other command in an earlier segment was read with
// it.
func (f cliPasswordFlag) appendPasswords(regions []taggedRegion, s string) []taggedRegion {
	if !slices.ContainsFunc(f.needles, func(n string) bool { return strings.Contains(s, n) }) {
		return regions
	}
	lines := quoteScan{s: s, prose: true}
	var segment quoteScan
	segmentEnd, nestedEnd := 0, 0
	for _, cmd := range f.command.FindAllStringIndex(s, -1) {
		if f.containerName && isContainerNameArgument(s, cmd[0]) {
			continue
		}
		// The closing quote of a string the command is quoted in is not
		// an opening one: "/usr/bin/mysql" -p"x" or sh -c "mysql ...".
		closeAt, stringEnd := lines.enclosing(cmd[0])
		end := len(s)
		switch {
		case cmd[0] >= segmentEnd:
			segment = quoteScan{s: s, i: cmd[1], skip: closeAt, skipEnd: stringEnd}
		case f.leading:
		default:
			argEnd, nested := segment.stringAt(cmd[0])
			if !nested || cmd[0] < nestedEnd || !startsCommand(s[segment.inner():cmd[0]]) {
				continue
			}
			end = max(argEnd-1, cmd[1])
		}
		rest := segmentRestPattern(s, closeAt).FindString(s[cmd[1]:end])
		if end == len(s) {
			segmentEnd = max(segmentEnd, cmd[1]+len(rest))
		} else {
			nestedEnd = cmd[1] + len(rest)
		}
		quotes := quoteScan{s: s[:cmd[1]+len(rest)], i: cmd[1], skip: closeAt, skipEnd: stringEnd}
		regions = f.appendFlags(regions, s, &quotes, rest, stringEnd)
	}
	return regions
}

// segmentRestPattern returns the pattern for the rest of the segment of a
// command whose enclosing string closes at closeAt, or -1.
func segmentRestPattern(s string, closeAt int) *regexp.Regexp {
	if closeAt >= 0 && s[closeAt] == '"' {
		return cliEscapedSegmentRest
	}
	return cliSegmentRest
}

// startsCommand reports whether a command name after prefix, the text from
// the start of a quoted argument, is run as a command (sh -c 'mysql -pX',
// sh -c 'cd app && sudo mysql -pX') rather than being text in it (the SQL
// of mysql -e 'select mysql -pX').
func startsCommand(prefix string) bool {
	if i := strings.LastIndexAny(prefix, ";|&("); i >= 0 {
		prefix = prefix[i+1:]
	}
	for _, word := range strings.Fields(prefix) {
		switch word {
		case "sudo", "exec", "env", "time", "nohup", "command":
			continue
		}
		if eq := strings.IndexByte(word, '='); eq <= 0 || !isShellName(word[:eq]) {
			return false
		}
	}
	return true
}

// isShellName reports whether s is a shell variable name (an environment
// assignment's NAME=).
func isShellName(s string) bool {
	for i := range len(s) {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return s != ""
}

// appendFlags adds the value of every password flag in rest, the segment
// after a command, so a second flag (openssl -passin pass:a -passout pass:b)
// is read too. quotes scans rest from its start; a flag inside a quoted
// argument (the SQL of mysql -e 'select -p1') is text, not a flag. stringEnd
// is where the quoted string holding the command ends, or -1.
func (f cliPasswordFlag) appendFlags(regions []taggedRegion, s string, quotes *quoteScan, rest string, stringEnd int) []taggedRegion {
	base := quotes.i
	for _, loc := range f.flag.FindAllStringSubmatchIndex(rest, -1) {
		if _, quoted := quotes.stringAt(base + loc[0]); quoted {
			continue
		}
		if r, ok := f.valueRegion(s, base+loc[2], base+loc[3], stringEnd); ok {
			regions = append(regions, r)
		}
	}
	return regions
}

func (f cliPasswordFlag) valueRegion(s string, start, end, stringEnd int) (taggedRegion, bool) {
	v := shellWordValue(s, start, end, stringEnd)
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
	// gap marks a quoted value that is the gap between two strings in code
	// or JSON, not a value (see isStringGap).
	gap bool
}

// shellWordValue returns the password in the shell word that starts at start,
// whose first piece cliFlagValue matched up to end. The shell joins quoted
// and bare pieces into one argument ('alpha'hunter2, "abc\"def"), so the
// value is the whole word, without its quotes when it is one quoted string.
// stringEnd is where the quoted string holding the flag ends, or -1.
func shellWordValue(s string, start, end, stringEnd int) cliValue {
	wordEnd := shellWordEnd(s, start)
	if wordEnd <= start {
		wordEnd = end
	}
	if pieceEnd, quoteLen := quotedPiece(s, start); quoteLen > 0 {
		inner := cliValue{start: start + quoteLen, end: pieceEnd - quoteLen, quoted: true, singleQuoted: s[start] == '\''}
		inner.gap = inner.start == stringEnd && isStringGap(s[inner.start:inner.end])
		if pieceEnd >= wordEnd || inner.gap {
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
// quoted piece is part of the word, and so are an escaped character and a
// command substitution. Commands are often quoted inside other text, as the
// argument of sh -c or as a JSON string, so a bare piece ends at an escaped
// quote (\"), and does not join a quoted piece that starts or ends with a
// blank or holds a shell operator or JSON separator: the quote after hunter2
// in "mysql -phunter2","x" or sh -c "mysql -phunter2"; echo " ok" closes the
// string around the command.
func shellWordEnd(s string, start int) int {
	i, afterBare := start, false
	for i < len(s) {
		if pieceEnd, quoteLen := quotedPiece(s, i); quoteLen == 1 || quoteLen == 2 && i == start {
			if afterBare && !joinsBareWord(s[i+quoteLen:pieceEnd-quoteLen]) {
				return i
			}
			i, afterBare = pieceEnd, false
			continue
		}
		if end, ok := substitutionEnd(s, i); ok {
			i, afterBare = end, true
			continue
		}
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] != '\r' && s[i+1] != '\n':
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

// substitutionEnd returns the end of the $(...) that starts at i, after its
// matching parenthesis, and false when none starts there or it does not
// close.
func substitutionEnd(s string, i int) (int, bool) {
	if !strings.HasPrefix(s[i:], "$(") {
		return 0, false
	}
	depth := 0
	for j := i + 1; j < len(s); j++ {
		if pieceEnd, quoteLen := quotedPiece(s, j); quoteLen == 1 {
			j = pieceEnd - 1
			continue
		}
		switch s[j] {
		case '\\':
			j++
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return j + 1, true
			}
		}
	}
	return 0, false
}

// joinsBareWord reports whether a quoted piece with this inside, right after
// a bare piece, continues the word rather than closing a string around the
// command.
func joinsBareWord(inner string) bool {
	return strings.TrimSpace(inner) == inner && !strings.ContainsAny(inner, ";|&<>()`,:")
}

// quoteScan finds the quoted strings of s, from position i on, in one pass
// for queries at increasing positions, so a line with many flags or commands
// is read once.
type quoteScan struct {
	s string
	// i is where the scan goes on; start and end bound the last quoted
	// string it found.
	i, start, end int
	// skip is the closing quote of a string that opened before the scan
	// started, stepped over up to skipEnd instead of read as an opening
	// quote.
	skip, skipEnd int
	// prose skips an apostrophe inside a word (Let's), which opens no
	// string in the text around a command.
	prose bool
}

// stringAt returns the end of the quoted string that p is inside, after its
// opening quote, and false when p is not in one. p is never before an
// earlier query's.
func (q *quoteScan) stringAt(p int) (end int, ok bool) {
	if q.start < p && p < q.end {
		return q.end, true
	}
	for q.i < p {
		if q.i == q.skip && q.skipEnd > q.i {
			q.i = q.skipEnd
			continue
		}
		if q.prose && isApostrophe(q.s, q.i) {
			q.i++
			continue
		}
		if end, quoteLen := quotedPiece(q.s, q.i); quoteLen > 0 {
			q.start, q.end, q.i = q.i, end, end
			if p < end {
				return end, true
			}
			continue
		}
		if q.s[q.i] == '\\' {
			q.i++
		}
		q.i++
	}
	return 0, false
}

// stringEnd returns where the quoted string that p is inside ends, after its
// closing quote, or -1 when p is not in one. The scan starts at a line
// start, so this is the string of the line the command is quoted in
// ("mysql -p" + password, {"cmd":"mysql -p"}).
func (q *quoteScan) stringEnd(p int) int {
	_, end := q.enclosing(p)
	return end
}

// enclosing returns where the closing quote of the quoted string that p is
// inside starts, and where it ends, or -1 and -1 when p is not in one.
func (q *quoteScan) enclosing(p int) (closeAt, end int) {
	end, ok := q.stringAt(p)
	if !ok {
		return -1, -1
	}
	return end - q.quoteLen(), end
}

// inner returns where the inside of the last quoted string found starts.
func (q *quoteScan) inner() int {
	return q.start + q.quoteLen()
}

// quoteLen returns the length of the quote of the last quoted string found:
// 2 for \", 1 otherwise.
func (q *quoteScan) quoteLen() int {
	if q.s[q.start] == '\\' {
		return 2
	}
	return 1
}

// isApostrophe reports whether s[i] is a ' between two letters.
func isApostrophe(s string, i int) bool {
	return s[i] == '\'' && i > 0 && i+1 < len(s) && isLetterByte(s[i-1]) && isLetterByte(s[i+1])
}

// isStringGap reports whether the inside of a quoted piece that opens with
// the closing quote of the string holding a command is the gap before the
// next string rather than a value: JSON separators
// ({"cmd":"mysql -p","x":1}), a concatenation in code
// ("mysql -p" + password + " db") or shell operators
// (sh -c "mysql -p"; echo "ok"). A password such as '+hunter2+' outside such
// a string is a value.
func isStringGap(s string) bool {
	trimmed := strings.TrimSpace(s)
	switch {
	case trimmed == "" || strings.Trim(trimmed, ",:[]{}") == "":
		return true
	case strings.ContainsAny(trimmed, ";|&<>()`"):
		return true
	}
	return strings.ContainsRune("+.", rune(trimmed[0])) && strings.ContainsRune("+.", rune(trimmed[len(trimmed)-1]))
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
		lines := quoteScan{s: s, prose: true}
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
			v.gap = v.quoted && valueStart == lines.stringEnd(start-1) && isStringGap(s[valueStart:valueEnd])
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
	case value == "", dashFlag && !v.quoted && value[0] == '-', v.gap:
		return taggedRegion{}, false
	case v.singleQuoted:
		if !hasNonPlaceholderQuotedPasswordValue(value, true) {
			return taggedRegion{}, false
		}
	case strings.HasPrefix(s[v.start:], "$("):
		// Only a substitution that is the whole value and fetches the
		// password from elsewhere stays visible: $(printf hunter2) and
		// $(cat f)suffix carry it.
		if end, closed := substitutionEnd(s, v.start); closed {
			if end >= v.end && isSecretRecipeCommand(s[v.start+2:end-1]) {
				return taggedRegion{}, false
			}
			v.end = max(v.end, end)
		}
	case !hasNonPlaceholderPasswordValue(value):
		return taggedRegion{}, false
	}
	if requireNonWord && !v.quoted && isLowercaseWord(s[v.start:v.end]) {
		return taggedRegion{}, false
	}
	return taggedRegion{region: region{v.start, v.end}}, true
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
