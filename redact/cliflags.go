package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// cliFlagValue is one password argument on a command line: a quoted string
// that stays on its line (a double-quoted one may go on past an escaped line
// break, which the shell removes), or a bare word that ends at whitespace, a
// quote or a shell metacharacter. Ending at a backtick or a parenthesis keeps
// the closing delimiter of Markdown code (`mysql -pX`) and of $(...) out of
// the value.
const cliFlagValue = `"(?:[^"\r\n]|\\\r?\n)*"|'[^'\r\n]*'|[^\s'"` + "`" + `|;&()<>]+`

// cliSameCommandChar is one piece of the text between a command name and one
// of its flags. It skips quoted strings (with escapes inside double quotes:
// "abc\";x"), command substitutions ($(printf x; true)), parameter
// expansions (${IN:-a&b}), escaped characters (\;) and escaped line breaks
// (a command continued with a trailing backslash), and otherwise never
// crosses a line break, a shell operator or a backtick (which starts a
// command substitution, or ends Markdown code), so a flag is only read as
// belonging to the command that precedes it in the same pipeline segment.
// The & and | of a redirection (2>&1, &>log, >|f) are no shell operator.
const cliSameCommandChar = `\\\r?\n|\\[^\r\n]|"(?:[^"\\\r\n]|\\\r?\n|\\[^\r\n])*"|'[^'\r\n]*'|` + cliSubstitutionPattern + `|\$` + cliBraces2 + `|[<>]&|&>|>\||[^\r\n|;&` + "`" + `]`

const cliSameCommand = `(?:` + cliSameCommandChar + `)`

// cliSubstitutionPattern is a command substitution, or a process
// substitution (<(printf x | cat), >(tee log)), whose parentheses nest at
// most three deep ($(dirname $(pwd); true), $((1 + 2))), whose quoted
// strings may hold any character ($(printf ')')), and cliSubstitution one
// at the start of a string.
const cliSubstitutionPattern = `[$<>]` + cliParens3

// cliParens1 to cliParens3 are text in parentheses that nest at most one to
// three deep. Inside, a quoted string, an escaped character or a nested
// $(...) or (...) is one piece.
const (
	cliParensPiece = `\\[^\r\n]|[^()'"\\\r\n]|'[^'\r\n]*'|"(?:[^"\\\r\n]|\\[^\r\n])*"`
	cliParens1     = `\((?:` + cliParensPiece + `)*\)`
	cliParens2     = `\((?:` + cliParensPiece + `|\$?` + cliParens1 + `)*\)`
	cliParens3     = `\((?:` + cliParensPiece + `|\$?` + cliParens2 + `)*\)`
)

var cliSubstitution = regexp.MustCompile(`^` + cliSubstitutionPattern)

// cliBraceExpansion matches a parameter expansion at the start of a string
// (${PW:-hunter&2}, ${PW:-"a b"}), whose braces nest at most two deep. Inside
// it, blanks and shell operators are part of the word.
var cliBraceExpansion = regexp.MustCompile(`^\$` + cliBraces2)

// cliBraces1 and cliBraces2 are text in braces that nest at most one and two
// deep. Inside, a quoted string or an escaped character is one piece.
const (
	cliBracesPiece = `\\[^\r\n]|[^{}'"\\\r\n]|'[^'\r\n]*'|"(?:[^"\\\r\n]|\\[^\r\n])*"`
	cliBraces1     = `\{(?:` + cliBracesPiece + `)*\}`
	cliBraces2     = `\{(?:` + cliBracesPiece + `|\$?` + cliBraces1 + `)*\}`
)

// cliBlank separates a flag from its value or from the next flag, possibly
// across an escaped line break.
const cliBlank = `(?:[ \t]|\\\r?\n)+`

// cliFlagQuote is the quote that opens a quoted short flag, as group 1: one
// quoted with its value ("-pSummer 2026") or on its own ("-a" x, "-p"'x').
// cliFlagClose, group 2, is the quote after the flag's name, which closes
// one quoted on its own.
const (
	cliFlagQuote = `(\\?["'])`
	cliFlagClose = `((?:\\?["'])?)`
)

// cliGlobalOptions matches the options a client takes before its
// subcommand, each a shell word (--config="$HOME/.docker") with at most one
// value (--config $(mktemp -d)).
const cliGlobalOptions = `(?:` + cliBlank + `-` + cliWordPiece + `+(?:` + cliBlank + cliWordPiece + `+)?)*`

// cliWordPiece is one piece of a shell word: a quoted string, a command
// substitution, a parameter expansion (${D:-a b}), an escaped line break,
// which the shell removes (--config "/tmp/do\<newline>cker"), or another
// character.
const cliWordPiece = `(?:"(?:[^"\\\r\n]|\\[^\r\n]|\\\r?\n)*"|'[^'\r\n]*'|` + cliSubstitutionPattern + `|\$` + cliBraces2 + `|\\\r?\n|[^\s'"` + "`" + `|;&()<>])`

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
// Group 1 is the quote that closes a quoted flag ("--password" x, or
// \"--password\" x in a double-quoted string), group 2 "=" or the blank that
// separates the flag from its value, and group 3 the value. --password-stdin
// and --password-file never match: the flag must be followed by "=" or a
// blank.
var cliLongPasswordPattern = regexp.MustCompile(`(?:^|[\s"'])--(?:password|passwd|pass)((?:\\?["'])?)(=|` + cliBlank + `)(` + cliFlagValue + `)`)

// cliPasswordFlag is the short password flag of one family of clients. A
// short flag means different things to different programs (-p is a port to
// ssh and a profile to aws), so each flag is only read in the pipeline segment
// of a command that defines it.
type cliPasswordFlag struct {
	// needles are literals command needs, which spare most strings the regex.
	needles []string
	// command matches the client name, up to where its arguments start.
	command *regexp.Regexp
	// flag matches one password flag in the rest of the segment; its last
	// group is the value. In a flag with three groups, groups 1 and 2 are
	// the quotes of a quoted flag (cliFlagQuote, cliFlagClose).
	flag *regexp.Regexp
	// prefix starts every value and is not part of the password (openssl's
	// pass:); a value without it names another kind of source.
	prefix string
	// optional marks a client that prompts when the flag has no value, so a
	// following word that starts with '-' is the next flag.
	optional bool
	// leading marks a flag that is only read right after the command name,
	// so every occurrence of the name is tried (docker exec sshpass sshpass
	// -p ...).
	leading bool
}

var cliPasswordFlags = []cliPasswordFlag{
	// mysql takes the password glued to -p; a bare -p prompts for it.
	{
		needles: []string{"mysql", "mariadb"},
		command: regexp.MustCompile(`\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlimport|mysqlcheck|mysqlpump|mysqlsh)\b`),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "p", "")),
	},
	// login must be the client's subcommand, after its global options
	// (docker --config dir login, helm registry --debug login), so a
	// container named login (docker run --name login -p 8080:80) is not.
	{
		needles: []string{loginCommand},
		command: regexp.MustCompile(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm)\b(?i:\.exe)?["']?` + cliGlobalOptions + cliBlank + `(?:registry` + cliGlobalOptions + cliBlank + `)?login\b`),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "p", `(?:`+cliBlank+`)?`)),
	},
	{
		needles: []string{loginCommand},
		command: regexp.MustCompile(`\baz[ \t]+login\b`),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "p", `(?:`+cliBlank+`)?`)),
	},
	{
		needles:  []string{"mongo"},
		command:  regexp.MustCompile(`\b(?:mongosh|mongo|mongodump|mongorestore|mongoexport|mongoimport|mongostat|mongotop)\b`),
		flag:     regexp.MustCompile(cliShortFlag(`\s`, "p", cliBlank)),
		optional: true,
	},
	// sshpass reads its own options only before the command it wraps, so
	// the -p of sshpass -e ssh -p 2222 is ssh's port.
	{
		needles: []string{"sshpass"},
		command: regexp.MustCompile(`\bsshpass\b`),
		flag:    regexp.MustCompile(cliShortFlag(`^(?:`+cliBlank+`-(?:v|P[ \t]*(?:`+cliFlagValue+`)))*`+cliBlank, "p", `[ \t]*`)),
		leading: true,
	},
	{
		needles: []string{"redis-cli"},
		command: regexp.MustCompile(`\bredis-cli\b`),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "a", cliBlank)),
	},
	// openssl takes a source after a blank or "=" (-pass=pass:x).
	{
		needles: []string{"pass:"},
		command: regexp.MustCompile(`\bopenssl\b`),
		flag:    regexp.MustCompile(`\s(?:-pass(?:in|out|word)?|'-pass(?:in|out|word)?'|"-pass(?:in|out|word)?")(?:=|` + cliBlank + `)(` + cliFlagValue + `)`),
		prefix:  "pass:",
	},
}

// cliShortFlag matches the short flag -name after before, then sep and its
// value: -px, a flag quoted with its value ("-px", "-p x"), or one quoted on
// its own ("-p" x, "-p"'x').
func cliShortFlag(before, name, sep string) string {
	return before + `(?:` + cliFlagQuote + `-` + name + cliFlagClose + sep + `|-` + name + sep + `)(` + cliFlagValue + `)`
}

// netrcValue is a .netrc token. A double-quoted one may hold escaped quotes
// ("abc\"def", as curl reads it). Outside quotes, a quote or backtick ends it
// when whitespace, a shell metacharacter or a separator of code follows, so
// the closing quote of echo "machine h login me password p" > ~/.netrc and
// of fmt.Sprintf("machine %s login %s password %s", h, u, p) stays outside
// the value.
const netrcValue = `"(?:[^"\\\r\n]|\\[^\r\n])*"|'[^'\r\n]*'|[^\s"'` + "`" + `]+(?:["'` + "`" + `]+[^\s"'` + "`" + `;|&()<>,\]}][^\s"'` + "`" + `]*)*`

// netrcName is a .netrc machine, login or account name, quoted when it
// holds blanks (login "alice smith").
const netrcName = `"(?:[^"\\\r\n]|\\[^\r\n])*"|\S+`

// netrcEntry starts a .netrc entry. A default entry must start a line or a
// quoted string, so "by default login with password is" is not one. Group 1
// is the machine, absent for default.
const netrcEntry = `(?:\bmachine[ \t\r\n]+(` + netrcName + `)|(?:^|[\r\n"'])[ \t]*default)`

const (
	// netrcSep comes before a .netrc keyword: blanks, line breaks, and
	// comments, which run from a blank to the end of their line
	// (login me # build account). netrcValueSep comes before a value:
	// blanks, line breaks, comment lines, and a lone "#" or "# ..." that
	// runs to the end of its line (password # deploy, then the value on the
	// next line). A value may still start with # on the keyword's line
	// (password #x), or when no line follows.
	netrcSep      = `(?:[ \t\r\n]+(?:#[^\r\n]*[\r\n])?)+`
	netrcValueSep = `(?:\r?\n` + netrcCommentLines + `|[ \t]#(?:[ \t][^\r\n]*)?\r?\n` + netrcCommentLines + `|[ \t\r\n])+`
	// netrcCommentLines are whole comment lines after a line break. A
	// comment runs to the end of its line, so no part of it is a value.
	netrcCommentLines = `(?:[ \t]*#[^\r\n]*\r?\n)*`
	netrcAccount      = `(?:` + netrcSep + `account` + netrcValueSep + `(?:` + netrcName + `))?`
)

// netrcPasswordPatterns match the password of a .netrc entry that also names
// its login, in either order, on one line or across several. The login is
// required, so prose such as "machine learning password policy" is not an
// entry.
var netrcPasswordPatterns = []netrcPattern{
	{regexp.MustCompile(netrcEntry + netrcLoginPassword), 2, 3},
	{regexp.MustCompile(netrcEntry + netrcPasswordLogin), 3, 2},
}

// netrcDefaultPatterns match a default entry right after another entry on
// its line (machine h login a password x default login b password y), which
// netrcEntry does not, so prose with "default" is not read as an entry.
var netrcDefaultPatterns = []netrcPattern{
	{regexp.MustCompile(`^` + netrcAccount + `[ \t]+default()` + netrcLoginPassword), 2, 3},
	{regexp.MustCompile(`^` + netrcAccount + `[ \t]+default()` + netrcPasswordLogin), 3, 2},
}

// netrcLoginPassword and netrcPasswordLogin are the fields of a .netrc entry
// after its machine, in either order.
const (
	netrcLoginPassword = netrcAccount + netrcSep + `login` + netrcValueSep + `(` + netrcName + `)` + netrcAccount + netrcSep + `password` + netrcValueSep + `(` + netrcValue + `)`
	netrcPasswordLogin = netrcAccount + netrcSep + `password` + netrcValueSep + `(` + netrcValue + `)` + netrcAccount + netrcSep + `login` + netrcValueSep + `(` + netrcName + `)`
)

// netrcPattern is a .netrc entry pattern, whose group 1 is the machine, and
// login and value the indexes of the login and password groups.
type netrcPattern struct {
	pattern      *regexp.Regexp
	login, value int
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
	if strings.Contains(s, "password") && strings.Contains(s, loginCommand) {
		regions = appendNetrcPasswords(regions, s)
	}
	return regions
}

func appendLongPasswordFlags(regions []taggedRegion, s string) []taggedRegion {
	if !strings.Contains(s, "--pass") {
		return regions
	}
	lines := quoteScan{s: s, prose: true}
	for pos := 0; ; {
		loc := cliLongPasswordPattern.FindStringSubmatchIndex(s[pos:])
		if loc == nil {
			return regions
		}
		for k := range loc {
			if loc[k] >= 0 {
				loc[k] += pos
			}
		}
		// After "=" a leading '-' is part of the value; after a blank it
		// starts the next flag.
		blank := s[loc[4]] != '='
		v, ok := longFlagValue(s, loc, lines.stringEnd(loc[2]-1))
		var r taggedRegion
		if ok {
			r, ok = passwordValueRegion(s, v, blank, blank)
		}
		if !ok {
			// A value that is no password may hold the next flag
			// (echo "--password "; wget --password=x).
			pos = loc[6]
			continue
		}
		regions = append(regions, r)
		// A flag inside the password's word is part of it
		// (--password=x\ --password=y), and is not read again.
		pos = max(loc[1], r.end)
	}
}

// longFlagValue returns the value of the long password flag that loc, a
// cliLongPasswordPattern match, found. A flag quoted on its own
// ("--password" x) must close the quote that opens it, and a value in the
// string that a flag opens ("--password=Summer 2026") runs to that string's
// end, since the shell passes the whole string as one argument.
func longFlagValue(s string, loc []int, stringEnd int) (cliValue, bool) {
	open, quoteLen := loc[0], 0
	if s[open] == '"' && open > 0 && s[open-1] == '\\' {
		open--
	}
	if end, n := quotedPiece(s, open); n > 0 && end > loc[2] {
		quoteLen = n
		closeAt := end - n
		switch {
		case loc[3] > loc[2]:
			// "--password" x: the quote after the name closes this one.
			if closeAt != loc[3]-n {
				return cliValue{}, false
			}
		case s[loc[4]] == '=' && closeAt > loc[6]:
			return quotedFlagValue(s, open, loc[6], end, n, stringEnd), true
		}
	}
	if loc[3] > loc[2] && quoteLen == 0 {
		// A quote after the name that opened nothing before it.
		return cliValue{}, false
	}
	return shellWordValue(s, loc[6], loc[7], stringEnd), true
}

// appendPasswords adds the value of every password flag in the segment of
// each command f defines. A command inside a quoted argument or a command
// substitution of an earlier one, at any depth (docker exec db sh -c
// 'mysql -pX', mysql -h $(cat h; mysql -pX)), runs on its own, so its
// segment ends with that argument and the flags in it are not the earlier
// command's; any other command in an earlier segment was read with it.
func (f cliPasswordFlag) appendPasswords(regions []taggedRegion, s string) []taggedRegion {
	if !slices.ContainsFunc(f.needles, func(n string) bool { return strings.Contains(s, n) }) {
		return regions
	}
	lines := quoteScan{s: s, prose: true}
	var nested nestedScan
	var paths programPathMemo
	scripts := commandStart{lenient: true}
	var entrypoint entrypointArgs
	var entrypointStarts commandStart
	// A segment cut at a ")" (ssh $(mysql -Nse 'x') -p2222) goes on after
	// it, to uncutEnd, in a string that closes at uncutAt; a command there
	// starts a segment that ends there too, which is not found again.
	segmentEnd, uncutEnd, uncutAt := 0, 0, 0
	for _, cmd := range f.command.FindAllStringIndex(s, -1) {
		if !isCommandToken(s, cmd[0], cmd[1]) || isContainerNameArgument(s, cmd[0]) {
			continue
		}
		// The closing quote of a string the command is quoted in is not
		// an opening one: "/usr/bin/mysql" -p"x" or sh -c "mysql ...".
		closeAt, stringEnd := lines.enclosing(cmd[0])
		cmdFrom := lines.commandFrom(closeAt)
		if isWrapperOperand(s, cmdFrom, cmd[0]) || closeAt >= 0 && isPrintedInScript(s, &lines, &scripts, cmd[0]) {
			continue
		}
		skip, skipEnd := closeAt, stringEnd
		var rest string
		switch {
		case cmd[0] >= segmentEnd:
			nested = newNestedScan(s, cmd[1], closeAt, stringEnd)
			if cmd[1] >= uncutEnd || closeAt != uncutAt {
				uncutEnd, uncutAt = cmd[1]+len(f.segmentRest(s, &lines, &paths, cmd[1], closeAt, stringEnd)), closeAt
			}
			rest = untilUnmatchedParen(s[cmd[1]:uncutEnd], closeAt-cmd[1], stringEnd-cmd[1])
			segmentEnd = cmd[1] + len(rest)
		case f.leading:
			// Only the options right after the name are read, and they
			// end inside the segment already found, so it is not scanned
			// again for every name (which took quadratic time).
			rest = s[cmd[1]:max(segmentEnd, cmd[1])]
		default:
			var ok bool
			if rest, skip, skipEnd, ok = nested.rest(f, s, cmd, closeAt, stringEnd); !ok {
				continue
			}
		}
		from := cmd[1]
		if isEntrypointArgument(s, &entrypointStarts, cmdFrom, cmd[0]) {
			from = entrypoint.at(s, cmd[0], cmd[1]+len(rest), stringEnd)
		}
		quotes := quoteScan{s: s[:cmd[1]+len(rest)], i: from, skip: skip, skipEnd: skipEnd, substitutions: true}
		regions = f.appendFlags(regions, s, &quotes, s[from:cmd[1]+len(rest)], stringEnd)
	}
	return regions
}

// isPrintedInScript reports whether the client name at p, in the quoted
// string lines last found, is printed rather than run because the string is
// a shell's -c script (sh -c 'ssh db echo mysql -pX'). The script is read as
// a command substitution is, so a name after another command's arguments
// still runs (sh -c 'nice mysql -pX'); starts reads it.
func isPrintedInScript(s string, lines *quoteScan, starts *commandStart, p int) bool {
	return lines.sep <= lines.start && runsScript(s, lines.sep, lines.start) && !starts.at(s, lines.inner(), p)
}

// nestedScan finds the commands that run on their own in an argument of a
// segment's command, for names at increasing positions, reading the segment
// once at each depth.
type nestedScan struct {
	// args are the segment, the quoted string or command substitution in
	// it that the last name was in, the one in that, and so on.
	args []nestedArg
}

// nestedArg is the text of a segment, a quoted string or a command
// substitution, which a command can run in.
type nestedArg struct {
	kind argKind
	// from and to bound the text, and quote is the length of the quote
	// around a quoted string's.
	from, to, quote int
	// scan finds the quoted strings and substitutions in it, and starts
	// whether a name in it starts a command.
	scan   quoteScan
	starts commandStart
	// readEnd is where the last command read in it ends.
	readEnd int
	// path records whether a quoted string's text is a program path:
	// 0 when not yet known, 1 when it is, 2 when not.
	path int
}

// argKind is what a nestedArg holds.
type argKind int

const (
	// segmentArg is the segment of the command whose arguments are read.
	segmentArg argKind = iota
	// scriptArg is a quoted argument that may run commands
	// (sh -c 'mysql -pX'), or text (mysql -e 'select 1').
	scriptArg
	// textArg is a quoted string inside a quoted argument, such as a
	// string in its SQL (mysql -e 'select "x"'), except the script of a
	// shell's -c option (sh -c 'bash -c "mysql -pX"').
	textArg
	// substitutionArg is a command substitution, which only runs
	// commands.
	substitutionArg
)

// newNestedScan returns a scan of the segment of a command whose name ends
// at i, in a quoted string that closes at closeAt and ends at stringEnd
// (both -1 when it is in none).
func newNestedScan(s string, i, closeAt, stringEnd int) nestedScan {
	return nestedScan{args: []nestedArg{{
		kind: segmentArg,
		from: i,
		to:   len(s),
		scan: quoteScan{s: s, i: i, skip: closeAt, skipEnd: stringEnd, substitutions: true},
	}}}
}

// rest returns the rest of a command of f at cmd that runs on its own in an
// argument of the segment's command, at any depth: a quoted one (sh -c
// 'mysql -pX'), a command substitution (mysql -h $(cat h; mysql -pX)), or
// one in those (mysql -e $(sh -c 'mysql -pX')). The rest ends with that
// argument. A quoted program path ('"/usr/bin/mysql" -pX') goes on after
// its string, which closes at skip and ends at skipEnd; otherwise those are
// closeAt and stringEnd, where the string the command is quoted in closes
// and ends. It is false when the name is in no such argument or does not
// start a command in it.
func (n *nestedScan) rest(f cliPasswordFlag, s string, cmd []int, closeAt, stringEnd int) (rest string, skip, skipEnd int, ok bool) {
	p := cmd[0]
	n.descend(s, p)
	pattern := segmentRestPattern(s, closeAt)
	arg := &n.args[len(n.args)-1]
	if arg.isProgramPath(f, s) {
		parent := &n.args[len(n.args)-2]
		end := arg.to + arg.quote
		if !parent.runsAt(s, arg.from-arg.quote) {
			return "", 0, 0, false
		}
		rest = s[cmd[1] : end+len(segmentMatch(pattern, s[end:max(parent.to, end)]))]
		parent.readEnd = cmd[1] + len(rest)
		return rest, arg.to, end, true
	}
	if !arg.runsAt(s, p) {
		return "", 0, 0, false
	}
	rest = segmentMatch(pattern, s[cmd[1]:max(arg.to, cmd[1])])
	arg.readEnd = cmd[1] + len(rest)
	return rest, closeAt, stringEnd, true
}

// descend leaves the arguments that end before p and enters the quoted
// strings and substitutions that p is in.
func (n *nestedScan) descend(s string, p int) {
	for len(n.args) > 1 && n.args[len(n.args)-1].to <= p {
		n.args = n.args[:len(n.args)-1]
	}
	for {
		arg := &n.args[len(n.args)-1]
		child := nestedArg{}
		if end, quoted := arg.scan.stringAt(p); quoted {
			child.kind, child.quote = scriptArg, arg.scan.quoteLen()
			child.from, child.to = arg.scan.inner(), end-child.quote
			if !arg.runsString(s) {
				child.kind = textArg
			}
			// A shell's -c script is read as a substitution is, as on
			// the top level, so a name after another command's
			// arguments runs (sh -c 'case x in x) mysql -pX;; esac').
			child.starts.lenient = runsScript(s, arg.from, arg.scan.start)
		} else if start, end, ok := arg.scan.substitutionAt(p); ok {
			child.kind, child.from, child.to = substitutionArg, start+2, end-1
			child.starts.lenient = true
		} else {
			return
		}
		child.scan = quoteScan{s: s[:child.to], i: child.from, substitutions: true}
		n.args = append(n.args, child)
	}
}

// isProgramPath reports whether the argument is a quoted string whose text
// is the path of a program f defines. It reads the text once.
func (a *nestedArg) isProgramPath(f cliPasswordFlag, s string) bool {
	if a.path == 0 {
		a.path = 2
		if (a.kind == scriptArg || a.kind == textArg) && f.isProgramPath(s[a.from:a.to]) {
			a.path = 1
		}
	}
	return a.path == 1
}

// runsAt reports whether a command name at p, directly in the argument,
// starts a command there and is not in the rest of one read before.
func (a *nestedArg) runsAt(s string, p int) bool {
	switch a.kind {
	case scriptArg, substitutionArg:
		return p >= a.readEnd && a.starts.at(s, a.from, p)
	case segmentArg, textArg:
	}
	return false
}

// runsString reports whether the quoted string the argument's scan is at
// may run commands: any in a segment or a substitution, and in a quoted
// argument the script of a shell's -c option (sh -c 'bash -c "mysql -pX"')
// or a string where a command starts (sh -c 'ssh db "mysql -pX"').
func (a *nestedArg) runsString(s string) bool {
	switch a.kind {
	case segmentArg, substitutionArg:
		return true
	case scriptArg:
		return runsScript(s, a.from, a.scan.start) || a.runsAt(s, a.scan.start)
	case textArg:
	}
	return false
}

// runsScript reports whether the quoted string at start, in an argument
// whose text starts at from, follows a shell's -c option (sh -c, bash -lc),
// which runs it as commands.
func runsScript(s string, from, start int) bool {
	before := strings.TrimRight(s[from:start], " \t")
	word := before[max(0, len(before)-maxScriptOption):]
	if i := strings.LastIndexAny(word, " \t"); i >= 0 {
		word = word[i+1:]
	} else if len(word) < len(before) {
		return false
	}
	return scriptOption.MatchString(word)
}

// maxScriptOption bounds the length of the -c option runsScript reads.
const maxScriptOption = 8

// scriptOption is a shell's -c option, alone or with others (-lc, -ce).
var scriptOption = regexp.MustCompile(`^-[a-z]*c[a-z]*$`)

// segmentRest returns the rest of the pipeline segment of a command whose
// name ends at i, in a quoted string that closes at closeAt and ends at
// stringEnd (both -1 when it is in none); lines found that string, and paths
// records whether its text is a program path. A command
// quoted with its arguments (sh -c 'mysql --version'; ssh -p2222 host) ends
// with the string; a quoted program path ("/usr/bin/mysql" -pX, or
// "/opt/mysql tools/bin/mysql" -pX, where the name first matched a
// directory) goes on after it.
func (f cliPasswordFlag) segmentRest(s string, lines *quoteScan, paths *programPathMemo, i, closeAt, stringEnd int) string {
	pattern := segmentRestPattern(s, closeAt)
	switch {
	case closeAt < i:
		return segmentMatch(pattern, s[i:])
	case paths.blankAfter(s, lines.inner(), i, closeAt) && !paths.isProgramPath(f, s, lines, closeAt):
		return segmentMatch(pattern, s[i:closeAt])
	default:
		return s[i : stringEnd+len(segmentMatch(cliSegmentRest, s[stringEnd:]))]
	}
}

// segmentMatch returns the rest of a pipeline segment that pattern matches
// at the start of text, carried on past each command substitution that
// pattern ended inside, at a ";;" of a case statement
// ($(case x in x) printf h;; esac)).
func segmentMatch(pattern *regexp.Regexp, text string) string {
	n := len(pattern.FindString(text))
	if !strings.Contains(text[:n], caseKeyword) {
		return text[:n]
	}
	q := quoteScan{s: text, substitutions: true}
	for n < len(text) {
		if _, quoted := q.stringAt(n); quoted || n <= q.subStart || n >= q.subEnd {
			break
		}
		n = q.subEnd + len(pattern.FindString(text[q.subEnd:]))
	}
	return text[:n]
}

// programPathMemo records whether the text of the quoted string that a
// segment's command was last found in is a program path, and where its last
// blank is, so a string with many commands (sh -c "mysql -e x; mysql -e y;
// ...", sh -c 'mysql;mysql;...') is read once.
type programPathMemo struct {
	// from is one past where the text starts, 0 when nothing is recorded.
	from int
	path bool
	// to is one past where the text with its last blank at blank ends, 0
	// when nothing is recorded.
	to, blank int
}

// blankAfter reports whether s[i:to] holds a blank, where s[from:to] is the
// text of the quoted string i is in; only that text is searched, once.
func (m *programPathMemo) blankAfter(s string, from, i, to int) bool {
	if m.to != to+1 {
		m.to, m.blank = to+1, strings.LastIndexAny(s[from:to], " \t")
		if m.blank >= 0 {
			m.blank += from
		}
	}
	return m.blank >= i
}

// isProgramPath reports whether the text of the quoted string that lines
// last found, which closes at to, is the path of a program f defines. A path
// with a blank ("mysql tools/bin/mysql" -pX, nice "/opt/mysql tools/mysql"
// -pX) is not one when the string is a shell's -c script, whose command
// ends with it (sh -c 'mysql bin/mysql' -p2222).
func (m *programPathMemo) isProgramPath(f cliPasswordFlag, s string, lines *quoteScan, to int) bool {
	if from := lines.inner(); m.from != from+1 {
		m.from, m.path = from+1, f.isProgramPath(s[from:to])
		if m.path && strings.ContainsAny(s[from:to], " \t") {
			m.path = lines.sep > lines.start || !runsScript(s, lines.sep, lines.start)
		}
	}
	return m.path
}

// untilUnmatchedParen returns rest up to its first ")" that closes nothing
// opened in it: the end of a command substitution or subshell the command
// is in (ssh $(mysql -Nse 'select host') -p2222, where -p2222 is ssh's
// port). rest[skip:skipEnd] is the closing quote of the string the command
// is quoted in, which opens nothing ("/usr/bin/mysql" -e "select 1)" -pX).
func untilUnmatchedParen(rest string, skip, skipEnd int) string {
	depth := 0
	var cases caseMemo
	for i := 0; i < len(rest); i++ {
		if i == skip {
			i = skipEnd - 1
			continue
		}
		if end, quoteLen := quotedPiece(rest, i); quoteLen > 0 {
			i = end - 1
			continue
		}
		if isSubstitutionStart(rest, i) {
			// A substitution closes what it opens, also the patterns of
			// a case statement in it.
			if n := cases.substitutionLen(rest, i); n > 0 {
				i += n - 1
				continue
			}
		}
		switch rest[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return rest[:i]
			}
			depth--
		}
	}
	return rest
}

// notProgramPath matches an option, a path argument or a shell operator,
// which a program path holds none of.
var notProgramPath = regexp.MustCompile("[ \t](?:-|/|~/|\\.\\.?/)|[;|&<>`\r\n]")

// isProgramPath reports whether inside, the inside of a quoted string that
// holds a client name, names a program f defines ("mysql"), or is its path,
// also with a blank in a directory ("/opt/mysql tools/bin/mysql",
// "mysql tools/bin/mysql"), rather than a command with its arguments
// (sh -c 'mysql --execute=/bin/mysql'): it holds no option or shell
// operator, and its last path component starts with the name (mysql,
// mysql.exe).
func (f cliPasswordFlag) isProgramPath(inside string) bool {
	sep := strings.LastIndexAny(inside, `/\`)
	if strings.ContainsAny(inside[sep+1:], " \t") || notProgramPath.MatchString(inside) {
		return false
	}
	name := inside[sep+1:]
	loc := f.command.FindStringIndex(name)
	return loc != nil && loc[0] == 0 && isCommandToken(name, loc[0], loc[1])
}

// isCommandToken reports whether the client name at s[start:end] is a
// program name, alone or at the end of a path (mysql, /usr/bin/mysql,
// mysql.exe, C:\tools\mysql.exe), rather than part of a host or file name
// or a directory (mysql.example.com, ~/.ssh/mysql.pem, user@mysql, mysql:8,
// mysql-client, --name=mysql, /opt/mysql/bin/ssh, C:\mysql\ssh.exe).
func isCommandToken(s string, start, end int) bool {
	if start > 0 && strings.IndexByte(".-@:=%+", s[start-1]) >= 0 && !hasExecOptionBefore(s, start) {
		return false
	}
	if end == len(s) {
		return true
	}
	switch c := s[end]; {
	case c == '.':
		ext := end + len(".exe")
		return ext <= len(s) && strings.EqualFold(s[end:ext], ".exe") && (ext == len(s) || !isWordByte(s[ext]))
	case strings.IndexByte("-@:=/+", c) >= 0:
		return false
	case c == '\\':
		// A directory of a Windows path (C:\mysql\ssh.exe).
		return end+1 == len(s) || !isWordByte(s[end+1])
	}
	return true
}

// hasExecOptionBefore reports whether the word before start is a long option
// whose argument, joined to it, is the command to run
// (docker run --entrypoint=mysql, env --split-string=mysql).
func hasExecOptionBefore(s string, start int) bool {
	return strings.HasSuffix(s[:start], "--entrypoint=") || strings.HasSuffix(s[:start], "--split-string=")
}

// isWordByte reports whether c is an ASCII letter, digit or underscore.
func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// segmentRestPattern returns the pattern for the rest of the segment of a
// command whose enclosing string closes at closeAt, or -1.
func segmentRestPattern(s string, closeAt int) *regexp.Regexp {
	if closeAt >= 0 && s[closeAt] == '"' {
		return cliEscapedSegmentRest
	}
	return cliSegmentRest
}

// commandStart reports whether a command name is run as a command in an
// argument (sh -c 'mysql -pX', sh -c 'cd app && sudo mysql -pX',
// sh -c 'if x; then mysql -pX; fi') rather than being text in it (the SQL of
// mysql -e 'select mysql -pX'). It reads each argument once for names at
// increasing positions in it.
type commandStart struct {
	commandWords

	// from is where the argument's text starts, i where reading goes on:
	// the start of a word that may not have ended yet, and j how far that
	// word was read.
	from, i, j int
	// lenient reads the text of a command substitution, where a client
	// name after any word is run (mysql -h mysql -pX), except after the
	// name of a command that prints its arguments (echo mysql -pX).
	lenient bool
	// parens counts the parentheses open in a script or a substitution
	// (a substitution, a subshell), across its shell operators, and cases
	// holds the case statements open inside each of them.
	parens int
	cases  [caseLevels]caseLevel
}

// commandWords is what the words read since a command could start tell of
// it.
type commandWords struct {
	// wrapper is the command wrapper the words so far run (sudo), named as
	// a command rather than as an argument (docker exec ssh), optionArg is
	// set after its option that takes the next word, and host after the
	// destination of ssh or the container of a container client, which
	// runs the words after it. sub is set after a container client's
	// subcommand that runs a command (docker exec, kubectl exec).
	wrapper   string
	optionArg bool
	host      bool
	sub       bool
	// execArg is set with optionArg when that option's argument is the
	// command to run (env -S 'mysql -pX', docker run --entrypoint mysql).
	execArg bool
	// redirectArg is set after a redirection operator whose target is the
	// next word (2> /dev/null), and funcName after the function keyword,
	// whose name is the next word.
	redirectArg bool
	funcName    bool
	// command is set after the name of a command, and text after a word
	// that is no wrapper, option or assignment, or in a substitution after
	// the name of a command that prints its arguments.
	command bool
	text    bool
}

// at reports whether a command name at p, in the argument of s whose text
// starts at from, is run as a command: every word before it since the last
// shell operator is a shell keyword, a wrapper with its options, or an
// assignment.
func (c *commandStart) at(s string, from, p int) bool {
	if c.from != from || p < c.i {
		*c = commandStart{from: from, i: from, j: from, lenient: c.lenient}
	}
	for c.i < p {
		if c.j == c.i {
			r, size := utf8.DecodeRuneInString(s[c.i:])
			switch {
			case unicode.IsSpace(r):
				c.i += size
				c.j = c.i
				continue
			case size == 1 && isCommandSeparator(s, c.i):
				c.separate(s, from)
				continue
			}
		}
		for c.j < p {
			// A quoted string is part of the word (sudo -u "my user").
			if end, quoteLen := quotedPiece(s[:p], c.j); quoteLen > 0 {
				c.j = end
				continue
			}
			r, size := utf8.DecodeRuneInString(s[c.j:])
			if unicode.IsSpace(r) || size == 1 && isCommandSeparator(s, c.j) {
				break
			}
			c.j += size
		}
		if c.j >= p {
			return c.runsBefore(s[c.i:p])
		}
		c.word(s[c.i:c.j])
		c.i = c.j
	}
	return c.runs()
}

// runsBefore reports whether a command name right after part, the start of
// a word that may go on past it, is run: after the directory of a program
// path of any length (/usr/bin/mysql), or after a word that would let it
// run on its own. A longer part is no command's.
func (c *commandStart) runsBefore(part string) bool {
	if ok, _ := redirection(part); ok {
		// The name is in the redirection's target (2>/var/log/mysql).
		return false
	}
	switch {
	case c.text:
		return false
	case strings.HasSuffix(part, "/"):
		return c.runs()
	case len(part) > maxCommandPart:
		return false
	case runsJoinedArgument(c.wrapper, part):
		return true
	}
	next := *c
	next.word(part)
	return next.runs()
}

// maxCommandPart bounds the part of a word before a command name that
// runsBefore reads.
const maxCommandPart = 64

// isCommandSeparator reports whether s[i] is a shell operator after which a
// command starts (;, |, &, or the ( of a subshell), and not part of a
// redirection (2>&1, &>log, >|f).
func isCommandSeparator(s string, i int) bool {
	switch s[i] {
	case ';', '(':
		return true
	case '&', '|':
		return !isRedirectionOperator(s, i)
	}
	return false
}

// isRedirectionOperator reports whether the & or | at s[i] is part of a
// redirection operator (>&, <&, &>, >|), where the < or > is not escaped
// (key\>& mysql runs mysql).
func isRedirectionOperator(s string, i int) bool {
	before := i > 0 && (s[i-1] == '>' || s[i-1] == '<' && s[i] == '&') && !isEscaped(s, i-1)
	return before || s[i] == '&' && strings.HasPrefix(s[i+1:], ">")
}

// isEscaped reports whether an odd number of backslashes comes right before
// s[i].
func isEscaped(s string, i int) bool {
	n := 0
	for i > n && s[i-n-1] == '\\' {
		n++
	}
	return n%2 == 1
}

// redirection reports whether word is a redirection (>f, 2>/dev/null, 2>&1,
// &>log, <in), and whether it is only the operator, whose target is the next
// word (2> /dev/null).
func redirection(word string) (ok, operatorOnly bool) {
	i := 0
	for i < len(word) && '0' <= word[i] && word[i] <= '9' {
		i++
	}
	if i == 0 && strings.HasPrefix(word, "&>") {
		i = 1
	}
	if i == len(word) || word[i] != '<' && word[i] != '>' {
		return false, false
	}
	for i < len(word) && strings.IndexByte("<>&|", word[i]) >= 0 {
		i++
	}
	return true, i == len(word)
}

// separate starts a command after the shell operator at s[c.i], in the
// argument whose text starts at from, keeping the case statements and the
// parentheses open in a script or a substitution.
func (c *commandStart) separate(s string, from int) {
	next := commandStart{from: from, i: c.i + 1, j: c.i + 1, lenient: c.lenient, parens: c.parens, cases: c.cases}
	level := next.level()
	switch op := s[c.i]; {
	case !c.lenient:
	case op == '(' && level.pos == casePattern && (c.i == from || !isSubstitutionStart(s, c.i-1)):
		// The optional "(" before a pattern (case x in (x) mysql -pX;; esac),
		// not a substitution in one (case x in $(echo x)) ...).
	case op == '(' && !isEscaped(s, c.i):
		// A substitution in a word is part of it, so the words after it
		// go on from the words before (echo $(printf x) mysql -p2222),
		// while a subshell's are a command of their own.
		next.parens++
		if next.parens < caseLevels {
			substitution := c.i > from && isSubstitutionStart(s, c.i-1)
			next.cases[next.parens] = caseLevel{outer: c.commandWords, restore: substitution}
		}
	case level.open > 0 && c.i > from && s[c.i-1] == ';' && (op == ';' || op == '&'):
		// ;; ;& and ;;& end a case arm, and a pattern follows.
		level.pos = casePattern
	}
	*c = next
}

// caseLevel is what is open of the case statements read inside one pair of
// parentheses, or outside all of them.
type caseLevel struct {
	// open counts the case statements open, and pos is where the reading
	// is in the head or a pattern of the last one.
	open int
	pos  caseState
	// restore is set inside a substitution, after whose ")" the words read
	// before it, outer, go on.
	outer   commandWords
	restore bool
}

// caseLevels bounds the parentheses whose case statements are read apart;
// deeper ones share the last level.
const caseLevels = 4

// level returns the case statements open inside the innermost parentheses.
func (c *commandStart) level() *caseLevel {
	return &c.cases[min(c.parens, caseLevels-1)]
}

// readCase reads word, in a script or a substitution, for its case
// statements and parentheses, and reports whether it was read whole: part
// of a case statement's head or pattern, or the end of a pattern or of a
// substitution, after which the words before the substitution go on. The
// unescaped ")" that ends a pattern starts a command
// (case x in x) mysql -pX;; esac), while one that closes a substitution or
// a subshell (echo $(printf x) mysql -p2222), and an escaped one
// (echo x\) mysql -p2222), are text. A word holds no "(" of its own, which
// starts another word, so the parts between its ")" are read in turn.
func (c *commandStart) readCase(word string) bool {
	inCase, ended, restored := false, false, false
	for {
		i, part := closingParen(word), word
		if i >= 0 {
			part = word[:i]
		}
		// After a substitution, the rest of the word goes on the word
		// before it, which was read (case $(printf ech)o in echo) ...).
		if !restored && c.casePart(part, i < 0) {
			inCase = true
		}
		if i < 0 {
			break
		}
		switch level := c.level(); {
		case level.pos == casePattern:
			level.pos, ended = caseNone, true
		case c.parens > 0:
			if c.parens < caseLevels && level.restore {
				c.commandWords, restored = level.outer, true
			}
			c.parens--
		}
		word = word[i+1:]
	}
	if ended {
		*c = commandStart{from: c.from, i: c.i, j: c.j, lenient: true, parens: c.parens, cases: c.cases}
	}
	return inCase || ended || restored
}

// casePart reads part, a word or the part of one before a ")", for the head
// or a pattern of the case statements open in the innermost parentheses,
// and reports whether it belongs there. whole is set when part is the whole
// rest of the word.
func (c *commandStart) casePart(part string, whole bool) bool {
	level := c.level()
	switch {
	case part == "" || level.open == 0:
		return false
	case level.pos == caseWord:
		level.pos = caseSubject
	case level.pos == caseSubject && part == "in" && whole:
		level.pos = casePattern
	case level.pos == caseSubject:
		level.pos = caseNone
		return false
	case part == esacKeyword && (level.pos == casePattern || c.fresh()):
		level.open--
		level.pos, c.command = caseNone, false
	case level.pos != casePattern:
		return false
	}
	return true
}

// closingParen returns the index of the first unescaped ")" of word outside
// its quoted strings, or -1 when none is.
func closingParen(word string) int {
	for i := 0; i < len(word); i++ {
		if end, quoteLen := quotedPiece(word, i); quoteLen > 0 {
			i = end - 1
			continue
		}
		switch word[i] {
		case '\\':
			i++
		case ')':
			return i
		}
	}
	return -1
}

// fresh reports whether no word has been read since the command could start.
func (c *commandStart) fresh() bool {
	return !c.text && !c.command && c.wrapper == "" && !c.optionArg && !c.redirectArg && !c.funcName
}

// runs reports whether a command name after the words read is run.
func (c *commandStart) runs() bool {
	return !c.text && (c.execArg || !c.optionArg && !c.redirectArg && !c.funcName && (!hasDestination(c.wrapper) || c.host))
}

// word reads one word of the argument.
func (c *commandStart) word(word string) {
	switch {
	case c.lenient && c.readCase(word):
	case c.lenient && c.fresh() && word == caseKeyword:
		level := c.level()
		level.open, level.pos = level.open+1, caseWord
		c.command = true
	case c.text:
	case c.redirectArg:
		c.redirectArg = false
	case c.funcName:
		c.funcName = false
	case c.fresh() && (word == ")" || word == "function"):
		// A function definition, whose body runs commands
		// (f() { mysql -pX; }, function f { mysql -pX; }): the ")" after
		// the "(" that started this reading, or the keyword before the
		// function's name.
		c.funcName = word == "function"
	case isRedirection(word):
		// A redirection runs nothing (2>/dev/null mysql -pX), and an
		// operator alone takes the next word as its target (> out).
		_, c.redirectArg = redirection(word)
	case c.optionArg && c.execArg && c.wrapper == envCommand:
		c.splitString(word)
	case c.optionArg:
		c.optionArg, c.command, c.execArg = false, c.execArg, false
	case c.wrapper != "" && strings.HasPrefix(word, "-"):
		// An option of the wrapper (sudo -E, env -i), some of which take
		// the next word as their argument (sudo -u root), and a few the
		// command to run (env -S mysql, env -Ssudo).
		if line, ok := joinedSplitString(c.wrapper, word); ok {
			c.splitString(line)
			return
		}
		c.optionArg = takesArgument(c.wrapper, word)
		c.execArg = c.optionArg && runsArgument(c.wrapper, word)
	case isContainerClient(c.wrapper) && !c.sub:
		// The subcommand of a container client: exec and run run a
		// command in a container (docker exec db mysql -pX), and the
		// others run none (docker logs mysql). Any other word comes
		// before it: container, compose, or the value of a global option
		// (kubectl --as alice exec).
		switch {
		case slices.Contains(containerRunSubcommands, word):
			c.sub = true
		case slices.Contains(containerTextSubcommands, word):
			c.text = true
		}
	case hasDestination(c.wrapper) && !c.host:
		// The destination of ssh (ssh db mysql -pX), or the container or
		// image a command runs in (docker exec db mysql -pX), even one
		// named like a command, a keyword or a wrapper (ssh echo mysql
		// -pX, docker exec mysql ssh -p2222 h).
		c.host = true
	case isShellKeyword(word):
		c.wrapper, c.host, c.sub, c.command = "", false, false, false
	case !c.command && isCommandWrapper(programName(word)):
		c.wrapper, c.host = programName(word), false
	default:
		if isAssignment(word) {
			return
		}
		// In a substitution, a word after the name of a command is its
		// argument, so only the name prints the rest (echo mysql -pX,
		// not docker exec echo mysql -pX).
		c.text = !c.lenient || !c.command && isPrintCommand(programName(word))
		c.command = true
	}
}

// splitString reads arg, the shell word env -S splits into words, as the
// start of the command env runs, which goes on after it
// (env -S sudo ssh db mysql -pX, env -S 'sudo -u x' mysql -pX). The shell
// removes the word's quotes, and env splits what is left as the shell
// would, so a quoted string in it stays one word
// (env -S 'env A="x ssh"' mysql -pX).
func (c *commandStart) splitString(arg string) {
	*c = commandStart{from: c.from, i: c.i, j: c.j, lenient: c.lenient, parens: c.parens, cases: c.cases}
	shellWords(arg, func(line string) {
		shellWords(line, c.word)
	})
}

// shellWords calls f with each word of line as the shell splits it: blanks
// outside quotes end a word, and quotes and escaping backslashes are
// dropped (a "b c" d\ e gives a, b c and d e).
func shellWords(line string, f func(string)) {
	var word []byte
	inWord, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0 && ch == quote:
			quote = 0
		case quote != '\'' && ch == '\\' && i+1 < len(line):
			i++
			word, inWord = append(word, line[i]), true
		case quote != 0:
			word = append(word, ch)
		case ch == '\'' || ch == '"':
			quote, inWord = ch, true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if inWord {
				f(string(word))
				word, inWord = word[:0], false
			}
		default:
			word, inWord = append(word, ch), true
		}
	}
	if inWord {
		f(string(word))
	}
}

// joinedSplitString returns the string joined to option, an option of
// wrapper, when it is env's -S with its argument (--split-string=sudo,
// -Ssudo, -iSsudo).
func joinedSplitString(wrapper, option string) (string, bool) {
	if wrapper != envCommand {
		return "", false
	}
	if line, ok := strings.CutPrefix(option, "--split-string="); ok {
		return line, true
	}
	if strings.HasPrefix(option, "--") {
		return "", false
	}
	i := strings.IndexAny(option[1:], wrapperArgOptions[envCommand]) + 1
	if i == 0 || option[i] != 'S' || i == len(option)-1 {
		return "", false
	}
	return option[i+1:], true
}

// programName returns the program a word names, without its quotes, its
// directory or a .exe ("/usr/bin/ssh", /opt/build=debug/bin/env,
// C:\tools\ssh.exe), or the word itself when it is an assignment.
func programName(word string) string {
	if isAssignment(word) {
		// An assignment (FOO=/usr/bin/ssh), not a program.
		return word
	}
	if strings.ContainsAny(word, `"'`) {
		word = wordQuotes.Replace(word)
	}
	name := word[strings.LastIndexAny(word, `/\`)+1:]
	if ext := len(name) - len(".exe"); ext > 0 && strings.EqualFold(name[ext:], ".exe") {
		return name[:ext]
	}
	return name
}

// wordQuotes removes the quotes of a word, also escaped ones of a command in
// a double-quoted string (sh -c "\"/usr/bin/ssh\" mysql -p2222").
var wordQuotes = strings.NewReplacer(`\"`, "", `\'`, "", `"`, "", `'`, "")

// isRedirection reports whether word is a redirection.
func isRedirection(word string) bool {
	ok, _ := redirection(word)
	return ok
}

// isPrintCommand reports whether word prints its arguments rather than
// running them.
func isPrintCommand(word string) bool {
	return word == "echo" || word == "printf"
}

// isCommandWrapper reports whether word runs the command after it (and its
// own options, and for ssh its destination), as sshpass runs ssh:
// sshpass -e ssh mysql -p2222, where mysql is ssh's destination.
func isCommandWrapper(word string) bool {
	switch word {
	case "sudo", execCommand, envCommand, "time", "nohup", "command", "sshpass", sshCommand:
		return true
	}
	return isContainerClient(word)
}

// isContainerClient reports whether word is a client that runs a command in
// a container after its subcommand, options and container
// (docker exec db mysql -pX, kubectl exec -it db -- mysql -pX).
func isContainerClient(word string) bool {
	switch word {
	case "docker", "podman", "nerdctl", "kubectl":
		return true
	}
	return false
}

// containerRunSubcommands are the subcommands of container clients that run
// a command in a container, and containerTextSubcommands those that run
// none.
var (
	containerRunSubcommands  = []string{execCommand, "run", "create", "debug"}
	containerTextSubcommands = []string{
		"annotate", "api-resources", "api-versions", "apply", "attach", "auth", "autoscale", "build", "builder", "buildx", "certificate", "cluster-info", "commit", "completion", "config", "context", "cordon", "cp", "delete", "describe", "diff", "drain", "edit", "events", "explain", "export", "expose", "get", "history", "image", "images", "import", "info", "inspect", "kill", "kustomize", "label", "load", loginCommand, "logout", "logs", "manifest", "network", "node", "patch", "pause", "plugin", "port", "port-forward", "proxy", "ps", "pull", "push", "rename", "replace", "restart", "rm", "rmi", "rollout", "save", "scale", "search", "service", "set", "stack", "start", "stats", "stop", "swarm", "system", "tag", "taint", "top", "trust", "uncordon", "unpause", "update", "version", "volume", "wait",
	}
)

// hasDestination reports whether wrapper runs the words after a
// destination: ssh's host, or a container client's container.
func hasDestination(wrapper string) bool {
	return wrapper == sshCommand || isContainerClient(wrapper)
}

// isShellKeyword reports whether word is a shell reserved word after which
// a command starts (if true; then mysql -pX; fi).
func isShellKeyword(word string) bool {
	switch word {
	case "if", "then", "elif", "else", "while", "until", "do", "!", "{":
		return true
	}
	return false
}

// wrapperArgOptions are the short options of each wrapper that take an
// argument, and wrapperLongArgOptions the long ones.
var (
	wrapperArgOptions = map[string]string{
		"sudo": "CDghpRrTtUu", envCommand: "CSu", "time": "fo", execCommand: "a", "sshpass": "dfPp", sshCommand: "BbcDEeFIiJLlmOoPpQRSWw",
		"docker": containerArgOptions, "podman": containerArgOptions, "nerdctl": containerArgOptions, "kubectl": containerArgOptions,
	}
	wrapperLongArgOptions = []string{
		"chdir", "chroot", "close-from", "command-timeout", "format", "group", "host", "other-user", "output", "prompt", "role", "split-string", "type", "unset", "user",
		// container clients
		"as", "as-group", "as-uid", "cache-dir", "certificate-authority", "client-certificate", "client-key", "log-dir", "log-file", "password", "profile", "profile-output", "request-timeout", "server", "tls-server-name", "tlscacert", "tlscert", "tlskey", "token", "username",
		"add-host", "attach", "cap-add", "cap-drop", "cgroupns", "cidfile", "cluster", "config", "container", "context", "cpu-shares", "cpus", "detach-keys", "device", "dns", "entrypoint", "env", "env-file", "filename", "gpus", "hostname", "ip", "ipc", "kubeconfig", "label", "link", "log-driver", "log-level", "log-opt", "memory", "mount", "name", "namespace", "network", "network-alias", "pid", "platform", "publish", "pull", "restart", "runtime", "security-opt", "shm-size", "tmpfs", "ulimit", "volume", "volumes-from", "workdir",
	}
)

// containerArgOptions are the short options of container clients, before
// and after their subcommand, that take an argument (docker -H host,
// docker run -h mongo -p 27017:27017, kubectl exec -c app -n prod).
const containerArgOptions = "Hacefhlmnpsuvw"

// takesArgument reports whether option, an option of wrapper, takes the next
// word as its argument: -u and -Eu do, -n and -uroot (its argument glued on)
// do not.
func takesArgument(wrapper, option string) bool {
	if name, ok := strings.CutPrefix(option, "--"); ok {
		return slices.Contains(wrapperLongArgOptions, name)
	}
	for i := 1; i < len(option); i++ {
		if strings.IndexByte(wrapperArgOptions[wrapper], option[i]) >= 0 {
			return i == len(option)-1
		}
	}
	return false
}

// runsJoinedArgument reports whether part, the start of a word, is an option
// of wrapper whose argument, joined to it after "=", is the command to run,
// with the directory of its path (docker run --entrypoint=/usr/bin/mysql).
func runsJoinedArgument(wrapper, part string) bool {
	option, dir, ok := strings.Cut(part, "=")
	return ok && (dir == "" || strings.HasSuffix(dir, "/")) && strings.HasPrefix(option, "--") && runsArgument(wrapper, option)
}

// runsArgument reports whether option, an option of wrapper that takes an
// argument, runs it as the command: env -S, and a container's --entrypoint.
func runsArgument(wrapper, option string) bool {
	if wrapper == envCommand {
		return option == "--split-string" || !strings.HasPrefix(option, "--") && strings.HasSuffix(option, "S")
	}
	return isContainerClient(wrapper) && option == "--entrypoint"
}

// isAssignment reports whether word, without its quotes, is an
// environment assignment, as env reads one after the shell removes them
// (env "FOO=/usr/bin/ssh" mysql -pX).
func isAssignment(word string) bool {
	if strings.ContainsAny(word, `"'`) {
		word = wordQuotes.Replace(word)
	}
	eq := strings.IndexByte(word, '=')
	return eq > 0 && isShellName(word[:eq])
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
// argument (the SQL of mysql -e 'select -p1') is text, not a flag, and one
// inside a command substitution (mysql -e $(true; ssh -p2222 h)) belongs to
// another command, read on its own if f defines it. stringEnd is where the
// quoted string holding the command ends, or -1.
func (f cliPasswordFlag) appendFlags(regions []taggedRegion, s string, quotes *quoteScan, rest string, stringEnd int) []taggedRegion {
	base := quotes.i
	for pos := 0; pos < len(rest); {
		loc := f.flag.FindStringSubmatchIndex(rest[pos:])
		if loc == nil {
			break
		}
		for k := range loc {
			if loc[k] >= 0 {
				loc[k] += pos
			}
		}
		r, ok := taggedRegion{}, false
		if _, quoted := quotes.stringAt(base + loc[0]); !quoted {
			if _, _, inner := quotes.substitutionAt(base + loc[0]); !inner {
				r, ok = f.valueRegion(s, base, loc, stringEnd)
			}
		}
		if !ok {
			// A flag that is not read may have taken the next one as its
			// value (-in 'file -pass ' -pass 'pass:x'), so the search goes
			// on from where that value starts.
			pos = max(loc[len(loc)-2], loc[0]+1)
			continue
		}
		regions = append(regions, r)
		// A flag inside the password's word is part of it (-px\ -py),
		// and is not read again.
		pos = max(loc[1], r.end-base)
	}
	return regions
}

// valueRegion returns the password of the flag that loc, a match of f.flag in
// s from base, found.
func (f cliPasswordFlag) valueRegion(s string, base int, loc []int, stringEnd int) (taggedRegion, bool) {
	v, ok := shortFlagValue(s, base, loc, stringEnd)
	if !ok {
		return taggedRegion{}, false
	}
	if f.prefix != "" {
		var ok bool
		if v, ok = v.trimPrefix(s, f.prefix, stringEnd); !ok {
			return taggedRegion{}, false
		}
	}
	return passwordValueRegion(s, v, f.optional, false)
}

// shortFlagValue returns the value of the short password flag that loc, a
// match in s from base, found. The shell passes a quoted flag as if it were
// bare: a value in the string that opens the flag ("-pSummer 2026") runs to
// that string's end, and a flag quoted on its own ("-p" x) must close the
// quote that opens it.
func shortFlagValue(s string, base int, loc []int, stringEnd int) (cliValue, bool) {
	start, end := base+loc[len(loc)-2], base+loc[len(loc)-1]
	if len(loc) < 8 || loc[2] < 0 {
		return shellWordValue(s, start, end, stringEnd), true
	}
	closeFrom, closed := base+loc[4], loc[5] > loc[4]
	pieceEnd, quoteLen := quotedPiece(s, base+loc[2])
	closeAt := pieceEnd - quoteLen
	switch {
	case quoteLen == 0:
		// A quote that closes nothing on its line.
		if closed {
			return cliValue{}, false
		}
		return shellWordValue(s, start, end, stringEnd), true
	case closed && closeAt == closeFrom:
		// A flag quoted on its own ("-p" x, "-p"'x'). In code, "-p"+pw,
		// "-p" + pw and "-p" . $pw join strings, so a value right after the
		// quote must be quoted itself, and a lone operator is none.
		if _, n := quotedPiece(s, start); start == base+loc[5] && n == 0 || isConcatOperator(s[start:end]) {
			return cliValue{}, false
		}
		return shellWordValue(s, start, end, stringEnd), true
	case closed:
		// The quote after the name is the first character of the value
		// ('-p"x y').
		start = closeFrom
	}
	if closeAt <= start {
		// "-p" alone prompts for the password.
		return cliValue{}, false
	}
	return quotedFlagValue(s, base+loc[2], start, pieceEnd, quoteLen, stringEnd), true
}

// isConcatOperator reports whether value is an operator that joins strings
// in code ("-p" + pw, "-p" . $pw, "-p" .. pw).
func isConcatOperator(value string) bool {
	return value == "+" || value == "." || value == ".."
}

// quotedFlagValue returns the value that starts at start, inside the
// string that opens its flag at open and ends at end. When the word goes on
// after the string ("-pabc"def), the shell joins its pieces into one
// password, whose start keeps its quoting ('-p$ecret'"" is literal).
func quotedFlagValue(s string, open, start, end, quoteLen, stringEnd int) cliValue {
	closeAt := end - quoteLen
	if wordEnd := joinedWordEnd(s, open, end, stringEnd); wordEnd > end {
		return cliValue{start: start, end: wordEnd, singleQuoted: s[closeAt] == '\''}
	}
	return cliValue{start: start, end: closeAt, quoted: true, singleQuoted: s[closeAt] == '\''}
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
	wordEnd := shellWordEnd(s, start, stringEnd)
	if wordEnd <= start {
		wordEnd = end
	}
	if strings.HasPrefix(s[start:], "$'") {
		// An ANSI-C quoted string ($'pass:x'), where $ is literal and a
		// backslash escapes a quote ($'a\'b').
		if pieceEnd := ansiCQuoteEnd(s, start); pieceEnd > 0 {
			if end := ansiCWordEnd(s, pieceEnd, stringEnd); end > pieceEnd {
				// The word goes on after the string ($'pass:x\\'y), so it
				// is read from the quote, like 'pass:x'y.
				return cliValue{start: start + 1, end: end}
			}
			return cliValue{start: start + 2, end: pieceEnd - 1, quoted: true, singleQuoted: true}
		}
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

// joinedWordEnd returns the end of the shell word in which a quoted string,
// opened at open, ends at i. A quote right after it that closes the string
// the command is quoted in, which ends at stringEnd, ends the word
// (sh -c "wget '--password=x'"; echo "ok"), and so does a separator of code
// after a string that is an element of code (["mysql", "-px"],
// exec.Command("mysql", "-px",db), {"arg": "--password=x"}), but not one
// that a shell word goes on after (mysql "-pabc",def).
func joinedWordEnd(s string, open, i, stringEnd int) int {
	if i+1 == stringEnd && (s[i] == '"' || s[i] == '\'') || isCodeSeparator(s, open, i) {
		return i
	}
	return ansiCWordEnd(s, i, stringEnd)
}

// isCodeSeparator reports whether s[i] is a comma or closing bracket after a
// string, opened at open, that is an element of code: the first one in a
// bracket, or one after other elements (["mysql", "-px"], f(db, "-px")),
// not a shell word (mysql -h db, "-px",y, mysql -h 'db', "-px",y).
func isCodeSeparator(s string, open, i int) bool {
	if i == len(s) || strings.IndexByte(",]}", s[i]) < 0 {
		return false
	}
	return isCodeElementStart(s[:open])
}

// maxCodeElements bounds how many elements before a string isCodeSeparator
// reads, however long they are (f("mysql", "<a long host>", "-px",db)); one
// after that many is taken for code. Each element is read by the strings
// at most that many elements after it, so the scan stays linear.
const maxCodeElements = 16

// isCodeElementStart reports whether an element of code starts after code:
// after an opening bracket, or after a comma, colon or operator that
// follows other elements back to an opening bracket: strings ("a", 'a'),
// closed brackets and names, joined into expressions by operators
// (f("mysql", db, ...), {"arg": ...}, f("mysql", "db-"+suffix, ...)).
func isCodeElementStart(code string) bool {
	for range maxCodeElements {
		code = strings.TrimRight(code, " \t")
		switch {
		case code == "":
			return false
		case strings.IndexByte("[({", code[len(code)-1]) >= 0:
			return true
		case strings.IndexByte(",:+-*/%", code[len(code)-1]) < 0:
			return false
		}
		code = strings.TrimRight(code[:len(code)-1], " \t")
		if code != "" && strings.IndexByte(")]}", code[len(code)-1]) >= 0 {
			// A closed bracket ends an element of code (f(x), [a]).
			return true
		}
		element, ok := codeElementStart(code)
		if !ok {
			return false
		}
		code = element
	}
	return true
}

// codeElementStart returns the code before the string or name (db, os.Args)
// that code ends with, or false when it ends with neither.
func codeElementStart(code string) (string, bool) {
	if code == "" {
		return "", false
	}
	if last := code[len(code)-1]; last == '"' || last == '\'' {
		escaped := isEscaped(code, len(code)-1)
		for i := len(code) - 2; i >= 0; i-- {
			if code[i] == last && isEscaped(code, i) == escaped {
				if escaped {
					i--
				}
				return code[:i], true
			}
		}
		return "", false
	}
	n := len(code)
	for n > 0 && (code[n-1] == '.' || isWordByte(code[n-1])) {
		n--
	}
	return code[:n], n < len(code)
}

// ansiCQuoteEnd returns the end of the ANSI-C quoted string ($'...') that
// starts at i, after its closing quote, or 0 when it does not close on its
// line. A backslash escapes the character after it ($'a\'b', $'a\\').
func ansiCQuoteEnd(s string, i int) int {
	for j := i + 2; j < len(s); j++ {
		switch s[j] {
		case '\r', '\n':
			return 0
		case '\\':
			if j+1 < len(s) && (s[j+1] == '\r' || s[j+1] == '\n') {
				return 0
			}
			j++
		case '\'':
			return j + 1
		}
	}
	return 0
}

// ansiCWordEnd returns the end of the shell word in which a quoted string
// ends at i. stringEnd is as in shellWordEnd.
func ansiCWordEnd(s string, i, stringEnd int) int {
	return wordRestEnd(s, i, stringEnd, false)
}

// trimPrefix drops prefix from the start of v, also when v is a word whose
// first quoted piece starts with it ('pass:abc'def). stringEnd is where the
// quoted string holding the flag ends, or -1.
func (v cliValue) trimPrefix(s, prefix string, stringEnd int) (cliValue, bool) {
	skip := 0
	if !v.quoted && v.start < v.end && (s[v.start] == '"' || s[v.start] == '\'') {
		skip = 1
	}
	if !strings.HasPrefix(s[v.start+skip:v.end], prefix) {
		return v, false
	}
	v.start += skip + len(prefix)
	if skip == 0 && !v.quoted && v.start < len(s) {
		// The rest is read as a value of its own when it starts with a
		// quote: single quotes make $ literal (pass:'$ecret'), and a
		// string holding a substitution joins the prefix
		// (pass:"$(printf x)"), unless it is the gap after the string the
		// command is quoted in ("openssl enc -pass pass:" + pw).
		if _, quoteLen := quotedPiece(s, v.start); quoteLen > 0 {
			v = shellWordValue(s, v.start, v.start, stringEnd)
		}
	}
	return v, true
}

// shellWordEnd returns the end of the shell word that starts at start. A
// quoted piece is part of the word, and so are an escaped character, a line
// continuation, a command substitution and a parameter expansion. stringEnd
// is where the quoted string holding the command ends, after its closing
// quote, or -1. Commands are often quoted inside other text, as the argument
// of sh -c or as a JSON string: the word ends at the quote that closes that
// string (sh -c "mysql -phunter2"; echo " ok"), and inside a double-quoted
// one an escaped quote opens a quoted piece of the command
// (sh -c "mysql -pabc\"x y\""), unless it closes a string of that command.
// Elsewhere \" is a literal quote (mysql -pabc\"def), and any quoted piece
// joins the word (mysql -pabc"x;y").
func shellWordEnd(s string, start, stringEnd int) int {
	return wordRestEnd(s, start, stringEnd, true)
}

// wordRestEnd returns the end of the shell word that goes on at i, which
// is the word's start when atStart is set, as in shellWordEnd.
func wordRestEnd(s string, i, stringEnd int, atStart bool) int {
	first := i
	for i < len(s) {
		if end, stop := wordPieceEnd(s, i, stringEnd, atStart && i == first); stop {
			return i
		} else if end > i {
			i = end
			continue
		}
		c := s[i]
		switch {
		case c == '\\' && (strings.HasPrefix(s[i+1:], "\n") || strings.HasPrefix(s[i+1:], "\r\n")):
			// A line continuation joins the word to the next line's text
			// (pass:Summer\<newline>2026), and ends it before a blank.
			next := i + 1 + strings.IndexByte(s[i+1:], '\n') + 1
			if next == len(s) || isShellWordEnd(s[next]) {
				return i
			}
			i = next
		case c == '\\' && i+1 < len(s) && s[i+1] != '\r' && s[i+1] != '\n':
			i += 2
		case c == '"' || c == '\'' || isShellWordEnd(c):
			return i
		default:
			i++
		}
	}
	return i
}

// wordPieceEnd returns the end of the quoted piece, command substitution or
// parameter expansion of a shell word that starts at s[i], or i when none
// does, and whether the word ends at i instead, at a quote that closes the
// string holding it, which ends at stringEnd (-1 for none). first marks the
// word's first piece.
func wordPieceEnd(s string, i, stringEnd int, first bool) (end int, stop bool) {
	inString := i < stringEnd
	if inString && closesString(s, i, stringEnd) {
		return i, true
	}
	if strings.HasPrefix(s[i:], "$'") && (!inString || s[stringEnd-1] == '"') {
		// An ANSI-C quoted piece, in which a backslash escapes a quote
		// (pass:$'abc\' def').
		if end := ansiCQuoteEnd(s, i); end > 0 && (!inString || end < stringEnd) {
			return end, false
		}
	}
	pieceEnd, quoteLen := quotedPiece(s, i)
	if strings.HasPrefix(s[i:], `\"`) && !first {
		// Elsewhere than in a double-quoted string, \" is a literal
		// quote (mysql -pabc\"def).
		if !inString || s[stringEnd-1] != '"' {
			quoteLen = 0
		} else if closesInnerString(s, i, stringEnd, pieceEnd, quoteLen) {
			return i, true
		}
	}
	if quoteLen > 0 && (!inString || pieceEnd < stringEnd) {
		return pieceEnd, false
	}
	if end, ok := substitutionEnd(s, i); ok {
		return end, false
	}
	if strings.HasPrefix(s[i:], "${") {
		if m := cliBraceExpansion.FindStringIndex(s[i:]); m != nil {
			return i + m[1], false
		}
	}
	return i, false
}

// maxInnerQuoteScan bounds how far closesInnerString counts quotes, so a
// long string with many commands is not read again for each one.
const maxInnerQuoteScan = 4096

// closesInnerString reports whether the \" at s[i], in a double-quoted
// string that ends at stringEnd, is a quote of the command inside it that
// closes the string holding a word, rather than one that opens a piece of
// the word, which ends at pieceEnd when quoteLen is not 0. The quotes pair
// up from the string's start, so it closes one when an odd number of them
// follow up to the string's end ("sh -c \"mysql -phunter2\" && echo ok").
// Past maxInnerQuoteScan bytes, a piece that starts or ends with a blank or
// holds a shell operator closes one.
func closesInnerString(s string, i, stringEnd, pieceEnd, quoteLen int) bool {
	if rest := s[i : stringEnd-1]; len(rest) <= maxInnerQuoteScan {
		return innerQuotes(rest)%2 == 1
	}
	if quoteLen == 0 {
		return true
	}
	inner := s[i+quoteLen : pieceEnd-quoteLen]
	return strings.TrimSpace(inner) != inner || strings.ContainsAny(inner, ";|&<>()`,:")
}

// innerQuotes counts the quotes of the command inside a double-quoted
// string in text: its escaped quotes (\"), unless the command escapes them in
// turn (\\\" is a literal quote there, \\\\\" is not).
func innerQuotes(text string) int {
	n, escaped := 0, false
	for j := 0; j < len(text); j++ {
		c := text[j]
		if c == '\\' && j+1 < len(text) && (text[j+1] == '\\' || text[j+1] == '"') {
			// The double-quoted string turns \\ and \" into one character
			// of the command.
			j++
			c = text[j]
		}
		if c == '"' && !escaped {
			n++
		}
		escaped = c == '\\' && !escaped
	}
	return n
}

// closesString reports whether the quote at s[i] closes the quoted string
// that ends at stringEnd.
func closesString(s string, i, stringEnd int) bool {
	return (s[i] == '"' || s[i] == '\'') && i+1 == stringEnd || strings.HasPrefix(s[i:], `\"`) && i+2 == stringEnd
}

// quotedPiece returns the end of the quoted string that starts at i, and the
// length of its quote: 1 for ' and ", 2 for \" (a quote inside a JSON
// string). It is 0 when no quoted string starts at i, or when the string does
// not close on its line. A backslash escapes the next character inside double
// quotes, and an escaped line break goes on to the next line, which the shell
// joins to the string ("pass:Summer\<newline>2026").
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
		case quote == `"` && strings.HasPrefix(s[j:], "$("):
			// A command substitution has quotes of its own
			// ("$(printf "%s" x)").
			if m := cliSubstitution.FindStringIndex(s[j:]); m != nil {
				j += m[1] - 1
			}
		case quote == `"` && strings.HasPrefix(s[j:], "\\\r\n"):
			j += 2
		case quote == `"` && s[j] == '\\' && j+1 < len(s):
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
	// substitutions steps over the command substitutions outside quoted
	// strings, and subStart and subEnd bound the last one found.
	substitutions    bool
	subStart, subEnd int
	cases            caseMemo
	// sep is where the text after the last shell operator outside the
	// quoted strings found starts.
	sep int
	// blankOf is one past the start of the last quoted string commandFrom
	// read, 0 when none, and blank whether its text holds a blank.
	blankOf int
	blank   bool
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
		if q.stepSubstitution() {
			continue
		}
		if end, quoteLen := quotedPiece(q.s, q.i); quoteLen > 0 {
			q.start, q.end, q.i = q.i, end, end
			if p < end {
				return end, true
			}
			continue
		}
		if isCommandEnd(q.s, q.i) {
			q.sep = q.i + 1
		}
		if q.s[q.i] == '\\' {
			q.i++
		}
		q.i++
	}
	return 0, false
}

// commandEnds are the characters after which a new command starts: shell
// operators, line breaks, and a backtick, which starts a command
// substitution or ends Markdown code.
const commandEnds = "\r\n;|&(`"

// isCommandEnd reports whether s[i] is one of commandEnds, and not part of a
// redirection (2>&1).
func isCommandEnd(s string, i int) bool {
	return strings.IndexByte(commandEnds, s[i]) >= 0 && !isRedirectionOperator(s, i)
}

// commandFrom returns where to read the command at the position of the last
// stringAt query from: inside the quoted string it is in, whose closing
// quote is at closeAt, unless the string is one word (ssh "mysql"), or else
// after the last shell operator before it.
func (q *quoteScan) commandFrom(closeAt int) int {
	if closeAt < 0 {
		return q.sep
	}
	if q.blankOf != q.start+1 {
		// Each string is read once (sh -c 'mysql;mysql;...').
		q.blankOf, q.blank = q.start+1, strings.ContainsAny(q.s[q.inner():closeAt], " \t")
	}
	if q.blank {
		return q.inner()
	}
	return q.sep
}

// stepSubstitution steps over the command substitution that starts where the
// scan is, when the scan looks for them, and reports whether one did.
func (q *quoteScan) stepSubstitution() bool {
	if !q.substitutions || !isSubstitutionStart(q.s, q.i) {
		return false
	}
	n := q.cases.substitutionLen(q.s, q.i)
	if n == 0 {
		return false
	}
	q.subStart, q.subEnd, q.i = q.i, q.i+n, q.i+n
	return true
}

// isSubstitutionStart reports whether a command substitution ($(...)) or a
// process substitution (<(...), >(...)) starts at s[i].
func isSubstitutionStart(s string, i int) bool {
	return i+1 < len(s) && s[i+1] == '(' && strings.IndexByte("$<>", s[i]) >= 0
}

// caseMemo finds the length of command and process substitutions at
// increasing positions of one string. The ")" that ends a pattern of a case
// statement in one does not close it ($(case x in x) mysql -pX;; esac)).
type caseMemo struct {
	// openTo is the end of the line where a case statement in a
	// substitution did not close, 0 when none: one there is read as
	// before, and the rest of the line is not searched again.
	openTo int
}

// substitutionLen returns the length of the command or process substitution
// that starts at s[i], or 0 when none does.
func (m *caseMemo) substitutionLen(s string, i int) int {
	rest := s[i:]
	n := len(cliSubstitution.FindString(rest))
	if n == 0 || i < m.openTo || !strings.Contains(rest[:n], caseKeyword) {
		return n
	}
	c := caseScan{start: true}
	from, end := 2, n
	for c.read(rest[:end-1], from) {
		more := len(cliParensRest.FindString(rest[end:]))
		if more == 0 {
			lineEnd := strings.IndexAny(rest[end:], "\r\n")
			if lineEnd < 0 {
				lineEnd = len(rest) - end
			}
			m.openTo = i + end + lineEnd
			return n
		}
		from, end = end-1, end+more
	}
	return end
}

// cliParensRest matches text inside parentheses up to the ")" that closes
// them.
var cliParensRest = regexp.MustCompile(`^(?:` + cliParensPiece + `|\$?` + cliParens2 + `)*\)`)

// cliParensGroup matches text in parentheses at the start of a string.
var cliParensGroup = regexp.MustCompile(`^\$?` + cliParens2)

// caseScan reads the text of a command substitution for its case
// statements, so the ")" after a pattern is told from the one that closes
// the substitution.
type caseScan struct {
	// open is the number of case statements read and not yet closed.
	open int
	// state is where the reading is in a case statement's head or pattern.
	state caseState
	// start is set where a command starts.
	start bool
}

// caseKeyword and esacKeyword open and close a case statement.
const (
	caseKeyword = "case"
	esacKeyword = "esac"
)

// caseState is a part of a case statement.
type caseState int

const (
	// caseNone is outside the head and patterns of a case statement.
	caseNone caseState = iota
	// caseWord follows the case keyword, and caseSubject the word after it.
	caseWord
	caseSubject
	// casePattern is in a pattern (after in, ;; or ;&), which ")" ends.
	casePattern
)

// read reads s[from:], text inside the substitution, and reports whether
// it ends in a pattern, so the ")" after it ends that pattern.
func (c *caseScan) read(s string, from int) bool {
	for i := from; i < len(s); {
		switch ch := s[i]; {
		case ch == ' ' || ch == '\t':
			i++
		case ch == ';' && c.open > 0 && i+1 < len(s) && (s[i+1] == ';' || s[i+1] == '&'):
			// ;; ;& and ;;& end a case arm, and a pattern follows.
			c.state, c.start = casePattern, true
			i += 2
		case ch == '(' && c.state == casePattern:
			// The optional "(" before a pattern.
			i++
		case ch == '(':
			i += max(1, len(cliParensGroup.FindString(s[i:])))
			c.start = false
		case ch == ')':
			// Only a pattern's ")" is inside the substitution.
			c.state, c.start = caseNone, true
			i++
		case strings.IndexByte("\r\n;&|", ch) >= 0:
			// A "|" between patterns starts no command.
			c.start = c.start || c.state != casePattern
			i++
		default:
			end := caseWordEnd(s, i)
			c.word(s[i:end])
			i = end
		}
	}
	return c.state == casePattern
}

// caseWordEnd returns the end of the word that starts at s[i], whose quoted
// strings, escaped characters and substitutions are part of it.
func caseWordEnd(s string, i int) int {
	for i < len(s) && strings.IndexByte(" \t\r\n;&|()", s[i]) < 0 {
		if end, quoteLen := quotedPiece(s, i); quoteLen == 1 {
			i = end
			continue
		}
		if s[i] == '\\' {
			i++
		} else if isSubstitutionStart(s, i) {
			i += len(cliParensGroup.FindString(s[i+1:]))
		}
		i++
	}
	return min(i, len(s))
}

// word reads one word of the substitution.
func (c *caseScan) word(w string) {
	switch {
	case c.state == caseWord:
		c.state = caseSubject
	case c.state == caseSubject && w == "in":
		c.state = casePattern
		c.open++
	case c.state == caseSubject:
		c.state = caseNone
	case (c.start || c.state == casePattern) && w == esacKeyword && c.open > 0:
		c.state = caseNone
		c.open--
	case c.state == casePattern:
	case c.start && w == caseKeyword:
		c.state = caseWord
	}
	c.start = isShellKeyword(w)
}

// substitutionAt returns where the command substitution that p is inside
// starts and ends, and false when p is in none. It reads what the last
// stringAt query for p found.
func (q *quoteScan) substitutionAt(p int) (start, end int, ok bool) {
	return q.subStart, q.subEnd, q.subStart < p && p < q.subEnd
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

// sshCommand is ssh, a command wrapper that runs the words after its
// destination.
const sshCommand = "ssh"

// execCommand and envCommand are the command wrappers exec and env; exec is
// also the subcommand of a container client that runs a command, and
// loginCommand the one that logs in to a registry (docker login -p).
const (
	execCommand  = "exec"
	envCommand   = "env"
	loginCommand = "login"
)

// isWrapperOperand reports whether the client name at start is the
// destination of an ssh command (ssh mysql -p2222, ssh -i "key" mysql
// -p2222, where -p2222 is ssh's port) or the argument of a wrapper's option
// (sudo -u mysql ssh -p2222), not a command. It reads the words of the
// command from its start, at most maxSSHCommand bytes back, so an ssh that
// is an argument of another command (docker exec ssh mysql -pX) does not
// count. from is outside quoted strings, at or before the command's start;
// when it is further back, the command starts after the last quote or shell
// operator.
func isWrapperOperand(s string, from, start int) bool {
	// The word may be a path ending in the name (ssh -i ~/.ssh/mysql), and
	// quoted (ssh "mysql").
	word := start
	for word > 0 && start-word < maxSSHCommand && !isShellWordEnd(s[word-1]) && s[word-1] != '"' && s[word-1] != '\'' {
		word--
	}
	if word > from && (s[word-1] == '"' || s[word-1] == '\'') {
		if end, quoteLen := quotedPiece(s, word-1); quoteLen == 1 && end > start {
			word--
		}
	}
	if word == 0 || s[word-1] != ' ' && s[word-1] != '\t' {
		return false
	}
	part := s[word:start]
	start = word
	from, ok := wrapperCommandFrom(s, from, start)
	if !ok || !slices.ContainsFunc(argWrappers, func(w string) bool { return strings.Contains(s[from:start], w) }) {
		return false
	}
	var c commandStart
	c.at(s, from, start)
	if runsJoinedArgument(c.wrapper, part) {
		return false
	}
	// The destination, or the argument of an option (ssh -i mysql,
	// sudo -u mysql), unless that option runs it (env -S mysql).
	return !c.text && !c.execArg && (c.optionArg || hasDestination(c.wrapper) && !c.host)
}

// wrapperCommandFrom returns where the command whose word starts at start
// begins, reading s from from, which is outside quoted strings: at most
// maxSSHCommand bytes back, where the command starts after the last quote or
// shell operator. It is false when the command starts further back.
func wrapperCommandFrom(s string, from, start int) (int, bool) {
	if start-from <= maxSSHCommand {
		return commandFrom(s, from, start), true
	}
	from = max(0, start-maxSSHCommand)
	if i := strings.LastIndexAny(s[from:start], commandEnds+`'"`); i >= 0 {
		return from + i + 1, true
	}
	return from, from == 0
}

// argWrappers are the wrappers that take an argument after the command's
// start: a destination, or an option's (wrapperArgOptions).
var argWrappers = []string{sshCommand, "sudo", envCommand, "time", execCommand, "docker", "podman", "nerdctl", "kubectl"}

// maxSSHCommand bounds how far back isWrapperOperand looks for the start of
// a wrapper command.
const maxSSHCommand = 256

// commandFrom returns where the command that start is in begins, reading s
// from from, which is outside quoted strings: after the last shell operator
// before start, or inside the quoted string start is in. A quoted string
// that closes before start is part of a word (ssh -i "key" mysql).
func commandFrom(s string, from, start int) int {
	head, b := s[:start], from
	for i := from; i < start; {
		if isApostrophe(s, i) {
			i++
			continue
		}
		if end, quoteLen := quotedPiece(head, i); quoteLen > 0 {
			i = end
			continue
		}
		switch {
		case s[i] == '"' || s[i] == '\'':
			// start is inside this string, or it never closes.
			i++
			b = i
		case strings.HasPrefix(head[i:], `\"`):
			i += 2
			b = i
		case isCommandEnd(s, i):
			i++
			b = i
		case s[i] == '\\':
			i += 2
		default:
			i++
		}
	}
	return b
}

// isEntrypointArgument reports whether the client name at start is the
// argument of a container client's --entrypoint (docker run --entrypoint
// mysql, --entrypoint=/usr/bin/mysql), whose own arguments come after the
// image, rather than of another program's option of that name
// (runner --entrypoint=mysql -pX). from is outside quoted strings, at or
// before the command's start; starts reads the command from there, once for
// names at increasing positions, however long the command is.
func isEntrypointArgument(s string, starts *commandStart, from, start int) bool {
	window := s[max(0, start-maxEntrypointPart):start]
	part := window[strings.LastIndexAny(window, " \t")+1:]
	joined := strings.HasPrefix(part, "--entrypoint=")
	if !joined {
		if part != "" && !strings.HasSuffix(part, "/") || len(part) == len(window) {
			return false
		}
		before := strings.TrimRight(window[:len(window)-len(part)], " \t")
		option := strings.TrimSuffix(before, "--entrypoint")
		if len(option) == len(before) || option != "" && option[len(option)-1] != ' ' && option[len(option)-1] != '\t' {
			return false
		}
	}
	word := start - len(part)
	starts.at(s, from, word)
	if joined {
		return isContainerClient(starts.wrapper) && runsJoinedArgument(starts.wrapper, part)
	}
	return isContainerClient(starts.wrapper) && starts.execArg
}

// maxEntrypointPart bounds how far before a client name isEntrypointArgument
// looks for --entrypoint, so a long word of names is not read again for each.
const maxEntrypointPart = 256

// entrypointArgs records where the arguments of the last container
// entrypoint read begin, so repeated --entrypoint options
// (docker run --entrypoint sshpass --entrypoint sshpass ...) do not read the
// options after each again.
type entrypointArgs struct {
	// start is where the last entrypoint's name starts, end where its
	// command's segment ends, and args where its arguments begin.
	start, end, args int
}

// at returns where the arguments of the entrypoint whose name starts at
// start begin (entrypointArgsStart). A later name among the options read
// for the last one, in the same segment and not inside a quoted string or
// substitution opened after it, finds the same image; one inside them
// (--env X=$(docker run --entrypoint sshpass inner -pX)) is read on its own.
func (e *entrypointArgs) at(s string, start, end, stringEnd int) int {
	if e.start < start && start < e.args && end == e.end && isSameLevel(s[e.start:start]) {
		e.start = start
		return e.args
	}
	e.start, e.end = start, end
	e.args = entrypointArgsStart(s, start, end, stringEnd)
	return e.args
}

// isSameLevel reports whether text, between two words of a command, closes
// every quoted string and parenthesis it opens, so the second word is where
// the first one is rather than inside a string or substitution.
func isSameLevel(text string) bool {
	depth, quote := 0, byte(0)
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
	}
	return depth == 0 && quote == 0
}

// entrypointArgsStart returns where the arguments of a container's
// entrypoint, whose name starts at start, begin: after the container
// client's options and the image (docker run --entrypoint mysql
// -p3306:3306 mysql:8 -pX), or at end when no image comes before it.
// stringEnd is where the quoted string holding the command ends, or -1.
func entrypointArgsStart(s string, start, end, stringEnd int) int {
	i := min(shellWordEnd(s, start, stringEnd), end)
	optionArg := false
	for i < end {
		if s[i] == ' ' || s[i] == '\t' {
			i++
			continue
		}
		j := min(max(shellWordEnd(s, i, stringEnd), i+1), end)
		word := s[i:j]
		switch {
		case optionArg:
			optionArg = false
		case strings.HasPrefix(word, "-"):
			optionArg = takesArgument("docker", word)
		default:
			return j
		}
		i = j
	}
	return end
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
			regions = p.appendPassword(regions, s, &lines, loc)
			for _, d := range netrcDefaultPatterns {
				if dloc := d.pattern.FindStringSubmatchIndex(s[loc[1]:]); dloc != nil {
					for k := range dloc {
						if dloc[k] >= 0 {
							dloc[k] += loc[1]
						}
					}
					regions = d.appendPassword(regions, s, &lines, dloc)
					break
				}
			}
		}
	}
	return regions
}

// appendPassword adds the password of the .netrc entry that loc, a match of
// p in s, found.
func (p netrcPattern) appendPassword(regions []taggedRegion, s string, lines *quoteScan, loc []int) []taggedRegion {
	machine := ""
	if loc[2] >= 0 {
		machine = s[loc[2]:loc[3]]
	}
	login := s[loc[2*p.login]:loc[2*p.login+1]]
	start, end := loc[2*p.value], loc[2*p.value+1]
	if isNetrcDeclaration(s, loc[0], machine, login, s[start:end]) {
		return regions
	}
	valueStart, valueEnd, singleQuoted := unquoteRangeQuoted(s, start, end)
	// A .netrc file expands nothing, so a password that starts with $
	// ($ecret, $SUMMER2026) is literal, as in single quotes.
	literal := singleQuoted || strings.HasPrefix(s[valueStart:valueEnd], "$")
	v := cliValue{start: valueStart, end: valueEnd, quoted: valueStart != start, singleQuoted: literal}
	v.gap = v.quoted && valueStart == lines.stringEnd(start-1) && isStringGap(s[valueStart:valueEnd])
	if r, ok := passwordValueRegion(s, v, false, false); ok {
		regions = append(regions, r)
	}
	return regions
}

// passwordValueRegion returns the region of the password v, unless it is not
// a secret. The value stays visible when it is one of the shared placeholders
// ($PASSWORD, ${PASSWORD}, <password>) or a command substitution that fetches
// the password from elsewhere ($(cat file)); inside single quotes $ is
// literal. When dashFlag is set, an unquoted value that starts like another
// flag is that flag. A long flag followed by a blank instead of "=" is also
// common in prose ("pass --password to set it") and help text ("--password
// string   Password for"), so there (requireNonWord) an unquoted word of
// prose stays visible (see isProseValue); any other word, or a prose word
// the command ends with or gives more arguments after, may be a password
// (mongosh --password swordfish, mongosh --password string).
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
	if requireNonWord && !v.quoted && isProseValue(s, v) {
		return taggedRegion{}, false
	}
	return taggedRegion{region: region{v.start, v.end}}, true
}

// isProseValue reports whether v, the value after a long flag and a blank,
// is a word of prose (to, flag, string) in a sentence: one that ends a
// sentence or clause ("the --password flag.", "--password option, or"), or
// a grammatical word that another word follows ("--password to set it",
// "--password" is required). A word that names what the flag takes (flag,
// string, value) may also be the password, so another word makes it prose
// only when that word is prose too ("the --password flag is") or the text
// of a help line ("--password string   Password for"); the words after it
// may be arguments (mongosh --password string db admin).
func isProseValue(s string, v cliValue) bool {
	value := s[v.start:v.end]
	word := strings.TrimRight(value, ".,:!?")
	if !slices.Contains(proseWords, word) {
		return false
	}
	if len(word) < len(value) {
		return v.end == len(s) || unicode.IsSpace(rune(s[v.end]))
	}
	gap := len(s[v.end:]) - len(strings.TrimLeft(s[v.end:], " \t"))
	first, _ := proseWord(s[v.end+gap:])
	if gap == 0 || first == "" {
		return false
	}
	return !slices.Contains(proseNouns, word) || gap > 1 || s[v.end] == '\t' || slices.Contains(proseWords, strings.TrimRight(first, ".,:!?"))
}

// proseWord returns the word of prose that text starts with, letters and
// the punctuation after them, and the text after it; the word is empty when
// text starts with something else.
func proseWord(text string) (word, rest string) {
	end := 0
	for end < len(text) && unicode.IsLetter(rune(text[end])) {
		end++
	}
	if end == 0 {
		return "", text
	}
	for end < len(text) && strings.IndexByte(".,:!?", text[end]) >= 0 {
		end++
	}
	if end < len(text) && !unicode.IsSpace(rune(text[end])) {
		return "", text
	}
	return text[:end], text[end:]
}

// isNetrcDeclaration reports whether the .netrc match at start gives its
// machine, login and password one type name inside an open brace or
// parenthesis, the shape of a declaration (machine string, login string,
// password string in a Go struct or parameter list) rather than of an entry,
// which may give all three one word (machine admin login admin password
// admin), even a type name (machine string login string password string).
func isNetrcDeclaration(s string, start int, machine, login, password string) bool {
	name := declarationType(password)
	if declarationType(machine) != name || declarationType(login) != name || !slices.Contains(declarationTypes, name) {
		return false
	}
	before := s[max(0, start-maxDeclarationScan):start]
	opened := max(strings.LastIndexByte(before, '{'), strings.LastIndexByte(before, '('))
	return opened >= 0 && declarationFields.MatchString(before[opened+1:])
}

// declarationFields matches the fields a declaration may give before the
// .netrc-shaped ones, each ended by a separator or a line break, after a
// struct tag or a comment (struct {\n\tHost string `json:"host"`\n\t,
// f(ctx context.Context, ), so a shell group or subshell that runs a command
// is no declaration ({ echo 'machine string ...'; },
// { echo foo machine string ...; }).
var declarationFields = regexp.MustCompile(`^\s*(?:[A-Za-z_]\w*[ \t]+[\w.*&\[\]]+(?:[ \t]+` + "`[^`\\r\\n]*`" + `)?[ \t]*(?://[^\n]*)?(?:[;,]|\r?\n)\s*)*$`)

// maxDeclarationScan bounds how far back isNetrcDeclaration looks for the
// brace or parenthesis that opens a declaration, so the scan stays linear.
const maxDeclarationScan = 4096

// declarationType returns the type name a declaration's field may give,
// without the separator after it or the pointer, slice or reference mark
// before it ("*string," and "[]byte)" give string and byte).
func declarationType(field string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimRight(field, ",;)"), "*&[]"))
}

// declarationTypes are the type names of a declaration's fields, without a
// pointer, slice or reference mark (*string, []byte, &str).
var declarationTypes = []string{"string", "str", "text", "varchar", "char", "byte", "bytes", "int", "int32", "int64", "uint", "number", "bool", "boolean", "any", "object", "secret"}

// proseWords are the words that follow a long password flag and a blank in
// prose and help text ("--password to set it", "the --password flag",
// "--password string").
var proseWords = append([]string{
	"a", "an", "and", "are", "as", "at", "be", "but", "by", "can", "does", "for", "from", "if", "in", "into", "is", "it", "not", "of", "on", "or", "so", "than", "that", "the", "then", "this", "to", "was", "when", "where", "which", "while", "will", "with", "you", "your",
	"also", "instead", "only", "here", "now", "too", "via", "etc",
}, proseNouns...)

// proseNouns are the proseWords that name what a flag takes, which a
// password may also be (mongosh --password string db).
var proseNouns = []string{"flag", "flags", "option", "options", "argument", "arguments", "arg", "args", "parameter", "parameters", "param", "value", "values", "string", "prompt", "field"}
