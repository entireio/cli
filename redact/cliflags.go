package redact

import (
	"container/heap"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// cliFlagValue is one password argument on a command line: a quoted string
// that stays on its line (a double-quoted one may go on past an escaped line
// break, which the shell removes, and its match may stop at a command
// substitution, which shellWordValue reads on to a later line:
// "$(printf x<newline>)"), or a bare word that ends at whitespace, a
// quote or a shell metacharacter. Ending at a backtick or a parenthesis keeps
// the closing delimiter of Markdown code (`mysql -pX`) and of $(...) out of
// the value. A value may start with a backtick, which shellWordValue reads
// as a command substitution (--password=`printf x`) or rejects as the end
// of Markdown code.
const cliFlagValue = `"(?:[^"\r\n]|\\\r?\n)*(?:"|\$\()|'[^'\r\n]*'|[^\s'"` + "`" + `|;&()<>]+|` + "`"

// cliSameCommandChar is one piece of the text between a command name and one
// of its flags. It skips quoted strings (with escapes inside double quotes:
// "abc\";x", and in ANSI-C quotes: $'abc\';x'), command substitutions ($(printf x; true)), parameter
// expansions (${IN:-a&b}), escaped characters (\;) and escaped line breaks
// (a command continued with a trailing backslash), and otherwise never
// crosses a line break, a shell operator or a backtick (which starts a
// command substitution, or ends Markdown code), so a flag is only read as
// belonging to the command that precedes it in the same pipeline segment.
// The & and | of a redirection (2>&1, &>log, >|f) are no shell operator,
// and neither is the | between the patterns of an extended glob
// (-in /dev/@(null|zero)).
const cliSameCommandChar = `\\\r?\n|\\[^\r\n]|\$'(?:[^'\\\r\n]|\\[^\r\n])*'|"(?:[^"\\\r\n$]|\$` + cliParens3 + `|\$|\\\r?\n|\\[^\r\n])*"|'[^'\r\n]*'|` + cliSubstitutionPattern + `|\$` + cliBraces2 + `|[<>]&|&>|>\||` + cliExtendedGlob + `|[^\r\n|;&` + "`" + `]`

// cliExtendedGlob is an extended glob's patterns in parentheses, which bash
// reads with extglob set (@(a|b), !(*.log), +(x)), with no quote,
// parenthesis, backtick or line break in them.
const cliExtendedGlob = `[?*+@!]\([^()'"\r\n` + "`" + `]*\)`

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

// cliListNext matches the end of one quoted element of an argument list in
// code and the start of the next, which may name the subcommand of a client
// that the list runs (["docker", "login", ...]).
const cliListNext = `\\?["'],` + cliListGap + `\\?["']`

// cliListOptions matches the options a client takes before its subcommand
// in an argument list in code, as cliGlobalOptions does on a command line:
// each an element that starts with '-', with at most one value element
// after it (["docker", "--config", "/tmp/config", "login"]), each with the
// end of the element before it. cliListChar is a character of an element.
const (
	cliListOptions = `(?:` + cliListNext + `-` + cliListChar + `*(?:` + cliListNext + `[^-"'\\\r\n]` + cliListChar + `*)?)*`
	cliListChar    = `(?:[^"'\\\r\n]|\\\\)`
)

// cliNoValueGlobalOption matches a global option of the login clients that
// takes no value (docker --debug, podman --remote, az --verbose), or one
// joined to its value (docker --config=/tmp/c). The word after one is the
// client's subcommand.
var cliNoValueGlobalOption = regexp.MustCompile(`^(?:-[Ddhrv]|--(?:debug|debug-full|tls|tlsverify|verbose|only-show-errors|remote|syslog|noout|transient-store|insecure-policy|insecure-registry|experimental|kube-insecure-skip-tls-verify|help|version)$|\\?["']?--[\w-]+=)`)

// cliOptionWord matches the next shell word of a client's global options on
// a command line, group 1, after the blank before it, and cliListElement the
// next element of an argument list in code, group 1, after the end of the
// one before it. They read the words as the shell does, so text inside a
// quoted value or a substitution, or after an escaped blank, is part of its
// word (docker --config "/opt --debug x" login, --config $(mktemp -d)
// login, --config /opt\ --debug\ x login).
var (
	cliOptionWord  = regexp.MustCompile(`^` + cliBlank + `((?:\\[^\r\n]|` + cliWordPiece + `)+)`)
	cliListElement = regexp.MustCompile(`^` + cliListNext + `(` + cliListChar + `*)`)
	cliClientName  = regexp.MustCompile(`^\w+(?i:\.exe)?`)
)

// cliGlobalOptions matches the options a client takes before its
// subcommand, each a shell word (--config="$HOME/.docker", or quoted
// whole: "--config=/tmp/c") with at most one value (--config $(mktemp -d)).
const cliGlobalOptions = `(?:` + cliBlank + `(?:-` + cliWordPiece + `+|(?:"-(?:[^"\\\r\n]|\\[^\r\n])*"|'-[^'\r\n]*')` + cliWordPiece + `*)(?:` + cliBlank + cliWordPiece + `+)?)*`

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
// separates the flag from its value, or the comma between the elements of
// an argument list in code (["wget", "--password", "x"]), and group 3 the
// value. --password-stdin
// and --password-file never match: the flag must be followed by "=" or a
// blank. The name may be joined from quoted pieces (--pass"word"=x), which
// longPasswordName checks.
var cliLongPasswordPattern = regexp.MustCompile(`(?:^|[\s"'])--(?:password|passwd|pass|` + cliQuotedFlagName + `)((?:\\?["'])?)(=|` + cliBlank + `|,` + cliListGap + `)(` + cliFlagValue + `)`)

// cliListGap is what may come between the elements of an argument list in
// code: blanks, line breaks, blank lines and comments ("--password", # pw,
// then "x" on the next line, or /* c */ between elements).
const cliListGap = `(?:[ \t\r\n]|#[^\r\n]*|//[^\r\n]*|/\*(?:[^*]|\*+[^/])*\*/)*`

// cliQuotedFlagName matches the name of a long flag joined from quoted and
// bare pieces of letters.
const cliQuotedFlagName = `(?:[a-z]|"[a-z]*"|'[a-z]*')+`

// longPasswordName reports whether the flag that loc, a
// cliLongPasswordPattern match in s, found is a password flag once the shell
// removes its quotes, and whether it is --pass.
func longPasswordName(s string, loc []int) (ok, pass bool) {
	from := loc[0] + len("--")
	if s[loc[0]] != '-' {
		from++
	}
	name := s[from:loc[2]]
	if strings.ContainsAny(name, `"'`) {
		name = wordQuotes.Replace(name)
	}
	return name == "password" || name == "passwd" || name == "pass", name == "pass"
}

// mayHoldLongPasswordFlag reports whether s may hold a match of
// cliLongPasswordPattern, which spares most strings the pattern.
func mayHoldLongPasswordFlag(s string) bool {
	return strings.Contains(s, "--pass") ||
		(strings.Contains(s, "--p") || strings.Contains(s, `--"`) || strings.Contains(s, "--'")) && strings.ContainsAny(s, `"'`)
}

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
	// noValueLong marks a client whose --password takes no value, or one
	// only after "=" (psql --password -h db, mysql --password -uroot), so
	// a word after a blank that starts with '-' is its next flag. For
	// other commands it is the value (docker login --password -x, wget
	// --password -x). flag is nil for a client with no short flag.
	noValueLong bool
	// sourceLong marks a client whose --pass and --password take a
	// password source, as its one-dash flags do (openssl enc --pass
	// env:PW): one that names where the password is (env:, file:, fd:,
	// stdin) stays visible after them too.
	sourceLong bool
}

var cliPasswordFlags = []cliPasswordFlag{
	// mysql takes the password glued to -p; a bare -p prompts for it. Its
	// options may be clustered, so -p may follow options that take no
	// value (-vpX, -Bqp"X"); one that takes a value takes the rest of the
	// word (-uphil is user phil). The MariaDB tools are named mariadb-dump
	// and so on, which mariadb alone would leave to isCommandToken to
	// reject.
	{
		needles:     []string{"mysql", "mariadb"},
		command:     regexp.MustCompile(`\b(?:mariadb-(?:dump|admin|import|check|upgrade)|mysql(?:dump|admin|import|check|_upgrade)?|mariadb)\b`),
		flag:        regexp.MustCompile(cliShortFlag(`\s`, mysqlForceOptions+"p", "")),
		noValueLong: true,
	},
	{
		needles:     []string{"mysql", "mariadb"},
		command:     regexp.MustCompile(`\b(?:mariadb-(?:show|slap|binlog|backup)|mysql(?:pump|sh|show|slap|binlog))\b`),
		flag:        regexp.MustCompile(cliShortFlag(`\s`, mysqlNoValueOptions+"p", "")),
		noValueLong: true,
	},
	// login must be the client's subcommand, after its global options
	// (docker --config dir login, helm registry --debug login), so a
	// container named login (docker run --name login -p 8080:80) is not.
	// The shell removes the quotes of a quoted one (docker "login" -p x).
	{
		needles: []string{loginCommand},
		command: regexp.MustCompile(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm)\b(?i:\.exe)?(?:["']?` + cliGlobalOptions + cliBlank + `(?:` + cliSubcommand("registry") + cliGlobalOptions + cliBlank + `)?|` + cliListOptions + cliListNext + `(?:registry` + cliListOptions + cliListNext + `)?)` + cliSubcommand(loginCommand)),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "p", `(?:`+cliBlank+`)?`)),
	},
	// az may be quoted with its path ("/usr/bin/az" login) and take
	// global options before login (az --debug login).
	{
		needles: []string{loginCommand},
		command: regexp.MustCompile(`\baz\b(?i:\.exe)?(?:["']?` + cliGlobalOptions + cliBlank + `|` + cliListOptions + cliListNext + `)` + cliSubcommand(loginCommand)),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "p", `(?:`+cliBlank+`)?`)),
	},
	// The mongo tools take -p's value after a blank, after "=" (mongodump
	// -p=x, as they document it) or joined to it.
	{
		needles:     []string{"mongo"},
		command:     regexp.MustCompile(`\b(?:mongosh|mongo|mongodump|mongorestore|mongoexport|mongoimport|mongostat|mongotop)\b`),
		flag:        regexp.MustCompile(cliShortFlag(`\s`, "p", `(?:=|`+cliBlank+`)?`)),
		optional:    true,
		noValueLong: true,
	},
	// The PostgreSQL clients take --password with no value: it makes them
	// prompt (psql --password -h db).
	{
		needles:     []string{"psql", "pgcli", "pg_", "createdb", "dropdb", "createuser", "dropuser", "clusterdb", "reindexdb", "vacuumdb"},
		command:     regexp.MustCompile(`\b(?:psql|pgcli|pg_(?:dump(?:all)?|restore|basebackup|receivewal|recvlogical|amcheck)|createdb|dropdb|createuser|dropuser|clusterdb|reindexdb|vacuumdb)\b`),
		noValueLong: true,
	},
	// sshpass reads its own options only before the command it wraps, so
	// the -p of sshpass -e ssh -p 2222 is ssh's port. They may be
	// clustered after -v (-vv, -vp x, -vPprompt), and be elements of an
	// argument list in code (["sshpass", "-v", "-p", "x", "ssh", "h"],
	// ["sshpass", "-P", "Password:", "-p", "x", ...]).
	{
		needles: []string{"sshpass"},
		command: regexp.MustCompile(`\bsshpass\b`),
		flag:    regexp.MustCompile(cliShortFlag(`^(?:(?:`+cliBlank+`-(?:v+|v*P[ \t]*(?:`+cliFlagValue+`)))*`+cliBlank+`|\\?["'](?:,`+cliListGap+`\\?["']-(?:v+|v*P(?:`+cliListChar+`+|`+cliListNext+cliListChar+`*))\\?["'])*,`+cliListGap+`)`, "v*p", `[ \t]*`)),
		leading: true,
	},
	{
		needles: []string{"redis-cli"},
		command: regexp.MustCompile(`\bredis-cli\b`),
		flag:    regexp.MustCompile(cliShortFlag(`\s`, "a", cliBlank)),
	},
	// openssl takes a source after a blank or "=" (-pass=pass:x), and its
	// options with two dashes too (--passin pass:x). The flag may be
	// quoted, in escaped quotes too in a double-quoted script
	// (sh -c "openssl enc \"-pass\" \"pass:x\""), or with its source as one
	// element of an argument list in code (["openssl", "-pass=pass:x"]).
	{
		needles:    []string{"openssl"},
		command:    regexp.MustCompile(`\bopenssl\b`),
		flag:       regexp.MustCompile(cliShortFlag(`\s`, opensslSourceFlag, `(?:=|`+cliBlank+`)`)),
		prefix:     "pass:",
		sourceLong: true,
	},
}

// opensslSourceFlag matches the names of openssl's options that take a
// password source, after their first dash, with one more dash or none
// (-passin, --passout). The long-flag pattern already reads the other
// two-dash spellings (--pass, --password).
const opensslSourceFlag = `(?:-pass(?:in|out)|pass(?:in|out|word)?)`

// mysqlNoValueOptions matches the short options that take no value in every
// mysql and MariaDB tool that has them (-v, -s, -B, -N, ...), which may come
// before -p in one word. An option that takes a value in any of them (-u,
// -h, -D, -e, -c, -i, -r, -w, mysqlslap's -q, mysqlsh's -f, mariadb-backup's
// -H and -t, ...) is left out, since its value would be the rest of the word.
const mysqlNoValueOptions = `[ABCENVXbnsv]*`

// mysqlForceOptions is mysqlNoValueOptions with -f, which is --force in the
// client and in the dump, admin, import, check and upgrade tools, and takes
// no value there (mysql -uroot -fpX), unlike mysqlsh's -f.
const mysqlForceOptions = `[ABCENVXbfnsv]*`

// cliPythonPrefix matches the prefix of a Python string, which may open a
// quoted flag in an argument list in code ([r"-pC:\x"], see
// pythonStringPrefix).
const cliPythonPrefix = `(?:[rRbBuU]|[rR][bB]|[bB][rR])?`

// cliShortFlag matches the short flag -name after before, then sep and its
// value: -px, a flag quoted with its value ("-px", "-p x"), or one quoted on
// its own ("-p" x, "-p"'x'). A blank before may also be a separator of code,
// as in an argument list written without blanks (["mysql","-px"]), before a
// quoted flag only (see isJoinedFlag). Where sep may be a blank, a flag
// quoted on its own may also take the next element of an argument list in
// code as its value (["redis-cli", "-a", "x"], see isListFlag).
func cliShortFlag(before, name, sep string) string {
	if before == `\s` {
		before = `[\s,(\[]`
	}
	quotedSep := sep
	if sep != "" {
		quotedSep = `(?:,` + cliListGap + `|` + sep + `)`
	}
	return before + `(?:` + cliPythonPrefix + cliFlagQuote + `-` + name + cliFlagClose + quotedSep + `|-` + name + sep + `)(` + cliFlagValue + `)`
}

// cliSubcommand matches name as a client's subcommand, which may be quoted
// ("login", 'login', or \"login\" in a double-quoted command), as the shell
// passes it unquoted.
func cliSubcommand(name string) string {
	return `(?:\\?["'])?` + name + `\b(?:\\?["'])?`
}

// netrcValue is a .netrc token. A double-quoted one may hold escaped quotes
// ("abc\"def", as curl reads it), and one in escaped quotes is that in a
// double-quoted command that writes the file (echo "... password
// \"Summer 2026\"" > ~/.netrc). curl reads a single quote as part of a
// token ('Summer'2026, and an empty pair before hunter2), so a single-quoted
// piece takes the rest of its token, and so does a backtick-quoted one
// (`hunter2`). A shell-escaped apostrophe between single-quoted pieces
// (it's $2026 written in single quotes) is still that token. A quote or
// backtick that closes nothing on its line starts a token too ('hunter2),
// as curl reads it. A
// command substitution, which the
// command writing the file runs ($(printf hunter2)), is one piece of a
// token. Outside quotes, a quote or backtick ends it when whitespace, a
// shell metacharacter or a separator of code follows, so the closing quote
// of echo "machine h login me password p" > ~/.netrc and of
// fmt.Sprintf("machine %s login %s password %s", h, u, p) stays outside the
// value.
const netrcValue = `"(?:[^"\\\r\n]|\\[^\r\n])*"|\\"(?:[^"\\\r\n]|\\[^"\r\n])*\\"|'[^'\r\n]*'(?:\\''[^'\r\n]*')*(?:` + netrcBareValue + `)?|` +
	"`[^`\\s]*`(?:" + netrcBareValue + `)?|['` + "`" + `]+(?:` + netrcBareValue + `)|` + netrcBareValue

// netrcBareValue is a .netrc token outside quotes (see netrcValue).
const netrcBareValue = `(?:\$` + cliParens3 + `|[^\s"'` + "`" + `])+(?:["'` + "`" + `]+(?:\$` + cliParens3 + `|[^\s"'` + "`" + `;|&()<>,\]}])(?:\$` + cliParens3 + `|[^\s"'` + "`" + `])*)*`

// netrcName is a .netrc machine, login or account name, quoted when it
// holds blanks (login "alice smith").
const netrcName = `"(?:[^"\\\r\n]|\\[^\r\n])*"|\S+`

// netrcEntry starts a .netrc entry. A default entry must start a line or a
// quoted string, so "by default login with password is" is not one. Group 1
// is the machine, absent for default.
const netrcEntry = `(?:\b(?i:machine)[ \t\r\n]+(` + netrcName + `)|(?:^|[\r\n"'])[ \t]*(?i:default))`

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
	netrcAccount      = `(?:` + netrcSep + `(?i:account)` + netrcValueSep + `(?:` + netrcName + `))?`
	// netrcFields are the account and login fields that may come before
	// the ones a pattern reads, each any number of times, as a client
	// reads the last of each (machine h login old login me password x).
	netrcFields = `(?:` + netrcSep + `(?i:account|login)` + netrcValueSep + `(?:` + netrcName + `))*`
)

// netrcPasswordPatterns match the password of a .netrc entry that also names
// its login, in either order, on one line or across several. The login is
// required, so prose such as "machine learning password policy" is not an
// entry.
var netrcPasswordPatterns = []netrcPattern{
	{regexp.MustCompile(netrcEntry + netrcLoginPassword), 2, 3},
	{regexp.MustCompile(netrcEntry + netrcPasswordLogin), 3, 2},
	// An entry may leave the login to the URL (machine 127.0.0.1 password
	// x) when it starts a line or a quoted string, as a default entry must,
	// and its machine is shaped like a host, which prose such as "the
	// machine learning password policy" is not.
	{regexp.MustCompile(`(?:^|[\r\n"'])[ \t]*(?i:machine)[ \t\r\n]+(` + netrcHost + `)` + netrcAccount + netrcSep + `(?i:password)` + netrcValueSep + `(` + netrcValue + `)`), 0, 2},
	// A default entry may leave the login to the URL too (default password x),
	// when it starts a line or a quoted string, as a machine entry must.
	{regexp.MustCompile(`(?:^|[\r\n"'])[ \t]*(?i:default)()` + netrcAccount + netrcSep + `(?i:password)` + netrcValueSep + `(` + netrcValue + `)`), 0, 2},
}

// netrcHost is a .netrc machine name shaped like a host: localhost, or a
// name or address with a dot (db.example.com, 127.0.0.1).
const netrcHost = `(?:localhost|[\w-]+(?:\.[\w-]+)+)`

// netrcDefaultPatterns match a default entry right after another entry on
// its line (machine h login a password x default login b password y), which
// netrcEntry does not, so prose with "default" is not read as an entry.
var netrcDefaultPatterns = []netrcPattern{
	{regexp.MustCompile(`^` + netrcAccount + `[ \t]+(?i:default)()` + netrcLoginPassword), 2, 3},
	{regexp.MustCompile(`^` + netrcAccount + `[ \t]+(?i:default)()` + netrcPasswordLogin), 3, 2},
	{regexp.MustCompile(`^` + netrcAccount + `[ \t]+(?i:default)()` + netrcAccount + netrcSep + `(?i:password)` + netrcValueSep + `(` + netrcValue + `)`), 0, 2},
}

// netrcLoginPassword and netrcPasswordLogin are the fields of a .netrc entry
// after its machine, in either order.
const (
	netrcLoginPassword = netrcFields + netrcSep + `(?i:login)` + netrcValueSep + `(` + netrcName + `)` + netrcFields + netrcSep + `(?i:password)` + netrcValueSep + `(` + netrcValue + `)`
	netrcPasswordLogin = netrcAccount + netrcSep + `(?i:password)` + netrcValueSep + `(` + netrcValue + `)` + netrcAccount + netrcSep + `(?i:login)` + netrcValueSep + `(` + netrcName + `)`
)

// netrcPattern is a .netrc entry pattern, whose group 1 is the machine, and
// login and value the indexes of the login and password groups, login 0
// for one that names no login.
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
		var noValue noValueFlags
		for _, flag := range cliPasswordFlags {
			regions = flag.appendPasswords(regions, s, &noValue)
		}
		regions = appendLongPasswordFlags(regions, s, &noValue)
	}
	if mayHoldNetrcEntry(s) {
		regions = appendNetrcPasswords(regions, s)
	}
	return regions
}

// mayHoldNetrcEntry reports whether s may hold a .netrc entry, whose
// keywords curl reads in any case (MACHINE h LOGIN me PASSWORD x, LoGiN me
// PaSsWoRd x, machine 127.0.0.1 password x, default password x), which
// spares most strings the patterns.
func mayHoldNetrcEntry(s string) bool {
	return containsFoldASCII(s, "password") && (containsFoldASCII(s, "login") || containsFoldASCII(s, "machine") || containsFoldASCII(s, "default"))
}

func appendLongPasswordFlags(regions []taggedRegion, s string, noValue *noValueFlags) []taggedRegion {
	if !mayHoldLongPasswordFlag(s) {
		return regions
	}
	lines := quoteScan{s: s, prose: true, acrossLines: true}
	subs := substitutionMemo{}
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
		isPassword, pass := longPasswordName(s, loc)
		if !isPassword {
			pos = loc[2]
			continue
		}
		// After "=" a leading '-' is part of the value. After a blank it
		// is too (docker login --password -x, wget --password --x), but
		// starts the next flag of a client whose --password takes no
		// value (psql --password -h db, see noValueLong); --pass always
		// takes one (curl --pass -x, redis-cli --pass -x).
		blank := s[loc[4]] != '='
		dashFlag := blank && !pass && noValue.has(loc[2])
		argv, program := false, ""
		if s[loc[4]] == ',' {
			program = listProgram(s, loc[0])
			dashFlag = dashFlag || !pass && isNoValueClientName(program)
			argv = isListProgram(program)
		}
		if noValue.isSourceReference(s, loc, program) {
			// openssl enc --pass env:PW names where the password is.
			pos = loc[6]
			continue
		}
		stringEnd := lines.stringEnd(loc[2] - 1)
		var r taggedRegion
		// A '-' after a blank starts the next flag, whose word is not
		// read: one that a parameter expansion around it goes on after
		// would be read again for each (--password -${x --password -${x
		// ...}}).
		ok := !dashFlag || s[loc[6]] != '-'
		var v cliValue
		if ok {
			v, ok = longFlagValue(s, loc, stringEnd, subs, dashFlag, argv)
		}
		if ok && pass && isPassNumber(s[v.start:v.end]) {
			// --pass 2 numbers a pass of a tool that runs several.
			ok = false
		}
		if ok {
			r, ok = passwordValueRegion(s, v, stringEnd, subs, dashFlag, blank)
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
// cliLongPasswordPattern match, found. A flag quoted on its own takes a
// value only after "=" ("--password"=x), and must close the quote that
// opens it, and a value in the string that a flag opens
// ("--password=Summer 2026") runs to that string's end, since the shell
// passes the whole string as one argument. dashFlag is
// set when a value that starts with '-' is the next flag, and argv when the
// flag is an element of an argument list that names its program (see
// listPasswordValue).
func longFlagValue(s string, loc []int, stringEnd int, subs substitutionMemo, dashFlag, argv bool) (cliValue, bool) {
	list := s[loc[4]] == ','
	if loc[3] > loc[2] && !list && s[loc[4]] != '=' {
		// A flag quoted on its own before a blank is a word of code or
		// an argument that names it (if arg == "--password" {, grep --
		// "--password" docs/), so it takes a value only after "=" or as
		// an element of a list.
		return cliValue{}, false
	}
	var listValue cliValue
	if list {
		var ok bool
		if listValue, ok = listPasswordValue(s, loc, dashFlag, argv); !ok {
			return cliValue{}, false
		}
	}
	open, quoteLen := loc[0], 0
	if s[open] == '"' && open > 0 && s[open-1] == '\\' {
		open--
	}
	if end, n := flagStringEnd(s, open); n > 0 && end > loc[2] {
		quoteLen = n
		closeAt := end - n
		switch {
		case loc[3] > loc[2]:
			// "--password" x: the quote after the name closes this one.
			if closeAt != loc[3]-n {
				return cliValue{}, false
			}
		case s[loc[4]] == '=' && closeAt > loc[6]:
			return quotedFlagValue(s, open, loc[6], end, n, stringEnd, subs), true
		}
	}
	if loc[3] > loc[2] && quoteLen == 0 {
		// A quote after the name that opened nothing before it.
		return cliValue{}, false
	}
	if list {
		return listValue, true
	}
	return shellWordValue(s, loc[6], loc[7], stringEnd, subs), true
}

// listPasswordValue returns the value after a comma that loc, a
// cliLongPasswordPattern match, found when it is the next element of an
// argument list in code: a quoted flag and a quoted value (["wget",
// "--password", "x"], or [\"wget\", \"--password\", \"x\"] in a JSON
// string), not a name (["wget", "--password", pw]). A value that starts
// with '-' is the next flag where dashFlag is set (["psql", "--password",
// "-h", "db"]), while it is the password of other clients (["wget",
// "--password", "-x"]). Where argv is set, the list names the program it
// runs, so each element is one argument, whatever it holds (["wget",
// "--password", "--x"], ["wget", "--password", "my secret is x"]).
// Elsewhere a value that starts with "--" is the next flag (["--password",
// "--verbose"]), and so is a short option, as a parser declares a flag's
// names (parser.add_argument("--password", "-p", required=True)), and one
// made of prose is text: a word of prose, or several words with a
// grammatical one among them (print("--password", "is required")), while a
// passphrase is not ("correct horse battery staple").
func listPasswordValue(s string, loc []int, dashFlag, argv bool) (cliValue, bool) {
	if loc[3] == loc[2] {
		return cliValue{}, false
	}
	v, ok := listElementValue(s, loc[6])
	if !ok {
		return cliValue{}, false
	}
	value := s[v.start:v.end]
	if value == "" || dashFlag && value[0] == '-' {
		return cliValue{}, false
	}
	return v, argv || !strings.HasPrefix(value, "--") && !isShortOption(value) && !isListProse(value)
}

// isShortOption reports whether value is one short option: a dash and a
// letter or digit (-p).
func isShortOption(value string) bool {
	return len(value) == 2 && value[0] == '-' && isASCIIAlnum(value[1])
}

// listProgram returns the first element of the argument list in code that
// holds the element quoted at s[open] (a quoted --password), with its
// quotes, up to maxCodeElements elements back: the program the list runs
// (["psql", "--password", ...]). It is "" when the element at open is the
// first, or when no list start is found.
func listProgram(s string, open int) string {
	if elements := listElementsBefore(s, open); len(elements) > 0 {
		return elements[0]
	}
	return ""
}

// listElementsBefore returns the elements of the argument list in code
// that holds the element quoted at s[open], before it and with their
// quotes, from the first, up to maxCodeElements back. It returns none when
// the element at open is the first, or when no list start is found.
func listElementsBefore(s string, open int) []string {
	if open > 0 && s[open-1] == '\\' {
		open--
	}
	code := s[:open-pythonPrefixBefore(s, open)]
	var elements []string
	for range maxCodeElements {
		code = strings.TrimRight(code, " \t\r\n")
		switch {
		case code == "":
			return nil
		case strings.IndexByte("[(", code[len(code)-1]) >= 0 || isArrayLiteralOpen(code):
			slices.Reverse(elements)
			return elements
		case code[len(code)-1] != ',':
			return nil
		}
		code = strings.TrimRight(code[:len(code)-1], " \t\r\n")
		element, ok := codeElementStart(code)
		if !ok {
			return nil
		}
		text := code[len(element):]
		code, elements = element, append(elements, text[pythonStringPrefix(text, 0):])
	}
	return nil
}

// isArrayLiteralOpen reports whether code ends with the brace that opens
// an array or slice literal: after a type that ends with [] ([]string{,
// String[] {, new[] {), not a Python set or a JSON object.
func isArrayLiteralOpen(code string) bool {
	before, ok := strings.CutSuffix(code, "{")
	if !ok {
		return false
	}
	before = strings.TrimRight(strings.TrimRight(before, " \t"), "_.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	return strings.HasSuffix(before, "[]")
}

// isListWrapperOperand reports whether the client name at start is an
// element of an argument list in code that runs a wrapper, and is the
// destination of an ssh it runs (["ssh", "mysql", "-p2222"], where -p2222
// is ssh's port) or the argument of a wrapper's option (["sudo", "-u",
// "mysql", "ssh", "-p2222"]), as isWrapperOperand reads a command line.
func isListWrapperOperand(s string, start int) bool {
	if start == 0 || s[start-1] != '"' && s[start-1] != '\'' {
		return false
	}
	elements := listElementsBefore(s, start-1)
	if len(elements) == 0 {
		return false
	}
	wrapper, optionArg := listProgramName(elements[0]), false
	if _, ok := wrapperArgOptions[wrapper]; !ok {
		return false
	}
	for _, element := range elements[1:] {
		name := strings.Trim(element, `"'\`)
		_, runsWrapper := wrapperArgOptions[listProgramName(element)]
		switch {
		case optionArg:
			optionArg = false
		case strings.HasPrefix(name, "-") && runsArgument(wrapper, name):
			// env -S, --split-string and -iS run their argument
			// (["env", "--split-string", "mysql", "-pX"]), which may be
			// another wrapper with options of its own, so the element
			// after it is read as the start of a command.
		case strings.HasPrefix(name, "-"):
			// Long and grouped options take an argument too (["sudo",
			// "--user", "ssh", "mysql", "-pX"]); one already joined to
			// its value ("--user=ssh") does not take the next element.
			if _, _, glued := strings.Cut(name, "="); glued {
				optionArg = false
			} else {
				optionArg = takesArgument(wrapper, name)
			}
		case runsWrapper && wrapper != sshCommand:
			// The command it runs is another wrapper (["sudo", "ssh", ...]).
			wrapper = listProgramName(element)
		default:
			// The destination, or the command run, comes before it.
			return false
		}
	}
	return optionArg || wrapper == sshCommand
}

// isListProgram reports whether element, the first element of an argument
// list (see listProgram), is a string that names a program: one word that
// starts with no '-' ("wget", "/usr/bin/wget"), in lower case unless it is
// a path or has an extension ("C:\\Tools\\Wget.exe"), not a name (cmd), a
// label ("Usage:") or a word of prose (print("Use", "--password", "to set
// it")).
func isListProgram(element string) bool {
	if element == "" || strings.IndexByte(`"'\`, element[0]) < 0 {
		return false
	}
	name := strings.Trim(element, `"'\`)
	if name == "" || name[0] == '-' || strings.ContainsAny(name, " \t") || strings.HasSuffix(name, ":") || slices.Contains(proseWords, name) {
		return false
	}
	return strings.ContainsAny(name, `/\.`) || strings.ToLower(name) == name
}

// isNoValueClientName reports whether element, a quoted element of code,
// names a client whose --password takes no value, with or without its
// directory and .exe ("psql", "/usr/bin/psql").
func isNoValueClientName(element string) bool {
	name := listProgramName(element)
	for _, f := range cliPasswordFlags {
		if !f.noValueLong {
			continue
		}
		if m := f.command.FindStringIndex(name); m != nil && m[0] == 0 && m[1] == len(name) {
			return true
		}
	}
	return false
}

// listProgramName returns the name of the program that element, a quoted
// element of code, names, without its quotes, directory and .exe
// ("/usr/bin/psql" is psql).
func listProgramName(element string) string {
	name := strings.Trim(element, `"'\`)
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	if ext := len(name) - len(".exe"); ext > 0 && strings.EqualFold(name[ext:], ".exe") {
		name = name[:ext]
	}
	return name
}

// isListProse reports whether value, an element of an argument list after
// a long password flag, is prose (see listPasswordValue).
func isListProse(value string) bool {
	words := strings.Fields(value)
	prose := 0
	for _, w := range words {
		switch {
		case slices.Contains(proseNouns, w):
			prose++
		case slices.Contains(proseWords, w):
			if len(words) > 1 {
				return true
			}
			prose++
		}
	}
	return prose == len(words)
}

// appendPasswords adds the value of every password flag in the segment of
// each command f defines. A command inside a quoted argument or a command
// substitution of an earlier one, at any depth (docker exec db sh -c
// 'mysql -pX', mysql -h $(cat h; mysql -pX)), runs on its own, so its
// segment ends with that argument and the flags in it are not the earlier
// command's; any other command in an earlier segment was read with it.
func (f cliPasswordFlag) appendPasswords(regions []taggedRegion, s string, noValue *noValueFlags) []taggedRegion {
	if !slices.ContainsFunc(f.needles, func(n string) bool { return strings.Contains(s, n) }) {
		return regions
	}
	ownLong := f.readsOwnLong() && mayHoldLongPasswordFlag(s)
	lines := quoteScan{s: s, prose: true}
	var nested nestedScan
	var paths programPathMemo
	scripts := commandStart{lenient: true}
	var entrypoint entrypointArgs
	var entrypointStarts commandStart
	var long, operands wrapperStarts
	// A segment cut at a ")" (ssh $(mysql -Nse 'x') -p2222) goes on after
	// it, to where uncutEnds keeps for its string; a command there starts
	// a segment that ends there too, which is not found again, also after
	// commands in other strings between them ()'sshpass )'sshpass ...).
	segmentEnd := 0
	uncutEnds := segmentEnds{}
	// A name inside a password found before is part of that word
	// (mysql -p$(printf x; mysql -py), and is not read again.
	var found foundPasswords
	subs := substitutionMemo{}
	reads := &lineReads{s: s, subs: subs}
	for _, cmd := range f.command.FindAllStringIndex(s, -1) {
		if found.covers(cmd[0]) || !isCommandToken(s, cmd[0], cmd[1]) || isContainerNameArgument(s, cmd[0]) || isFunctionName(s, cmd[0], cmd[1]) || f.runsOtherSubcommand(s[cmd[0]:cmd[1]]) {
			continue
		}
		// The closing quote of a string the command is quoted in is not
		// an opening one: "/usr/bin/mysql" -p"x" or sh -c "mysql ...".
		closeAt, stringEnd := lines.enclosing(cmd[0])
		cmdFrom := lines.commandFrom(closeAt)
		if isWrapperOperand(s, &long, lines.wordFrom(closeAt), cmd[0]) || closeAt >= 0 && isPrintedInScript(s, &lines, &scripts, cmd[0]) || isOperandOfCommand(s, &operands, cmdFrom, cmd[0]) {
			continue
		}
		skip, skipEnd := commandCloseQuote(s, cmd, closeAt, stringEnd)
		var rest string
		switch {
		case cmd[0] >= segmentEnd:
			nested = newNestedScan(s, cmd[1], closeAt, stringEnd, reads)
			uncutEnd := uncutEnds.at(closeAt, cmd[1], func() int {
				return cmd[1] + len(f.segmentRest(s, &lines, &paths, reads, cmd, closeAt, stringEnd))
			})
			rest = untilUnmatchedParen(s[cmd[1]:uncutEnd], closeAt-cmd[1], stringEnd-cmd[1], caseMemo{anyHeight: true, lines: reads, base: cmd[1]})
			segmentEnd = cmd[1] + len(rest)
		case f.leading:
			// Only the options right after the name are read, and they
			// end inside the segment already found, so it is not scanned
			// again for every name, which keeps a line of many names
			// linear.
			rest = s[cmd[1]:max(segmentEnd, cmd[1])]
		default:
			var ok bool
			if rest, skip, skipEnd, ok = nested.rest(f, s, cmd, closeAt, stringEnd); !ok {
				continue
			}
		}
		if f.flag == nil && !ownLong {
			continue
		}
		from := cmd[1]
		if isEntrypointArgument(s, &entrypointStarts, cmdFrom, cmd[0]) {
			from = entrypoint.at(s, cmd[0], cmd[1]+len(rest), stringEnd)
		}
		quotes := quoteScan{s: s[:cmd[1]+len(rest)], i: from, skip: skip, skipEnd: skipEnd, substitutions: true, backticks: true, cases: caseMemo{lines: reads}}
		if ownLong {
			own := quotes
			noValue.read(s, &own, cmdFrom, cmd[0], from, cmd[1]+len(rest), f.sourceLong)
		}
		if f.flag == nil {
			continue
		}
		n := len(regions)
		regions = f.appendFlags(regions, s, &quotes, s[from:cmd[1]+len(rest)], stringEnd, subs)
		for _, r := range regions[n:] {
			heap.Push(&found.ahead, r.region)
		}
	}
	return regions
}

// segmentEnds keeps, for each string that commands are quoted in, by where
// its closing quote starts (-1 for none), where the uncut segment of the
// last command read in it ends.
type segmentEnds map[int]int

// at returns where the uncut segment of the command whose name ends at
// from, in the string whose closing quote starts at closeAt, ends: the end
// kept for that string when the name is before it, else the one read
// returns, which is kept.
func (e segmentEnds) at(closeAt, from int, read func() int) int {
	if end, ok := e[closeAt]; ok && from < end {
		return end
	}
	end := read()
	e[closeAt] = end
	return end
}

// runsOtherSubcommand reports whether cmd, a match of f.command, is a login
// client's whose global options took a word after one that takes no value,
// or after one joined to its value, as its value. That word is the client's
// subcommand, so the client runs no login (docker --debug run --name login
// -p 8080:80, docker --config=/tmp/c run --name login -p8080:80).
func (f cliPasswordFlag) runsOtherSubcommand(cmd string) bool {
	if f.needles[0] != loginCommand {
		return false
	}
	words := clientWords(cmd)
	for i := 0; i+1 < len(words); i++ {
		if !cliNoValueGlobalOption.MatchString(words[i]) {
			continue
		}
		// An option may be quoted whole (docker --debug "--config=/c").
		next := words[i+1]
		if !strings.HasPrefix(strings.TrimLeft(next, `"'\`), "-") && next != loginCommand && next != "registry" {
			return true
		}
	}
	return false
}

// clientWords returns the words after the client's name in cmd, a match of
// a login client's command: the elements of an argument list in code, or
// else the shell words of a command line, each with its quotes removed when
// it is no more than a quoted name ("--debug", \"run\").
func clientWords(cmd string) []string {
	rest := cmd[len(cliClientName.FindString(cmd)):]
	var words []string
	if cliListElement.MatchString(rest) {
		for m := cliListElement.FindStringSubmatchIndex(rest); m != nil; m = cliListElement.FindStringSubmatchIndex(rest) {
			words = append(words, rest[m[2]:m[3]])
			rest = rest[m[1]:]
		}
		return words
	}
	if strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "'") {
		rest = rest[1:]
	}
	for m := cliOptionWord.FindStringSubmatchIndex(rest); m != nil; m = cliOptionWord.FindStringSubmatchIndex(rest) {
		words = append(words, plainWord(rest[m[2]:m[3]]))
		rest = rest[m[1]:]
	}
	return words
}

// plainWord returns word without its quotes and backslashes when what is
// left is a plain name or option (-D, --debug, run), and otherwise word.
func plainWord(word string) string {
	plain := strings.NewReplacer(`\`, "", `"`, "", `'`, "").Replace(word)
	for _, c := range plain {
		if c != '-' && c != '_' && c != '.' && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return word
		}
	}
	return plain
}

// commandCloseQuote returns where the quote that closes the string a
// command is quoted in starts and ends, given the one after its name at
// closeAt, which ends at stringEnd (-1 for none). A client and its
// subcommand may be elements of an argument list in code (["docker",
// "login", ...]), and then the quote after the subcommand closes the last
// of them.
func commandCloseQuote(s string, cmd []int, closeAt, stringEnd int) (int, int) {
	if closeAt >= 0 && closeAt < cmd[1] && strings.HasPrefix(s[cmd[1]:], s[closeAt:stringEnd]) {
		return cmd[1], cmd[1] + stringEnd - closeAt
	}
	return closeAt, stringEnd
}

// noValueFlags holds the ends of the --password and --passwd flags that a
// client whose --password takes no value (see noValueLong) reads itself: a
// dash-led word after one of them is its next flag. sources holds those of
// the --pass and --password flags that take a password source (see
// sourceLong).
type noValueFlags struct {
	ends    map[int]bool
	sources map[int]bool
	// starts tells whether a client's name starts a command.
	starts commandStart
}

// read adds the flags in s[from:end], the segment of a client whose name
// is at name in a command from cmdFrom, that are its own words: not in a
// quoted string or a command substitution that another command reads
// (psql -c "$(wget --password -x)"). quotes scans that segment as in
// appendFlags. A client reads none where its name does not start a
// command, as another's argument (docker login -u mysql --password -x,
// wget --user mysql --password -x). With source, they go in sources.
func (n *noValueFlags) read(s string, quotes *quoteScan, cmdFrom, name, from, end int, source bool) {
	if !n.starts.at(s, cmdFrom, name) {
		return
	}
	for pos := from; pos < end; {
		loc := cliLongPasswordPattern.FindStringSubmatchIndex(s[pos:end])
		if loc == nil {
			return
		}
		if ok, _ := longPasswordName(s[pos:end], loc); !ok {
			pos += loc[2]
			continue
		}
		// An escaped quote around the flag (sh -c "psql \"--password\" -h
		// db") is read from its backslash.
		at := pos + loc[0]
		if at > 0 && s[at-1] == '\\' {
			at--
		}
		if _, quoted := quotes.stringAt(at); !quoted {
			if _, _, inner := quotes.substitutionAt(at); !inner {
				n.add(pos+loc[2], source)
			}
		}
		pos += loc[2]
	}
}

// add records a flag that ends at i, in sources with source.
func (n *noValueFlags) add(i int, source bool) {
	ends := &n.ends
	if source {
		ends = &n.sources
	}
	if *ends == nil {
		*ends = map[int]bool{}
	}
	(*ends)[i] = true
}

// has reports whether read found a flag that ends at i, other than a source
// flag.
func (n *noValueFlags) has(i int) bool {
	return n.ends[i]
}

// readsOwnLong reports whether f reads its own long password flags (see
// noValueLong and sourceLong).
func (f cliPasswordFlag) readsOwnLong() bool {
	return f.noValueLong || f.sourceLong
}

// isSourceReference reports whether the long flag that loc, a
// cliLongPasswordPattern match in s, found takes a password source (see
// sourceLong) and its value names where the password is: env:, file:,
// fd: or stdin. A pass: source is redacted whole, as a value. program is
// the first element of the argument list in code the flag is an element
// of, "" for none.
func (n *noValueFlags) isSourceReference(s string, loc []int, program string) bool {
	if !n.sources[loc[2]] && listProgramName(program) != "openssl" {
		return false
	}
	v := strings.TrimLeft(s[loc[6]:], `"'\`)
	for _, ref := range []string{"env:", "file:", "fd:"} {
		if len(v) > len(ref) && strings.HasPrefix(v, ref) && !strings.ContainsRune(" \t\r\n\"'\\", rune(v[len(ref)])) {
			return true
		}
	}
	return strings.HasPrefix(v, "stdin") && (len(v) == len("stdin") || !isWordByte(v[len("stdin")]))
}

// foundPasswords tells, for names at increasing positions, whether a name is
// inside a password found for an earlier one. That one may come after names
// that are not in it (docker run --entrypoint sshpass --env
// X=$(sshpass -p x) -p y).
type foundPasswords struct {
	// ahead are the passwords that start after the last name asked about,
	// and end the furthest end of the others.
	ahead regionHeap
	end   int
}

func (f *foundPasswords) covers(p int) bool {
	for len(f.ahead) > 0 && f.ahead[0].start <= p {
		if r, ok := heap.Pop(&f.ahead).(region); ok {
			f.end = max(f.end, r.end)
		}
	}
	return p < f.end
}

// regionHeap orders regions by their start, for container/heap.
type regionHeap []region

func (h *regionHeap) Len() int           { return len(*h) }
func (h *regionHeap) Less(i, j int) bool { return (*h)[i].start < (*h)[j].start }
func (h *regionHeap) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

func (h *regionHeap) Push(x any) {
	if r, ok := x.(region); ok {
		*h = append(*h, r)
	}
}

func (h *regionHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	*h = old[:len(old)-1]
	return r
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
	// reads reads the substitutions that close on a later line.
	reads *lineReads
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
func newNestedScan(s string, i, closeAt, stringEnd int, reads *lineReads) nestedScan {
	return nestedScan{reads: reads, args: []nestedArg{{
		kind: segmentArg,
		from: i,
		to:   len(s),
		scan: quoteScan{s: s, i: i, skip: closeAt, skipEnd: stringEnd, substitutions: true, backticks: true, cases: caseMemo{lines: reads}},
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
		rest = s[cmd[1] : end+len(segmentMatch(pattern, s[end:max(parent.to, end)], caseMemo{anyHeight: true, lines: n.reads, base: end}))]
		parent.readEnd = cmd[1] + len(rest)
		return rest, arg.to, end, true
	}
	if !arg.runsAt(s, p) {
		return "", 0, 0, false
	}
	rest = segmentMatch(pattern, s[cmd[1]:max(arg.to, cmd[1])], caseMemo{anyHeight: true, lines: n.reads, base: cmd[1]})
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
			// $( or <( opens one, or a backtick (see backtickSegment).
			child.kind, child.from, child.to = substitutionArg, start+2, end-1
			if s[start] == '`' {
				child.from = start + 1
			}
			child.starts.lenient = true
		} else {
			return
		}
		child.scan = quoteScan{s: s[:child.to], i: child.from, substitutions: true, backticks: true, cases: caseMemo{lines: n.reads}}
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
// name is at cmd, in a quoted string that closes at closeAt and ends at
// stringEnd (both -1 when it is in none); lines found that string, and paths
// records whether its text is a program path. A command
// quoted with its arguments (sh -c 'mysql --version'; ssh -p2222 host) ends
// with the string, and so does a name at the end of text that runs no
// command (Run "connect mysql" through ssh -p2222 host,
// ssh -o "HostName mysql" -p2222 host); a quoted program path
// ("/usr/bin/mysql" -pX, or "/opt/mysql tools/bin/mysql" -pX, where the
// name first matched a directory) or command (ssh db "sudo mysql" -pX)
// goes on after it.
func (f cliPasswordFlag) segmentRest(s string, lines *quoteScan, paths *programPathMemo, reads *lineReads, cmd []int, closeAt, stringEnd int) string {
	pattern := segmentRestPattern(s, closeAt)
	i := cmd[1]
	switch {
	case closeAt < i:
		return backtickSegment(pattern, s, cmd[0], i, len(s), reads, true)
	case paths.blankAfter(s, lines.inner(), i, closeAt) && !paths.isProgramPath(f, s, lines, closeAt):
		rest := backtickSegment(pattern, s, cmd[0], i, closeAt, reads, false)
		if i+len(rest) == closeAt && paths.args.joins(s, lines.sep, lines.start) {
			// eval and ssh join the string to the words after it into the
			// command they run (eval "openssl enc" -pass pass:x), so a
			// segment that the string does not end goes on past it.
			return s[i:paths.segmentEnd(s, stringEnd, reads)]
		}
		return rest
	case paths.endsText(f, s, lines, cmd[0], closeAt):
		return s[i:closeAt]
	default:
		return s[i:paths.segmentEnd(s, stringEnd, reads)]
	}
}

// backtickSegment returns the rest of the pipeline segment of the command
// whose name is at p, which goes on at i and ends by end, as segmentMatch
// reads it with pattern, carried on past each command substitution in
// backticks that closes on its line (openssl enc -in `printf f` -pass
// pass:x), as the shell reads it. In Markdown code, which an odd number of
// backticks before the name on its line opens, a backtick ends the segment
// instead (`openssl enc -in f`, then `ls -pass x`), and so does one that a
// blank or punctuation follows (see backtickEnd). acrossLines reads
// substitutions that do not close on their line with reads (see caseMemo).
func backtickSegment(pattern *regexp.Regexp, s string, p, i, end int, reads *lineReads, acrossLines bool) string {
	memo := func(at int) caseMemo {
		if !acrossLines {
			return caseMemo{anyHeight: true}
		}
		return caseMemo{anyHeight: true, lines: reads, base: at}
	}
	j := i + len(segmentMatch(pattern, s[i:end], memo(i)))
	for j+1 < end && s[j] == '`' && strings.IndexByte(codeSpanFollowers, s[j+1]) < 0 && !oddTicksBefore(s, p, reads.subs, nameTickKey) {
		at, closed := backtickClose(s, j, end)
		if !closed {
			break
		}
		k := at + 1
		j = k + len(segmentMatch(pattern, s[k:end], memo(k)))
	}
	return s[i:j]
}

// segmentMatch returns the rest of a pipeline segment that pattern matches
// at the start of text, carried on past each command substitution that
// pattern ended inside, at a ";;" of a case statement
// ($(case x in x) printf h;; esac)), also one whose parentheses nest deeper
// than maxSubstitutionHeight, or at a line break of one that cases reads to
// a later line (openssl enc -in $(printf f;<newline>true) -pass pass:x), and
// past each parameter expansion it ended inside, whose braces nest deeper
// than it reads (${H:-a;${B:-${C:-b}}}). cases reads the substitutions of
// text.
func segmentMatch(pattern *regexp.Regexp, text string, cases caseMemo) string {
	n := len(pattern.FindString(text))
	if !strings.Contains(text[:n], caseKeyword) && !strings.Contains(text[:n], "${") && !cases.mayOpenLine(text, n) {
		return text[:n]
	}
	q := quoteScan{s: text, substitutions: true, braces: true, cases: cases}
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
	// starts reads whether names in the text run as commands, and args
	// whether the string is part of a command that ssh or eval runs.
	starts commandStart
	args   joinedArgs
	// goesOnFrom is where the segment that the last command going on past
	// its string was read in goes on, and goesOnEnd where it ends, 0 when
	// none was read.
	goesOnFrom, goesOnEnd int
}

// segmentEnd returns the end of the pipeline segment that goes on at from,
// after the quoted string a command is in, as cliSegmentRest reads it. A
// string inside the segment read last ends in that segment, so its end is
// reused (f("mysql", "-h", h) + f("mysql", "-h", h) + ..., where each
// command's rest ends at its ")"), which keeps many such strings linear.
// reads reads the substitutions of s.
func (m *programPathMemo) segmentEnd(s string, from int, reads *lineReads) int {
	if m.goesOnEnd == 0 || from < m.goesOnFrom || from > m.goesOnEnd {
		m.goesOnFrom = from
		end := from
		for {
			end += len(segmentMatch(cliSegmentRest, s[end:], caseMemo{anyHeight: true, lines: reads, base: end}))
			// A raw string, as Go writes one, is an element of the list
			// too (exec.Command("redis-cli", "-a", `x`, "ping")).
			next := rawListElementEnd(s, end)
			if next < 0 {
				break
			}
			end = next
		}
		m.goesOnEnd = end
	}
	return m.goesOnEnd
}

// rawListElementEnd returns where the raw string that opens at s[at] ends,
// when it is an element of an argument list in code: a comma comes before
// it, and a comma or a closing bracket after it. It returns -1 otherwise.
func rawListElementEnd(s string, at int) int {
	if at >= len(s) || s[at] != '`' || !strings.HasSuffix(strings.TrimRight(s[:at], " \t"), ",") {
		return -1
	}
	if end := rawStringEnd(s, at); end > 0 && endsListElement(s, end) {
		return end
	}
	return -1
}

// rawStringEnd returns where the raw string that opens at s[at], as Go
// writes one, ends, after its closing backtick, or -1 when none closes it.
// Its text may go on across lines (exec.Command("wget", "--password",
// `a<newline>b`, url)), but then only to a backtick that ends an element of
// a list, which a comma or a closing bracket follows, so that a backtick
// of Markdown code reads no later line.
func rawStringEnd(s string, at int) int {
	k := strings.IndexByte(s[at+1:], '`')
	if k < 0 {
		return -1
	}
	end := at + k + 2
	if strings.ContainsAny(s[at+1:end-1], "\r\n") && !endsListElement(s, end) {
		return -1
	}
	return end
}

// endsListElement reports whether a comma or a closing bracket follows
// s[:end], after blanks, line breaks or comments, as one does an element
// of a list in code.
func endsListElement(s string, end int) bool {
	after := skipListGap(s, end)
	return after < len(s) && strings.IndexByte(",)]", s[after]) >= 0
}

// skipListGap returns the index after blanks, line breaks, # or // comments
// and /* */ block comments starting at i, as may come between elements of
// an argument list in code. An unclosed block comment is left in place.
func skipListGap(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			if j := strings.IndexAny(s[i:], "\r\n"); j >= 0 {
				i += j
			} else {
				return len(s)
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				if j := strings.IndexAny(s[i:], "\r\n"); j >= 0 {
					i += j
					continue
				}
				return len(s)
			}
			if i+1 < len(s) && s[i+1] == '*' {
				end := strings.Index(s[i+2:], "*/")
				if end < 0 {
					return i
				}
				i += end + 4
				continue
			}
			return i
		default:
			return i
		}
	}
	return i
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

// endsText reports whether the client name at p ends the text of the quoted
// string that lines last found, which closes at to, as a word of text that
// runs no command: its text holds a blank before the name, is no program
// path, no word before the name lets it run (sudo mysql, cd app && mysql),
// and the string is no argument that ssh or eval joins into the command it
// runs (ssh db "nice mysql" -pX, eval "nice mysql" -pX), as text around a
// command is not (Run "connect mysql" through ssh -p2222 host), nor the
// value of one of ssh's options (ssh -o "HostName mysql" -p2222 host).
// blankAfter has read the text.
func (m *programPathMemo) endsText(f cliPasswordFlag, s string, lines *quoteScan, p, to int) bool {
	from := lines.inner()
	return m.blank >= from && m.blank < p && !m.args.joins(s, lines.sep, lines.start) &&
		!m.isProgramPath(f, s, lines, to) && !m.starts.at(s, from, p)
}

// joinedArgs reads the words of a command up to a quoted string in it for
// an ssh, which joins the words after its destination into the command it
// runs (ssh db "nice mysql" -pX), whatever runs ssh (nice ssh db ...), or an
// eval, which joins all of them. It reads a command's text once for strings
// at increasing positions in it.
type joinedArgs struct {
	// from is where the command's text starts, i where the word being read
	// starts, and j how far it was read.
	from, i, j int
	// ssh is set after a word that names ssh, dest after its destination,
	// command after the first word of the command it runs, or all three
	// after eval, and optionArg after an option of ssh that takes the next
	// word. env is set after a word that names env, and split after its
	// option -S, which takes the next word as the command it splits into
	// words. unknown is set when the
	// command may start before from: after a ")" in the text, which closes
	// a substitution from is in (ssh $(hostname) "nice mysql" -pX), or
	// after a closing backtick or a continued line break.
	ssh, dest, command, optionArg, env, split, unknown bool
}

// joins reports whether the quoted string at start, in the command whose
// text starts at from, is an argument that ssh or eval joins into the
// command it runs, and no value of one of ssh's options (ssh db -o
// "HostName mysql" -p2222, ssh db -o"HostName mysql" -p2222), or may be one.
func (a *joinedArgs) joins(s string, from, start int) bool {
	if from > start {
		return true
	}
	a.read(s, from, start)
	switch {
	case a.unknown:
		return true
	case !a.dest:
		return false
	case a.command:
		return true
	case a.optionArg:
		return false
	}
	// A word joined to the string may be an option that takes it.
	part := s[a.i:start]
	return !strings.HasPrefix(part, "-") || len(part) > maxOptionWord || !takesArgument(sshCommand, part)
}

// maxOptionWord bounds the length of an option joined to a string that
// joins reads.
const maxOptionWord = 64

// read reads the words of s from from up to p, where a quoted string
// starts; a word joined to it starts at a.i.
func (a *joinedArgs) read(s string, from, p int) {
	if a.from != from || p < a.i {
		*a = joinedArgs{from: from, i: from, j: from, unknown: startsMidCommand(s, from)}
	}
	for a.j < p {
		if end, quoteLen := quotedPiece(s[:p], a.j); quoteLen > 0 {
			a.j = end
			continue
		}
		switch s[a.j] {
		case ' ', '\t', '\r', '\n':
			if a.i < a.j {
				a.word(s[a.i:a.j])
			}
			a.i = a.j + 1
		case ')':
			a.unknown = true
		case '\\':
			a.j++
		}
		a.j++
	}
}

// word reads one word of the command. After env and before ssh, the
// string env -S splits into words is read as those words
// (env -S 'ssh db' "nice mysql" -pX, env -S'ssh db' ...,
// env --split-string='ssh db' ...).
func (a *joinedArgs) word(word string) {
	if a.ssh || !a.env {
		a.step(word)
		return
	}
	shellWords(word, func(arg string) {
		switch {
		case a.split:
			a.split = false
		case strings.HasPrefix(arg, "-"):
			line, ok := joinedSplitString(envCommand, arg)
			a.split = !ok && takesArgument(envCommand, arg) && runsArgument(envCommand, arg)
			if !ok {
				return
			}
			arg = line
		default:
			a.step(arg)
			return
		}
		shellWords(arg, a.step)
	})
}

// step reads one word of the command as a single word.
func (a *joinedArgs) step(word string) {
	switch {
	case a.optionArg:
		a.optionArg = false
	case a.command:
	case !a.ssh:
		name := programName(word)
		a.ssh, a.env = name == sshCommand, a.env || name == envCommand
		if name == evalCommand {
			a.ssh, a.dest, a.command = true, true, true
		}
	case strings.HasPrefix(word, "-"):
		// ssh reads its options also after its destination
		// (ssh db -p 2222 mysql -pX).
		a.optionArg = takesArgument(sshCommand, word)
	case a.dest:
		a.command = true
	default:
		a.dest = true
	}
}

// startsMidCommand reports whether the command whose text the last shell
// operator before from starts may start before it: from follows a closing
// backtick and a blank (ssh `hostname` "nice mysql" -pX), or a line break
// that a backslash continues.
func startsMidCommand(s string, from int) bool {
	switch {
	case from == 0 || from >= len(s):
		return false
	case s[from-1] == '`':
		return s[from] == ' ' || s[from] == '\t'
	case s[from-1] == '\n':
		return !isCommandSeparator(s, from-1)
	}
	return false
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
// cases reads the substitutions of rest.
func untilUnmatchedParen(rest string, skip, skipEnd int, cases caseMemo) string {
	depth := 0
	for i := 0; i < len(rest); i++ {
		if i == skip {
			i = skipEnd - 1
			continue
		}
		if end, quoteLen := cases.quotedPiece(rest, i); quoteLen > 0 {
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
		// A parameter expansion is one word (-in ${INPUT:-a)b}).
		if end := cases.braceEnd(rest, i); end > 0 {
			i = end - 1
			continue
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
// (docker run --entrypoint=mysql, env --split-string=mysql), or an ssh
// option whose value is a command line (ssh -o ProxyCommand=openssl ...).
func hasExecOptionBefore(s string, start int) bool {
	return strings.HasSuffix(s[:start], "--entrypoint=") || strings.HasSuffix(s[:start], "--split-string=") || endsSSHCommandOption(s[:start])
}

// sshCommandOptions are the ssh options whose value is a command line, which
// ssh runs with the user's shell.
var sshCommandOptions = []string{"ProxyCommand", "LocalCommand", "RemoteCommand", "KnownHostsCommand"}

// endsSSHCommandOption reports whether before ends with one of
// sshCommandOptions and the "=" that joins its value, as a word of its own
// or one quoted (ssh -o 'ProxyCommand=...'), or joined to -o
// (ssh -oProxyCommand=...). ssh reads the names in any case.
func endsSSHCommandOption(before string) bool {
	name, ok := strings.CutSuffix(before, "=")
	if !ok {
		return false
	}
	for _, o := range sshCommandOptions {
		k := len(name) - len(o)
		if k >= 0 && strings.EqualFold(name[k:], o) {
			return k == 0 || strings.IndexByte(" \t\"'", name[k-1]) >= 0 || strings.HasSuffix(name[:k], "-o")
		}
	}
	return false
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
	// words, when set, reads a command or process substitution that closes
	// as part of a word (ssh -i $(printf key) mysql), as the shell does,
	// rather than as a command of its own, and finds where each closes.
	words *caseMemo
	// parens counts the parentheses open in a script or a substitution
	// (a substitution, a subshell), across its shell operators, and cases
	// holds the case statements open inside each of them.
	parens int
	cases  [caseLevels]caseLevel
	// tick is the backtick substitution open in a script or a substitution.
	tick tickLevel
}

// tickLevel is a backtick substitution open where commandStart reads. A
// command starts in it (sh -c 'echo `mysql -pX`'), and after the backtick
// that closes it the words read before it, outer, go on
// (sh -c 'echo `date` mysql -p2222').
type tickLevel struct {
	open  bool
	outer commandWords
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
	// latestExec is set after podman's exec, whose -l (--latest) runs in
	// the latest container and so names it (podman exec -l mysql -pX).
	latestExec bool
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
	// noRun is set with text after a container client's subcommand that
	// runs no command, all of whose words are its operands
	// (kubectl patch deployment mysql -p='{...}'), unless optionUnknown
	// is: it is set after a long option of a container client, before its
	// subcommand, that may take the next word as its argument but is not
	// known to (podman --connection build exec db mysql -pX), so that word
	// may be no subcommand.
	noRun         bool
	optionUnknown bool
	// name is the program the command runs, after its wrappers, and
	// operandRuns whether a word after it runs the words that follow
	// (find -exec, go run).
	name        string
	operandRuns bool
}

// at reports whether a command name at p, in the argument of s whose text
// starts at from, is run as a command: every word before it since the last
// shell operator is a shell keyword, a wrapper with its options, or an
// assignment.
func (c *commandStart) at(s string, from, p int) bool {
	if c.from != from || p < c.i {
		*c = commandStart{from: from, i: from, j: from, lenient: c.lenient, words: c.words}
	}
	for c.i < p {
		if c.j == c.i {
			r, size := utf8.DecodeRuneInString(s[c.i:])
			switch {
			case size == 1 && isCommandSeparator(s, c.i):
				c.separate(s, from)
				continue
			case c.isTick(s, c.i):
				c.backtick()
				continue
			case unicode.IsSpace(r):
				c.i += size
				c.j = c.i
				continue
			}
		}
		c.j = c.wordEnd(s, c.j, p)
		if c.j >= p {
			return c.runsBefore(s[c.i:p])
		}
		c.word(s[c.i:c.j])
		c.i = c.j
	}
	return c.runs()
}

// wordEnd returns where the word that goes on at s[j] ends, by p.
func (c *commandStart) wordEnd(s string, j, p int) int {
	for j < p {
		// A quoted string is part of the word (sudo -u "my user").
		if end, quoteLen := quotedPiece(s[:p], j); quoteLen > 0 {
			j = end
			continue
		}
		if c.words != nil {
			if n := c.words.substitutionLen(s, j); n > 0 {
				j += n
				continue
			}
		}
		if s[j] == '\\' && j+1 < p && s[j+1] != '\r' && s[j+1] != '\n' {
			// An escaped character is part of the word, a blank too
			// (ssh -i /tmp/my\ key mysql).
			j += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[j:])
		if unicode.IsSpace(r) || size == 1 && (isCommandSeparator(s, j) || c.isTick(s, j)) {
			break
		}
		j += size
	}
	return j
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
// command starts (;, |, &, the ( of a subshell, or a line break, as in a
// substitution that spans lines), and not part of a redirection (2>&1,
// &>log, >|f) or a line continuation.
func isCommandSeparator(s string, i int) bool {
	switch s[i] {
	case ';', '(':
		// An escaped one is part of a word (ssh -i key\;mysql).
		return !isEscaped(s, i)
	case '&', '|':
		return !isRedirectionOperator(s, i) && !isEscaped(s, i)
	case '\n':
		return !isEscaped(s, i) && (i == 0 || s[i-1] != '\r' || !isEscaped(s, i-1))
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
	next := commandStart{from: from, i: c.i + 1, j: c.i + 1, lenient: c.lenient, words: c.words, parens: c.parens, cases: c.cases, tick: c.tick}
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

// isTick reports whether s[i] is a backtick that opens or closes a command
// substitution where c reads a script or a substitution. In the text of
// another quoted argument a backtick is text (the identifiers of
// mysql -e 'select `mysql`.`user`').
func (c *commandStart) isTick(s string, i int) bool {
	return c.lenient && s[i] == '`' && !isEscaped(s, i)
}

// backtick reads the backtick at c.i: one opens a substitution, where a
// command starts, and the next closes it, after which the words before it go
// on (see tickLevel).
func (c *commandStart) backtick() {
	if c.tick.open {
		c.commandWords, c.tick = c.tick.outer, tickLevel{}
	} else {
		c.tick = tickLevel{open: true, outer: c.commandWords}
		c.commandWords = commandWords{}
	}
	c.i++
	c.j = c.i
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
		*c = commandStart{from: c.from, i: c.i, j: c.j, lenient: true, words: c.words, parens: c.parens, cases: c.cases, tick: c.tick}
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

// latestOption returns option, an option of podman's exec before its
// container, without --latest or -l, which take no value: either one names
// the container, so it sets host, and the next word is the command
// (podman exec -l mysql -pX, podman exec -itl mysql -pX).
func (c *commandStart) latestOption(option string) string {
	if option == "--latest" {
		c.host = true
		return ""
	}
	if strings.HasPrefix(option, "--") {
		return option
	}
	for i := 1; i < len(option); i++ {
		if option[i] == 'l' {
			c.host = true
			return option[:i] + option[i+1:]
		}
		if strings.IndexByte(containerArgOptions, option[i]) >= 0 {
			// The rest of the word is this option's value (-ul is user l).
			break
		}
	}
	return option
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
		// Words after "--" may be a command to run (kubectl ... -- mysql
		// -pX), so they are no longer all operands.
		c.noRun = c.noRun && word != "--"
		c.operandRuns = c.operandRuns || runsOperands(c.name, unquotedWord(word))
	case c.redirectArg:
		c.redirectArg = false
	case c.funcName:
		c.funcName = false
	case c.fresh() && (word == ")" || word == functionKeyword):
		// A function definition, whose body runs commands
		// (f() { mysql -pX; }, function f { mysql -pX; }): the ")" after
		// the "(" that started this reading, or the keyword before the
		// function's name.
		c.funcName = word == functionKeyword
	case isRedirection(word):
		// A redirection runs nothing (2>/dev/null mysql -pX), and an
		// operator alone takes the next word as its target (> out).
		_, c.redirectArg = redirection(word)
	case c.optionArg && c.execArg && c.wrapper == envCommand:
		c.splitString(word)
	case c.optionArg:
		c.optionArg, c.command, c.execArg = false, c.execArg, false
	case c.wrapper != "" && strings.HasPrefix(unquotedWord(word), "-"):
		// An option of the wrapper (sudo -E, env -i), some of which take
		// the next word as their argument (sudo -u root), and a few the
		// command to run (env -S mysql, env -Ssudo). The shell removes
		// its quotes (ssh "-l" mysql).
		if line, ok := joinedSplitString(c.wrapper, word); ok {
			c.splitString(line)
			return
		}
		option := unquotedWord(word)
		if c.latestExec && !c.host {
			option = c.latestOption(option)
		}
		c.optionArg = takesArgument(c.wrapper, option)
		c.execArg = c.optionArg && runsArgument(c.wrapper, option)
		c.optionUnknown = c.optionUnknown || isContainerClient(c.wrapper) && !c.sub && !c.optionArg && mayTakeArgument(option)
	case isContainerClient(c.wrapper) && !c.sub:
		// The subcommand of a container client: exec and run run a
		// command in a container (docker exec db mysql -pX), and the
		// others run none (docker logs mysql). Any other word comes
		// before it: container, compose, or the value of a global option
		// (kubectl --as alice exec).
		// The shell removes a quoted subcommand's quotes (docker "exec").
		switch sub := unquotedWord(word); {
		case slices.Contains(containerRunSubcommands, sub):
			c.sub = true
			c.latestExec = c.wrapper == podmanCommand && sub == execCommand
		case slices.Contains(containerTextSubcommands, sub):
			c.text, c.noRun = true, !c.optionUnknown
		}
	case hasDestination(c.wrapper) && !c.host:
		// The destination of ssh (ssh db mysql -pX), or the container or
		// image a command runs in (docker exec db mysql -pX), even one
		// named like a command, a keyword or a wrapper (ssh echo mysql
		// -pX, docker exec mysql ssh -p2222 h).
		c.host = true
	case isShellKeyword(word):
		c.wrapper, c.host, c.sub, c.latestExec, c.command = "", false, false, false, false
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
		if !c.command {
			c.name = programName(word)
		}
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
	*c = commandStart{from: c.from, i: c.i, j: c.j, lenient: c.lenient, words: c.words, parens: c.parens, cases: c.cases, tick: c.tick}
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
	word = unquotedWord(word)
	name := word[strings.LastIndexAny(word, `/\`)+1:]
	if ext := len(name) - len(".exe"); ext > 0 && strings.EqualFold(name[ext:], ".exe") {
		return name[:ext]
	}
	return name
}

// unquotedWord returns word without its quotes (see wordQuotes).
func unquotedWord(word string) string {
	if strings.ContainsAny(word, `"'`) {
		return wordQuotes.Replace(word)
	}
	return word
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
	case "sudo", execCommand, envCommand, "time", "nohup", "nice", "command", "sshpass", sshCommand:
		return true
	}
	return isContainerClient(word)
}

// isContainerClient reports whether word is a client that runs a command in
// a container after its subcommand, options and container
// (docker exec db mysql -pX, kubectl exec -it db -- mysql -pX).
func isContainerClient(word string) bool {
	switch word {
	case "docker", podmanCommand, "nerdctl", "kubectl":
		return true
	}
	return false
}

// containerRunSubcommands are the subcommands of container clients that run
// a command in a container, and containerTextSubcommands those that run
// none. A group of subcommands whose create runs one (docker service create
// alpine mysql -pX) is in neither, as container is.
var (
	containerRunSubcommands  = []string{execCommand, "run", "create", "debug"}
	containerTextSubcommands = []string{
		"annotate", "api-resources", "api-versions", "apply", "attach", "auth", "autoscale", "build", "builder", "buildx", "certificate", "cluster-info", "commit", "completion", "config", "context", "cordon", "cp", "delete", "describe", "diff", "drain", "edit", "events", "explain", "export", "expose", "get", "history", "image", "images", "import", "info", "inspect", "kill", "kustomize", "label", "load", loginCommand, "logout", "logs", "manifest", "network", "node", "patch", "pause", "plugin", "port", "port-forward", "proxy", "ps", "pull", "push", "rename", "replace", "restart", "rm", "rmi", "rollout", "save", "scale", "search", "set", "stack", "start", "stats", "stop", "swarm", "system", "tag", "taint", "top", "trust", "uncordon", "unpause", "update", "version", "volume", "wait",
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
		"sudo": "CDghpRrTtUu", envCommand: "CSu", "time": "fo", execCommand: "a", "nice": "n", "sshpass": "dfPp", sshCommand: "BbcDEeFIiJLlmOoPpQRSWw",
		"docker": containerArgOptions, podmanCommand: containerArgOptions, "nerdctl": containerArgOptions, "kubectl": containerArgOptions,
	}
	wrapperLongArgOptions = []string{
		"adjustment", "chdir", "chroot", "close-from", "command-timeout", "format", "group", "host", "other-user", "output", "prompt", "role", "split-string", "type", "unset", "user",
		// container clients
		"as", "as-group", "as-uid", "cache-dir", "certificate-authority", "client-certificate", "client-key", "log-dir", "log-file", "password", "profile", "profile-output", "request-timeout", "server", "tls-server-name", "tlscacert", "tlscert", "tlskey", "token", "username",
		"add-host", "ansi", "attach", "cap-add", "cap-drop", "cgroupns", "cidfile", "cluster", "config", "container", "context", "cpu-shares", "cpus", "detach-keys", "device", "dns", "entrypoint", "env", "env-file", "file", "filename", "gpus", "hostname", "ip", "ipc", "kubeconfig", "label", "link", "log-driver", "log-level", "log-opt", "memory", "mount", "name", "namespace", "network", "network-alias", "parallel", "pid", "platform", "progress", "project-directory", "project-name", "publish", "pull", "restart", "runtime", "security-opt", "shm-size", "tmpfs", "ulimit", "volume", "volumes-from", "workdir",
	}
)

// containerFlagOptions are the long options of container clients, before
// their subcommand, known to take no value (kubectl --warnings-as-errors
// patch, docker --debug logs).
var containerFlagOptions = []string{
	"add-dir-header", "all-resources", "alsologtostderr", "compatibility", "debug", "debug-full", "disable-compression", "dry-run", "experimental", "insecure-registry", "insecure-skip-tls-verify", "logtostderr", "match-server-version", "noout", "one-output", "remote", "skip-headers", "skip-log-headers", "syslog", "tls", "tlsverify", "verbose", "warnings-as-errors",
}

// mayTakeArgument reports whether option, a container client's option that
// is not known to take an argument, may still take the next word: a long one
// with no value joined to it, unless it is known to take none.
func mayTakeArgument(option string) bool {
	name, ok := strings.CutPrefix(option, "--")
	return ok && !strings.Contains(name, "=") && !slices.Contains(containerFlagOptions, name)
}

// containerArgOptions are the short options of container clients, before
// and after their subcommand, that take an argument (docker -H host,
// docker run -h web -p 8080:80, kubectl exec -c app -n prod).
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
	if !ok || dir != "" && !strings.HasSuffix(dir, "/") {
		return false
	}
	if wrapper == sshCommand {
		// The value of an option that runs it (ssh -o ProxyCommand=openssl).
		return endsSSHCommandOption(option + "=")
	}
	return strings.HasPrefix(option, "--") && runsArgument(wrapper, option)
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
// quoted string holding the command ends, or -1, and subs is as in
// passwordValueRegion.
func (f cliPasswordFlag) appendFlags(regions []taggedRegion, s string, quotes *quoteScan, rest string, stringEnd int, subs substitutionMemo) []taggedRegion {
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
		if isJoinedFlag(s, base, loc) {
			// The rest of the word is part of it too (-h a,-px,-py).
			pos = joinedFlagEnd(rest, loc)
			continue
		}
		r, ok := taggedRegion{}, false
		if _, quoted := quotes.stringAt(base + loc[0]); !quoted {
			if _, _, inner := quotes.substitutionAt(base + loc[0]); !inner {
				r, ok = f.valueRegion(s, base, loc, stringEnd, subs)
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

// isJoinedFlag reports whether loc, a match of a flag pattern in s from base,
// is a bare short flag after a separator of code, which is joined to the word
// before it (mysql -h db,-px); only a quoted one starts an element of code
// there (["mysql","-px"], see cliShortFlag).
func isJoinedFlag(s string, base int, loc []int) bool {
	return len(loc) >= 8 && loc[2] < 0 && strings.IndexByte(",([", s[base+loc[0]]) >= 0
}

// isListFlag reports whether loc, a match of a flag pattern in s from base,
// is a flag quoted on its own whose value is the next element of an
// argument list in code (["redis-cli", "-a", "x"], see cliShortFlag).
func isListFlag(s string, base int, loc []int) bool {
	return len(loc) >= 8 && loc[5] > loc[4] && s[base+loc[5]] == ',' && loc[len(loc)-2] > loc[5]
}

// listElementValue returns the value in the string of code that opens at
// s[open], an element of an argument list, false when no string opens there
// (a name, such as pw) or when the string is the key of an object, which
// a colon follows ({"literal": "--password", "caseFold": true}). The
// string may have a Python prefix (r"pass:x", see pythonStringPrefix),
// which makes it literal: Python expands no $ (r"$ecret"). Python and C
// join the strings that follow it with only blanks between them into one
// ("hun" "ter2", "" "hunter2"), and the value runs from the first one
// that is not empty to the last one's closing quote, so an option joined
// to empty strings is read as one ("" "--host").
func listElementValue(s string, open int) (cliValue, bool) {
	prefix := pythonStringPrefix(s, open)
	open += prefix
	end, n := codeStringEnd(s, open)
	if n == 0 && s[open] == '`' {
		// A raw string, as Go writes one (exec.Command("wget",
		// "--password", `x`, url)), whose text is literal.
		if e := rawStringEnd(s, open); e > 0 {
			end, n = e, 1
		}
	}
	if n == 0 {
		return cliValue{}, false
	}
	v := cliValue{start: open + n, end: end - n, quoted: true, singleQuoted: isLiteralCodeString(s, open, prefix)}
	for empty := v.end == v.start; ; {
		next := end + len(s[end:]) - len(strings.TrimLeft(s[end:], " \t\r\n"))
		if next == end || next == len(s) {
			break
		}
		piecePrefix := pythonStringPrefix(s, next)
		pieceEnd, m := codeStringEnd(s, next+piecePrefix)
		if m == 0 {
			break
		}
		if pieceStart := next + piecePrefix + m; empty && pieceEnd-m > pieceStart {
			v.start, v.singleQuoted = pieceStart, isLiteralCodeString(s, next+piecePrefix, piecePrefix)
			empty = false
		}
		if !empty {
			v.end = pieceEnd - m
		}
		end = pieceEnd
	}
	if strings.HasPrefix(strings.TrimLeft(s[end:], " \t\r\n"), ":") {
		return cliValue{}, false
	}
	return v, true
}

// isLiteralCodeString reports whether the string of code whose quote is at
// s[quote], after a Python prefix of length prefix, is literal: a prefixed
// one, or one in single quotes or backticks, which expands no $.
func isLiteralCodeString(s string, quote, prefix int) bool {
	return prefix > 0 || s[quote] != '"' && s[quote] != '\\'
}

// pythonStringPrefix returns the length of the Python string prefix at
// s[i] that a quote, or an escaped one in a JSON string, follows: r, b, u,
// rb or br in either case (r"pass:x", rb'x'), else 0. An f-string's braces
// interpolate (f"{pw}"), so its prefix is not read as one.
func pythonStringPrefix(s string, i int) int {
	for n := 1; n <= 2 && i+n < len(s); n++ {
		if c := s[i+n]; c == '"' || c == '\'' || strings.HasPrefix(s[i+n:], `\"`) {
			switch strings.ToLower(s[i : i+n]) {
			case "r", "b", "u", "rb", "br":
				return n
			}
			return 0
		}
	}
	return 0
}

// pythonPrefixBefore returns the length of the Python string prefix that
// ends at code[i], the opening quote of a string (see pythonStringPrefix),
// where no other word character comes before it, else 0.
func pythonPrefixBefore(code string, i int) int {
	for n := 2; n >= 1; n-- {
		if from := i - n; from >= 0 && (from == 0 || !isWordByte(code[from-1])) && pythonStringPrefix(code, from) == n {
			return n
		}
	}
	return 0
}

// codeStringEnd is quotedPiece for a string in code: a backslash escapes
// the character after it in single quotes too ('abc\'def'), and in a string
// in escaped quotes, which a JSON string holds, an escaped backslash
// escapes the character after it, as the code in the JSON reads it
// ([\"wget\", \"--password\", \"abc\\\"def\"]). Three quotes open a
// Python string that only three close ("""abc"def""", or
// \"\"\"abc\"def\"\"\" in a JSON string), not an empty string and another.
func codeStringEnd(s string, i int) (end, quoteLen int) {
	switch {
	case strings.HasPrefix(s[i:], `"""`) || strings.HasPrefix(s[i:], `'''`):
		quoteLen = 3
	case strings.HasPrefix(s[i:], `\"\"\"`):
		quoteLen = 6
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
		case s[j] != '\\':
		case quoteLen%2 == 0 && strings.HasPrefix(s[j:], `\\\`):
			// \\\" and \\\\ are one escaped character.
			j += 3
		default:
			j++
		}
	}
	return 0, 0
}

// joinedFlagEnd returns where the word of the joined flag that loc found in
// rest ends: at the first blank in the match, since a flag whose value may
// follow a blank took the next word as one (docker login -u a,-p -p x,
// where the real -p comes after it), or else at the match's end, so a long
// word of joined flags is read once.
func joinedFlagEnd(rest string, loc []int) int {
	if k := strings.IndexAny(rest[loc[0]+1:loc[1]], " \t\r\n"); k >= 0 {
		return loc[0] + 1 + k
	}
	return loc[1]
}

// valueRegion returns the password of the flag that loc, a match of f.flag in
// s from base, found. stringEnd and subs are as in appendFlags.
func (f cliPasswordFlag) valueRegion(s string, base int, loc []int, stringEnd int, subs substitutionMemo) (taggedRegion, bool) {
	// A dash-led word after a blank is the next flag of a client whose
	// value is optional; one after "=" is the value (mongodump -p=-x).
	start := base + loc[len(loc)-2]
	nextFlag := f.optional && s[start-1] != '='
	if nextFlag && (len(loc) < 8 || loc[2] < 0) && s[start] == '-' {
		// Its word is not read (see appendLongPasswordFlags).
		return taggedRegion{}, false
	}
	v, ok := shortFlagValue(s, base, loc, stringEnd, subs)
	if !ok || nextFlag && isListFlag(s, base, loc) && strings.HasPrefix(s[v.start:v.end], "-") {
		// The next element of a list is the next flag of such a client
		// too (["mongosh", "-p", "--host", "db"]).
		return taggedRegion{}, false
	}
	if f.prefix != "" {
		var ok bool
		if v, ok = v.trimPrefix(s, f.prefix, stringEnd, subs); !ok {
			return taggedRegion{}, false
		}
	}
	return passwordValueRegion(s, v, stringEnd, subs, nextFlag, false)
}

// shortFlagValue returns the value of the short password flag that loc, a
// match in s from base, found. The shell passes a quoted flag as if it were
// bare: a value in the string that opens the flag ("-pSummer 2026") runs to
// that string's end, and a flag quoted on its own ("-p" x) must close the
// quote that opens it.
func shortFlagValue(s string, base int, loc []int, stringEnd int, subs substitutionMemo) (cliValue, bool) {
	start, end := base+loc[len(loc)-2], base+loc[len(loc)-1]
	if len(loc) < 8 || loc[2] < 0 {
		return shellWordValue(s, start, end, stringEnd, subs), true
	}
	if k := pythonPrefixBefore(s, base+loc[2]); k > 0 && !isCodeElementStart(s[:base+loc[2]-k]) {
		// A Python prefix opens a string of code only; on a command line
		// the shell joins it to the word (mysql r"-px" passes r-px).
		return cliValue{}, false
	}
	closeFrom, closed := base+loc[4], loc[5] > loc[4]
	pieceEnd, quoteLen := flagStringEnd(s, base+loc[2])
	closeAt := pieceEnd - quoteLen
	switch {
	case quoteLen == 0:
		// A quote that closes nothing on its line.
		if closed {
			return cliValue{}, false
		}
		return shellWordValue(s, start, end, stringEnd, subs), true
	case closed && closeAt == closeFrom && isListFlag(s, base, loc):
		// The next element of an argument list, which must be a string
		// (["redis-cli", "-a", "x"], not ["redis-cli", "-a", pw]).
		return listElementValue(s, start)
	case closed && closeAt == closeFrom:
		// A flag quoted on its own ("-p" x, "-p"'x'). In code, "-p"+pw,
		// "-p" + pw and "-p" . $pw join strings, so a value right after the
		// quote must be quoted itself, and a lone operator is none.
		if _, n := quotedPiece(s, start); start == base+loc[5] && n == 0 || isConcatOperator(s[start:end]) {
			return cliValue{}, false
		}
		return shellWordValue(s, start, end, stringEnd, subs), true
	case closed:
		// The quote after the name is the first character of the value
		// ('-p"x y').
		start = closeFrom
	}
	if closeAt <= start {
		// "-p" alone prompts for the password.
		return cliValue{}, false
	}
	v := quotedFlagValue(s, base+loc[2], start, pieceEnd, quoteLen, stringEnd, subs)
	v.singleQuoted = v.singleQuoted || pythonPrefixBefore(s, base+loc[2]) > 0
	return v, true
}

// flagStringEnd is quotedPiece for the string that opens a short flag at
// s[open], or codeStringEnd when that string is an element of an argument
// list in code that names its program, where a backslash escapes a quote
// in single quotes too (['mysql', '-pabc\' x']).
func flagStringEnd(s string, open int) (end, quoteLen int) {
	if p := listProgram(s, open); p != "" && isListProgram(p) {
		if end, quoteLen = codeStringEnd(s, open); quoteLen > 0 {
			return end, quoteLen
		}
	}
	return quotedPiece(s, open)
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
func quotedFlagValue(s string, open, start, end, quoteLen, stringEnd int, subs substitutionMemo) cliValue {
	closeAt := end - quoteLen
	if wordEnd := joinedWordEnd(s, open, end, stringEnd, subs); wordEnd > end {
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
// stringEnd is where the quoted string holding the flag ends, or -1, and
// subs is as in shellWordEnd.
func shellWordValue(s string, start, end, stringEnd int, subs substitutionMemo) cliValue {
	wordEnd := shellWordEnd(s, start, stringEnd, subs)
	if s[start] == '`' && wordEnd <= start+1 {
		// A backtick that opens no substitution ends Markdown code
		// (`mysql -p`), which leaves the flag with no value.
		return cliValue{start: start, end: start}
	}
	if wordEnd <= start {
		if s[start] == '"' && s[end-1] != '"' {
			// A string that does not close, up to a substitution in it
			// (cliFlagValue), holds no value.
			return cliValue{start: start, end: start}
		}
		wordEnd = end
	}
	if strings.HasPrefix(s[start:], "$'") {
		// An ANSI-C quoted string ($'pass:x'), where $ is literal and a
		// backslash escapes a quote ($'a\'b').
		if pieceEnd := ansiCQuoteEnd(s, start); pieceEnd > 0 {
			if end := ansiCWordEnd(s, pieceEnd, stringEnd, subs); end > pieceEnd {
				// The word goes on after the string ($'pass:x\\'y), so it
				// is read from the quote, like 'pass:x'y, whose $ is
				// literal ($'pass:$PASSWORD'"").
				return cliValue{start: start + 1, end: end, singleQuoted: true}
			}
			return cliValue{start: start + 2, end: pieceEnd - 1, quoted: true, singleQuoted: true}
		}
	}
	if pieceEnd, quoteLen := quotedPieceAcrossLines(s, start, stringEnd, subs); quoteLen > 0 {
		inner := cliValue{start: start + quoteLen, end: pieceEnd - quoteLen, quoted: true, singleQuoted: s[start] == '\''}
		inner.gap = inner.start == stringEnd && isStringGap(s[inner.start:inner.end])
		if pieceEnd >= wordEnd || inner.gap || isCodeSeparator(s, start, pieceEnd) {
			// A separator of code ends an element of code
			// (["wget", "--password", "x"], see joinedWordEnd).
			return inner
		}
		// The joined word starts with the text of its first piece that
		// holds any, so 'pass:$PASSWORD'"" and ""'$PASSWORD' are literal,
		// as 'pass:$PASSWORD' is, and ''"$PASSWORD" is not.
		return cliValue{start: start, end: wordEnd, singleQuoted: startsSingleQuoted(s, start, wordEnd)}
	}
	return cliValue{start: start, end: wordEnd}
}

// quotedPieceAcrossLines is quotedPiece, except that in the word of a
// password, which subs marks (see shellWordEnd), a double-quoted string may
// hold a command substitution that goes on to later lines, as the shell
// reads it ("$(printf hunter2<newline>)"). The string holding the command
// ends at stringEnd (-1 for none).
func quotedPieceAcrossLines(s string, i, stringEnd int, subs substitutionMemo) (end, quoteLen int) {
	end, quoteLen = quotedPiece(s, i)
	if subs == nil || s[i] != '"' || quoteLen > 0 && !strings.Contains(s[i:end], "$(") {
		return end, quoteLen
	}
	// A substitution that goes on to later lines may hold quotes of its
	// own, which the piece read within its line closes at
	// ("$(printf "%s" hunter2<newline>)").
	if e, n := doubleQuotedAcrossLines(s, i, stringEnd, subs); n > 0 {
		return e, n
	}
	return end, quoteLen
}

// doubleQuotedAcrossLines reads the double-quoted string at s[i] for
// quotedPieceAcrossLines.
func doubleQuotedAcrossLines(s string, i, stringEnd int, subs substitutionMemo) (end, quoteLen int) {
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '"':
			return j + 1, 1
		case s[j] == '\r' || s[j] == '\n':
			return 0, 0
		case strings.HasPrefix(s[j:], "$("):
			end, closed := substitutionEnd(s, j, stringEnd, subs, true)
			if !closed {
				return 0, 0
			}
			j = end - 1
		case strings.HasPrefix(s[j:], "${"):
			// A parameter expansion has quotes of its own too
			// ("$(printf x)${PW:-"a&b"}"), as in quotedPiece.
			if end := braceExpansionEnd(s, j, subs); end > 0 {
				j = end - 1
			}
		case strings.HasPrefix(s[j:], "\\\r\n"):
			j += 2
		case s[j] == '\\':
			j++
		}
	}
	return 0, 0
}

// emptyPiecesEnd returns where the text of a shell word at s[start:end]
// starts, after the empty quoted pieces it starts with, such as "".
func emptyPiecesEnd(s string, start, end int) int {
	for start < end {
		pieceEnd, quoteLen := quotedPiece(s, start)
		if quoteLen == 0 || pieceEnd > end || pieceEnd-start != 2*quoteLen {
			break
		}
		start = pieceEnd
	}
	return start
}

// startsSingleQuoted reports whether the first piece of the shell word at
// s[start:end] that holds text is in single quotes, which make $ literal.
func startsSingleQuoted(s string, start, end int) bool {
	start = emptyPiecesEnd(s, start, end)
	if start == end || s[start] != '\'' {
		return false
	}
	_, quoteLen := quotedPiece(s, start)
	return quoteLen == 1
}

// joinedWordEnd returns the end of the shell word in which a quoted string,
// opened at open, ends at i. A quote right after it that closes the string
// the command is quoted in, which ends at stringEnd, ends the word
// (sh -c "wget '--password=x'"; echo "ok"), and so does a separator of code
// after a string that is an element of code (["mysql", "-px"],
// exec.Command("mysql", "-px",db), {"arg": "--password=x"}), but not one
// that a shell word goes on after (mysql "-pabc",def).
func joinedWordEnd(s string, open, i, stringEnd int, subs substitutionMemo) int {
	if i+1 == stringEnd && (s[i] == '"' || s[i] == '\'') || isCodeSeparator(s, open, i) {
		return i
	}
	return ansiCWordEnd(s, i, stringEnd, subs)
}

// isCodeSeparator reports whether s[i] is a comma or closing bracket after a
// string, opened at open, that is an element of code: the first one in a
// bracket, or one after other elements (["mysql", "-px"], f(db, "-px")),
// not a shell word (mysql -h db, "-px",y, mysql -h 'db', "-px",y).
func isCodeSeparator(s string, open, i int) bool {
	if i == len(s) || strings.IndexByte(",]}", s[i]) < 0 {
		return false
	}
	return isCodeElementStart(s[:open-pythonPrefixBefore(s, open)])
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
// (f("mysql", db, ...), {"arg": ...}, f("mysql", "db-"+suffix, ...)), one
// to a line or more ([<newline>"wget",<newline>"--password", ...).
func isCodeElementStart(code string) bool {
	for range maxCodeElements {
		code = strings.TrimRight(code, " \t\r\n")
		switch {
		case code == "":
			return false
		case strings.IndexByte("[({", code[len(code)-1]) >= 0:
			return true
		case strings.IndexByte(",:+-*/%", code[len(code)-1]) < 0:
			return false
		}
		code = strings.TrimRight(code[:len(code)-1], " \t\r\n")
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
// that code ends with, or false when it ends with neither. A string's
// Python prefix is part of it (r"C:\Tools\wget.exe").
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
				return code[:i-pythonPrefixBefore(code, i)], true
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
// ends at i. stringEnd and subs are as in shellWordEnd.
func ansiCWordEnd(s string, i, stringEnd int, subs substitutionMemo) int {
	return wordRestEnd(s, i, stringEnd, false, subs)
}

// trimPrefix drops prefix from the start of v, also when v is a word whose
// first quoted piece starts with it ('pass:abc'def). stringEnd is where the
// quoted string holding the flag ends, or -1, and subs is as in
// shellWordEnd.
func (v cliValue) trimPrefix(s, prefix string, stringEnd int, subs substitutionMemo) (cliValue, bool) {
	from, skip := v.start, 0
	if !v.quoted {
		// Empty pieces before the prefix ""'pass:$PASSWORD' join nothing.
		from = emptyPiecesEnd(s, v.start, v.end)
		if from < v.end && (s[from] == '"' || s[from] == '\'') {
			skip = 1
		}
	}
	if !strings.HasPrefix(s[from+skip:v.end], prefix) {
		// The prefix may be joined from pieces ("pass":x, pa"ss":x), and
		// then the rest is read as after a bare one.
		if v.quoted {
			return v, false
		}
		end, quote := joinedPrefixEnd(s, v.start, v.end, prefix)
		if end < 0 {
			return v, false
		}
		if quote != 0 {
			return quotedPrefixRest(s, end, v.end, quote), true
		}
		from, skip = end-len(prefix), 0
		v.singleQuoted = false
	}
	v.start = from + skip + len(prefix)
	if skip == 1 && v.start < v.end && s[v.start] == s[v.start-len(prefix)-1] {
		// The first piece holds only the prefix, so the rest is read as
		// after a bare one: 'pass:'"$PASSWORD" is a reference and
		// "pass:"'$PASSWORD' is literal.
		v.start, v.singleQuoted, skip = v.start+1, false, 0
	}
	if skip == 0 && !v.quoted && v.start < len(s) {
		// The rest is read as a value of its own when it starts with a
		// quote: single quotes make $ literal (pass:'$ecret'), and a
		// string holding a substitution joins the prefix
		// (pass:"$(printf x)"), unless it is the gap after the string the
		// command is quoted in ("openssl enc -pass pass:" + pw).
		if _, quoteLen := quotedPiece(s, v.start); quoteLen > 0 {
			v = shellWordValue(s, v.start, v.start, stringEnd, subs)
		}
	}
	return v, true
}

// joinedPrefixEnd returns where prefix ends in the shell word at
// s[start:end] when the word, its quotes removed as the shell removes them,
// starts with it ("pass":x, pa"ss":x, pass\:x), after a quote that closes
// right after it too, or -1. quote is the quote of the piece it ends in
// when the piece goes on after it (pa"ss:x"), or 0.
func joinedPrefixEnd(s string, start, end int, prefix string) (at int, quote byte) {
	k := 0
	for j := start; j < end; j++ {
		c := s[j]
		if k == len(prefix) {
			switch quote {
			case 0:
				return j, 0
			case c:
				return j + 1, 0
			}
			return j, quote
		}
		switch {
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
			continue
		case c == quote:
			quote = 0
			continue
		case c == '\\' && quote != '\'' && j+1 < end:
			j++
			c = s[j]
		}
		if c != prefix[k] {
			return -1, 0
		}
		k++
	}
	if k == len(prefix) && quote == 0 {
		return end, 0
	}
	return -1, 0
}

// quotedPrefixRest returns the value after a prefix that ends at s[at],
// inside a piece quoted with quote of the shell word that ends at wordEnd
// (pa"ss:x"): the rest of the piece when it ends the word, quoted as the
// piece is, so pa'ss:$PASSWORD' is literal and pa"ss:$PASSWORD" a
// reference, or else the rest of the word.
func quotedPrefixRest(s string, at, wordEnd int, quote byte) cliValue {
	v := cliValue{start: at, end: wordEnd, singleQuoted: quote == '\''}
	for j := at; j < wordEnd; j++ {
		switch {
		case s[j] == '\\' && quote == '"':
			j++
		case s[j] == quote:
			if j+1 == wordEnd {
				v.end, v.quoted = j, true
			}
			return v
		}
	}
	return v
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
// joins the word (mysql -pabc"x;y"). subs, when set, marks the word of a
// password, in which a command substitution goes on, as the shell reads it,
// to the line that closes it (mysql -p$(printf a<newline>printf b)), and
// keeps where the ones read end (see substitutionEnd).
func shellWordEnd(s string, start, stringEnd int, subs substitutionMemo) int {
	return wordRestEnd(s, start, stringEnd, true, subs)
}

// wordRestEnd returns the end of the shell word that goes on at i, which
// is the word's start when atStart is set, as in shellWordEnd.
func wordRestEnd(s string, i, stringEnd int, atStart bool, subs substitutionMemo) int {
	first := i
	for i < len(s) {
		if end, stop := wordPieceEnd(s, i, stringEnd, atStart && i == first, subs); stop {
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
// word's first piece, and subs is as in shellWordEnd.
func wordPieceEnd(s string, i, stringEnd int, first bool, subs substitutionMemo) (end int, stop bool) {
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
	pieceEnd, quoteLen := quotedPieceAcrossLines(s, i, stringEnd, subs)
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
	if end, _ := substitutionEnd(s, i, stringEnd, subs, subs != nil); end > i {
		// A password in one that does not close is part of the word
		// (mysql -p$(printf hunter2), and so is one on the lines that a
		// password's goes on to (mysql -p$(printf a<newline>printf b)).
		return end, false
	}
	if end := braceExpansionEnd(s, i, subs); end > 0 {
		return end, false
	}
	if end := backtickEnd(s, i, stringEnd, subs); end > 0 {
		return end, false
	}
	return i, false
}

// backtickEnd returns the end of the command substitution in backticks that
// starts at s[i] in a shell word (pass:abc`printf hunter2`), after the
// backtick that closes it on its line, before the quote that closes the
// string holding the command, which ends at stringEnd (-1 for none), or 0
// when none starts there. One that does not close on its line runs to the
// end of it, as a $(...) does (mysql -pfoo`printf bar), except in a
// quoted string. A backtick after an odd number of others on its line, or
// followed by a blank, the end of a line, punctuation or another backtick,
// ends Markdown code instead (`openssl enc -pass pass:abc`, then `ls`;
// `mysql -pfoo`/`mysql -pbar`). Only the word of a password, which subs
// marks (see shellWordEnd), counts the backticks before it, and there one
// after an even number of others that a blank follows starts a command
// when it closes on its line (pass:` printf hunter2`); another word
// (docker run --entrypoint mysql -e X=`printf x` image) reads every one
// that a command follows.
func backtickEnd(s string, i, stringEnd int, subs substitutionMemo) int {
	if s[i] != '`' || i+1 == len(s) {
		return 0
	}
	blank := subs != nil && (s[i+1] == ' ' || s[i+1] == '\t')
	if !blank && strings.IndexByte(codeSpanFollowers, s[i+1]) >= 0 || subs != nil && oddTicksBefore(s, i, subs, wordTickKey) {
		return 0
	}
	limit := len(s)
	if i < stringEnd {
		limit = stringEnd
	}
	at, closed := backtickClose(s, i, limit)
	switch {
	case closed:
		return at + 1
	case i < stringEnd || blank:
		return 0
	}
	if subs != nil {
		// In a password, one that closes on a later line goes on to it, as
		// the shell reads it (pass:`printf %s<newline>printf hunter2`).
		if later := laterBacktick(s, at, subs); later >= 0 {
			return later + 1
		}
	}
	return at
}

// backtickLineKey is the stringEnd of the keys under which a
// substitutionMemo keeps, for a line break, where laterBacktick found the
// next backtick.
const backtickLineKey = -5

// laterBacktick returns where the first backtick after s[from], a line
// break, that no backslash escapes is, or -1 when none is. subs keeps the
// answer for each line break read, so the passwords on many lines, each
// with a backtick that closes on none of them, read the text after them
// once.
func laterBacktick(s string, from int, subs substitutionMemo) int {
	key := func(p int) substitutionKey { return substitutionKey{start: p, stringEnd: backtickLineKey} }
	var breaks []int
	found := -1
	for j := from; j < len(s) && found < 0; j++ {
		switch s[j] {
		case '\r', '\n':
			if r, ok := subs[key(j)]; ok {
				found = r.end - 1
				j = len(s)
				continue
			}
			breaks = append(breaks, j)
		case '\\':
			if j+1 < len(s) && s[j+1] != '\r' && s[j+1] != '\n' {
				j++
			}
		case '`':
			found = j
		}
	}
	for _, p := range breaks {
		subs[key(p)] = substitutionResult{end: found + 1}
	}
	return found
}

// backtickClose returns where the command substitution in backticks that
// opens at s[i] stops before limit: at the backtick that closes it, with
// closed set, or at the end of its line or limit. A backslash escapes the
// character after it on the line (`printf x\` -p2222`).
func backtickClose(s string, i, limit int) (at int, closed bool) {
	for j := i + 1; j < limit; j++ {
		switch s[j] {
		case '`':
			return j, true
		case '\r', '\n':
			return j, false
		case '\\':
			if j+1 < limit && s[j+1] != '\r' && s[j+1] != '\n' {
				j++
			}
		}
	}
	return limit, false
}

// codeSpanFollowers are the characters after which a backtick that ends a
// shell word ends Markdown code rather than starting a command substitution.
const codeSpanFollowers = " \t\r\n.,;:!?)]}>'\"`|&<("

// wordTickKey and nameTickKey are the stringEnd of the keys under which a
// substitutionMemo keeps how many backticks its line holds before the
// position oddTicksBefore was last asked about, which is the key's end: in
// password words, and before command names.
const (
	wordTickKey = -3
	nameTickKey = -4
)

// oddTicksBefore reports whether an odd number of backticks that no
// backslash escapes comes before s[i] on its line, so the one at s[i]
// closes Markdown code. A backtick in a quoted string that opens at the
// start of a word and closes before s[i] is a character of that string
// (echo '`'; mysql -pfoo`printf x`), not counted. subs keeps the count under
// the key that tickKey names, so positions asked about in increasing order
// are counted once.
func oddTicksBefore(s string, i int, subs substitutionMemo, tickKey int) bool {
	key := substitutionKey{start: -1, stringEnd: tickKey}
	r, ok := subs[key]
	from, n := r.end, r.height
	if !ok || i < from {
		from, n = strings.LastIndexAny(s[:i], "\r\n")+1, 0
	}
	for j := from; j < i; j++ {
		switch c := s[j]; {
		case c == '\r' || c == '\n':
			n = 0
		case c == '`':
			n++
		case c == '\\':
			j++
		case (c == '\'' || c == '"') && (j == 0 || strings.IndexByte(" \t\r\n;|&(=", s[j-1]) >= 0):
			if end, quoteLen := quotedPiece(s, j); quoteLen > 0 && end <= i {
				j = end - 1
			}
		}
	}
	subs[key] = substitutionResult{end: i, height: n}
	return n%2 == 1
}

// braceKey is the stringEnd of the keys under which a substitutionMemo
// keeps the ends of parameter expansions, which no substitution has.
const braceKey = -2

// braceExpansionEnd returns the end of the parameter expansion that starts
// at s[i] (${PW:-hunter&2}, ${PW:-"a b"}), after its matching brace, or 0
// when none starts there or it does not close on its line. Inside it,
// blanks and shell operators are part of the word, expansions nest at any
// depth (${A:-abc&${B:-${C:-x}}}) while a literal { is text, and a quoted
// string or an escaped character is one piece. braces, when set, keeps the ends found, of the
// braces nested in it too, so a word read again after a value that is no
// password does not read them again.
func braceExpansionEnd(s string, i int, braces substitutionMemo) int {
	return braceReader{braces: braces}.end(s, i)
}

// maxExpansionNesting is how deep parameter expansions, and the quoted
// strings and command substitutions in them, nest at most for braceReader
// to read each in full. Each is read by a call of its own
// (${A:-$(printf ${B:-"${C:-x}"})}), so past that depth a command
// substitution is read as quotedPiece reads one, with cliSubstitution, and
// a quoted string reads no expansion in it: a line that nests them,
// however long, then cannot exhaust the stack and end the process.
const maxExpansionNesting = 32

// braceReader reads parameter expansions for braceExpansionEnd.
type braceReader struct {
	braces substitutionMemo
	// nesting is how many expansions, and quoted strings and command
	// substitutions in them, are around the one read.
	nesting int
	// shallow is set when the command substitutions in it are read as
	// quotedPiece reads one, with cliSubstitution, as they are in a string
	// that quotedPiece reads. quotedPiece is also read inside
	// substitutions, which would otherwise nest without a bound.
	shallow bool
}

func braceKeyAt(p int) substitutionKey {
	return substitutionKey{start: p, stringEnd: braceKey}
}

// in returns the reader of what nests in the part it reads.
func (b braceReader) in() braceReader {
	b.nesting++
	return b
}

// end is braceExpansionEnd.
func (b braceReader) end(s string, i int) int {
	if !strings.HasPrefix(s[i:], "${") {
		return 0
	}
	if b.braces == nil {
		b.braces = substitutionMemo{}
	}
	if r, ok := b.braces[braceKeyAt(i)]; ok {
		return r.end
	}
	open := []int{i}
	for j := i + 2; j < len(s); j++ {
		switch s[j] {
		case '{':
			// Only a nested expansion opens a level: the shell reads a
			// literal { as text (${PW:-a{&b} is a{&b).
			if s[j-1] != '$' {
				continue
			}
			switch r, known := b.braces[braceKeyAt(j-1)]; {
			case !known:
				open = append(open, j-1)
			case r.closed:
				j = r.end - 1
			default:
				j = len(s)
			}
		case '$':
			j = b.substitutionEnd(s, j) - 1
		case '}':
			b.braces[braceKeyAt(open[len(open)-1])] = substitutionResult{end: j + 1, closed: true}
			if open = open[:len(open)-1]; len(open) == 0 {
				return j + 1
			}
		case '\\':
			if j+1 == len(s) || s[j+1] == '\r' || s[j+1] == '\n' {
				j = len(s)
			}
			j++
		case '\'', '"':
			end, quoteLen := b.in().quotedPiece(s, j)
			if quoteLen != 1 {
				j = len(s)
			}
			j = max(j, end-1)
		case '\r', '\n':
			j = len(s)
		}
	}
	for _, p := range open {
		b.braces[braceKeyAt(p)] = substitutionResult{}
	}
	return 0
}

// substitutionEnd returns where the reading of the expansion goes on after
// the $ at s[j]: after the command substitution that starts there, one
// piece whose } closes nothing (${PW:-$(printf a})hunter2}), at the end of
// s when that does not close, or after the $. Its reads are keyed under
// braceKey too, which no ${ shares, as one starts with $(.
func (b braceReader) substitutionEnd(s string, j int) int {
	if !strings.HasPrefix(s[j:], "$(") {
		return j + 1
	}
	if b.shallow || b.nesting >= maxExpansionNesting {
		if m := cliSubstitution.FindStringIndex(s[j:]); m != nil {
			return j + m[1]
		}
		return j + 1
	}
	if r := (substitutionEnds{subs: b.braces, stringEnd: braceKey}).read(s, j, b.nesting+1); r.closed {
		return r.end
	}
	return len(s)
}

// maxInnerQuoteScan bounds how far closesInnerString counts quotes, so a
// long string with many commands is not read again for each one.
const maxInnerQuoteScan = 4096

// closesInnerString reports whether the \" at s[i], in a double-quoted
// string that ends at stringEnd, is a quote of the command inside it that
// closes the string holding a word, rather than one that opens a piece of
// the word, which ends at pieceEnd when quoteLen is not 0. The quotes pair
// up from the string's start, so it closes one when an odd number of them
// follow up to the string's end ("sh -c \"mysql -pX\" && echo ok").
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
	return braceReader{shallow: true}.quotedPiece(s, i)
}

// quotedPiece is quotedPiece for a string that b.nesting parameter
// expansions, and the quoted strings and command substitutions in them, are
// around. It reads the expansions in it with b, none past
// maxExpansionNesting.
func (b braceReader) quotedPiece(s string, i int) (end, quoteLen int) {
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
			// ("$(printf "%s" x)"), and so does a parameter expansion
			// ("${PW:-"a&b"}").
			if m := cliSubstitution.FindStringIndex(s[j:]); m != nil {
				j += m[1] - 1
			}
		case quote == `"` && strings.HasPrefix(s[j:], "${") && b.nesting < maxExpansionNesting:
			if end := b.end(s, j); end > 0 {
				j = end - 1
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
// matching parenthesis, and whether it closes there. A substitution in it
// is read on its own, and the ")" that ends a pattern of a case statement
// does not close the one it is in ($(case x in x) printf hunter2;; esac),
// $(echo $(case x in x) printf a;; esac) hunter2)). One that does not
// close runs, as the shell reads it, to the end of its line, or of s when
// multiline is set, or to the quote that closes the string holding the
// command, which ends at stringEnd (-1 for none), or to a backtick that
// pairs with no other on the line, as one that ends a code span does
// (`mysql -p$(printf hunter2`); end is then where it stops. A backtick that
// pairs with a later one starts a command of its own ($(echo `printf x`)).
// end is 0 when no $(...) starts at i. subs, when set, keeps what the scan
// finds for the queries after it.
func substitutionEnd(s string, i, stringEnd int, subs substitutionMemo, multiline bool) (end int, closed bool) {
	if !strings.HasPrefix(s[i:], "$(") {
		return 0, false
	}
	r := readSubstitution(s, i, stringEnd, subs, multiline)
	return r.end, r.closed
}

// readSubstitution reads the command or process substitution ($(...),
// <(...), >(...)) that starts at s[i] as substitutionEnd does, and returns
// where it ends, whether it closes there, and, when it does, how deep the
// parentheses in it nest, its own counted.
func readSubstitution(s string, i, stringEnd int, subs substitutionMemo, multiline bool) substitutionResult {
	if subs == nil {
		subs = substitutionMemo{}
	}
	return substitutionEnds{subs, stringEnd, multiline}.read(s, i, 0)
}

// read is readSubstitution for a substitution that nesting parameter
// expansions, and the quoted strings and substitutions in them, are around
// (see maxExpansionNesting).
func (e substitutionEnds) read(s string, i, nesting int) substitutionResult {
	subs := e.subs
	scan := substitutionScan{ends: e, nesting: nesting}
	if r, ok := subs[scan.ends.key(i)]; ok {
		return r
	}
	limit := substitutionLimit(s, i, e.stringEnd)
	scan.open(i)
	scan.readsHeredocs = e.multiline
	j := i + 2
	for ; j < limit; j++ {
		if end := substitutionQuoteEnd(s, j, limit); end > 0 {
			j = end - 1
			continue
		}
		switch s[j] {
		case '\r', '\n':
			if !e.multiline {
				return substitutionResult{end: scan.stop(j)}
			}
			j = scan.lineBreak(s, j, limit)
		case '`':
			at, closed := backtickClose(s, j, limit)
			if !closed {
				return substitutionResult{end: scan.stop(j)}
			}
			j = at
		case '\\':
			// An escaped character, or a line break that the shell joins.
			if j++; strings.HasPrefix(s[j:], "\r\n") {
				j++
			}
		case '$', '<', '>':
			next, stop := scan.expansion(s, j, limit)
			if stop {
				return substitutionResult{end: next}
			}
			j = next
		case '(':
			scan.frames[len(scan.frames)-1].open(s, j)
		case ')':
			if scan.close(s, j) {
				return subs[scan.ends.key(i)]
			}
		case caseKeyword[0]:
			if strings.HasPrefix(s[j:], caseKeyword) {
				scan.frames[len(scan.frames)-1].hasCase = true
			}
		case '#':
			j = commentEnd(s, j, limit, scan.frames[len(scan.frames)-1].from)
		}
	}
	return substitutionResult{end: scan.stop(min(j, limit))}
}

// substitutionQuoteEnd returns where the quoted string that opens at s[j],
// in a substitution that ends by limit, ends, or 0 when none opens there. An
// ANSI-C quoted one is read with its escapes, a backslash escaping its
// quote ($(printf %s $'a\')&b')).
func substitutionQuoteEnd(s string, j, limit int) int {
	if strings.HasPrefix(s[j:limit], "$'") {
		if end := ansiCQuoteEnd(s[:limit], j); end > 0 {
			return end
		}
	}
	if pieceEnd, quoteLen := quotedPiece(s, j); quoteLen == 1 {
		return pieceEnd
	}
	return 0
}

// heredoc is a here-document's delimiter word, which ends its body on a
// line of its own, after the tabs that <<- strips.
type heredoc struct {
	word      string
	stripTabs bool
}

// heredocAt returns the here-document whose redirection (<<X, <<-'X',
// <<"X", <<123) starts at s[j], and the end of its delimiter word, or 0 when
// none does before limit. <<< reads a string instead.
func heredocAt(s string, j, limit int) (heredoc, int) {
	if !strings.HasPrefix(s[j:limit], "<<") || strings.HasPrefix(s[j:limit], "<<<") {
		return heredoc{}, 0
	}
	var doc heredoc
	k := j + 2
	if k < limit && s[k] == '-' {
		doc.stripTabs = true
		k++
	}
	for k < limit && (s[k] == ' ' || s[k] == '\t') {
		k++
	}
	end := k
	for end < limit && !isShellWordEnd(s[end]) {
		if pieceEnd, quoteLen := quotedPiece(s[:limit], end); quoteLen == 1 {
			end = pieceEnd
			continue
		}
		if s[end] == '\\' && end+1 < limit && s[end+1] != '\r' && s[end+1] != '\n' {
			// An escaped character is part of the word (<<E\ OF ends at a
			// line E OF).
			end++
		}
		end++
	}
	if end == k {
		return heredoc{}, 0
	}
	doc.word = heredocWord(s[k:end])
	return doc, end
}

// heredocWord returns the delimiter that the word raw of a here-document
// redirection names, its quotes removed as the shell removes them: a
// backslash escapes the next character outside quotes and, in double
// quotes, only $, `, ", \ and a line break, while in single quotes it is
// literal (<<'E\F' ends at a line E\F, <<E\F at EF). An ANSI-C quoted
// piece ($'EOF') is decoded (see ansiCEscape), and a $ before a double
// quote ($"EOF") dropped.
func heredocWord(raw string) string {
	var b strings.Builder
	quote := byte(0)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case quote == 0 && c == '$' && i+1 < len(raw) && (raw[i+1] == '\'' || raw[i+1] == '"'):
			i++
			quote = raw[i]
			if quote == '\'' {
				quote = ansiCQuote
			}
			continue
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
			continue
		case quote == ansiCQuote && c == '\'', quote != 0 && c == quote:
			quote = 0
			continue
		case quote == ansiCQuote && c == '\\' && i+1 < len(raw):
			i++
			var known bool
			if c, known = ansiCEscape(raw[i]); !known {
				b.WriteByte('\\')
			}
		case c == '\\' && i+1 < len(raw) && (quote == 0 || quote == '"' && strings.IndexByte("$`\"\\\n", raw[i+1]) >= 0):
			i++
			c = raw[i]
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ansiCQuote marks an ANSI-C quoted piece ($'...') for heredocWord.
const ansiCQuote = 1

// ansiCEscape returns the character that a backslash and c stand for in an
// ANSI-C quoted string, and whether they stand for one: a line break, tab
// or other control character for n, t, r, a, b, e, f and v, and c itself
// for \', \", \\ and ?. Otherwise the backslash stays, as the shell keeps
// it.
func ansiCEscape(c byte) (byte, bool) {
	if i := strings.IndexByte("ntrabefv", c); i >= 0 {
		return "\n\t\r\a\b\x1b\f\v"[i], true
	}
	return c, strings.IndexByte("'\"\\?", c) >= 0
}

// heredocBodiesEnd returns where the text after the bodies of docs, which
// start at from, starts: after the line that holds only the last one's
// delimiter, or limit when a body runs to it.
func heredocBodiesEnd(s string, from, limit int, docs []heredoc, subs substitutionMemo) int {
	for _, doc := range docs {
		from = heredocBodyEnd(s, from, limit, doc, subs)
	}
	return from
}

// heredocBodyEnd returns where the text after the body of doc, which starts
// at from, starts, as heredocBodiesEnd does. subs keeps the end for each
// line read, so the substitutions in a body that does not end, each read
// on its own, do not read the rest of it again
// ($(cat <<X<newline>$(cat <<X<newline>...).
func heredocBodyEnd(s string, from, limit int, doc heredoc, subs substitutionMemo) int {
	var lines []int
	end := limit
	for p := from; p < limit; {
		if r, ok := subs[substitutionKey{start: p, stringEnd: limit, heredoc: doc}]; ok {
			end = r.end
			break
		}
		lines = append(lines, p)
		lineEnd, next := limit, limit
		if n := strings.IndexByte(s[p:limit], '\n'); n >= 0 {
			lineEnd, next = p+n, p+n+1
		}
		line := strings.TrimSuffix(s[p:lineEnd], "\r")
		if doc.stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == doc.word {
			end = next
			break
		}
		p = next
	}
	for _, p := range lines {
		subs[substitutionKey{start: p, stringEnd: limit, heredoc: doc}] = substitutionResult{end: end}
	}
	return end
}

// commentEnd returns where the comment that the # at s[j] starts ends, at
// its last character before the line break or limit, or j when it starts
// none. A comment runs to the end of its line, so a ")" in it closes nothing
// ($(printf h<newline># ) x<newline>)). The # starts one when it starts a
// word: at from, where the text of the substitution it is in starts, or
// after a blank or a shell operator ($(echo #x) prints nothing, while
// $(echo a#b) and ${#x} hold no comment).
func commentEnd(s string, j, limit, from int) int {
	if !startsComment(s, j, from) {
		return j
	}
	if n := strings.IndexAny(s[j:limit], "\r\n"); n >= 0 {
		return j + n - 1
	}
	return limit - 1
}

// startsComment reports whether the # at s[j] starts a comment, as
// commentEnd reads one.
func startsComment(s string, j, from int) bool {
	return j == from || strings.IndexByte(" \t\r\n;&|(", s[j-1]) >= 0 && !isEscaped(s, j-1)
}

// substitutionLimit returns where the reading of a substitution that starts
// at i stops: at the closing quote of the string holding the command, which
// ends at stringEnd, or at the end of s.
func substitutionLimit(s string, i, stringEnd int) int {
	if i >= stringEnd {
		return len(s)
	}
	if stringEnd >= 2 && strings.HasPrefix(s[stringEnd-2:], `\"`) {
		return stringEnd - 2
	}
	return stringEnd - 1
}

// substitutionScan reads a command substitution for substitutionEnd, with
// the ones in it.
type substitutionScan struct {
	// frames are the substitutions open, the innermost last.
	frames []parenScan
	ends   substitutionEnds
	// heredocs are the here-documents whose bodies start on the next line,
	// read when readsHeredocs is set, as the substitution may go on to it.
	heredocs      []heredoc
	readsHeredocs bool
	// nesting is how many parameter expansions, and the quoted strings and
	// substitutions in them, the substitution is in.
	nesting int
}

// expansion reads the parameter expansion, here-document redirection or
// substitution that starts at s[j], if one does, and returns where the
// reading goes on, before the next character, or, with stop, where the
// outermost substitution stops, as inner does. A parameter expansion is one
// word, whose ")" closes nothing ($(printf %s ${PW:-a)b})).
func (c *substitutionScan) expansion(s string, j, limit int) (next int, stop bool) {
	if end := (braceReader{braces: c.ends.subs, nesting: c.nesting}).end(s[:limit], j); end > 0 {
		return end - 1, false
	}
	if strings.HasPrefix(s[j:limit], "<<<") {
		// A here-string reads the word after it, and its last two < start
		// no here-document ($(cat <<< /dev/null<newline>)).
		return j + 2, false
	}
	if c.readsHeredocs && c.frames[len(c.frames)-1].arithmetic == 0 {
		// Arithmetic ($((x << n)), ((y = x << n))) has no here-documents.
		if doc, end := heredocAt(s, j, limit); end > 0 {
			c.heredocs = append(c.heredocs, doc)
			return end - 1, false
		}
	}
	return c.inner(s, j, limit)
}

// lineBreak reads the line break at s[j], after which the bodies of the
// here-documents redirected on its line start, whose text closes nothing
// ($(cat <<X<newline>a)b<newline>X<newline>)), and returns where the
// reading goes on, before the next character.
func (c *substitutionScan) lineBreak(s string, j, limit int) int {
	if len(c.heredocs) == 0 || s[j] != '\n' {
		return j
	}
	end := heredocBodiesEnd(s, j+1, limit, c.heredocs, c.ends.subs)
	c.heredocs = c.heredocs[:0]
	return end - 1
}

func (c *substitutionScan) open(i int) {
	c.frames = append(c.frames, parenScan{start: i, from: i + 2, depth: 1, height: 1})
}

// inner reads the substitution that starts at s[j], if one does, and
// returns where the reading goes on, before the next character, or, with
// stop, where the outermost one stops: one that does not close keeps the
// ones it is in open.
func (c *substitutionScan) inner(s string, j, limit int) (next int, stop bool) {
	if !isSubstitutionStart(s[:limit], j) {
		return j, false
	}
	if r, ok := c.ends.subs[c.ends.key(j)]; ok {
		if !r.closed {
			return c.stop(r.end), true
		}
		c.frames[len(c.frames)-1].nest(r.height)
		return r.end - 1, false
	}
	c.open(j)
	return j + 1, false
}

// close reads the ")" at s[j] and reports whether it closes the outermost
// substitution.
func (c *substitutionScan) close(s string, j int) bool {
	top := &c.frames[len(c.frames)-1]
	if !top.close(s, j, c.ends) {
		return false
	}
	height := top.height
	c.keep(top.start, substitutionResult{end: j + 1, closed: true, height: height})
	c.frames = c.frames[:len(c.frames)-1]
	if len(c.frames) == 0 {
		return true
	}
	c.frames[len(c.frames)-1].nest(height)
	return false
}

// stop ends the substitutions open, which do not close, at end.
func (c *substitutionScan) stop(end int) int {
	for _, f := range c.frames {
		c.keep(f.start, substitutionResult{end: end})
	}
	return end
}

func (c *substitutionScan) keep(i int, r substitutionResult) {
	c.ends.subs[c.ends.key(i)] = r
}

// parenScan reads the parentheses of one command substitution for
// substitutionScan.
type parenScan struct {
	cases caseScan
	// hasCase is set once the text of the substitution, outside the ones
	// in it, holds the case keyword, and readCases once its case
	// statements are read.
	hasCase, readCases bool
	// start is where the substitution starts, from is where the text not
	// yet read for case statements starts, depth the number of parentheses
	// open, and height the most that were, with the ones of the
	// substitutions in it. arithmetic, when not 0, is the depth inside the
	// (( that opens arithmetic ($((...)) or ((...))).
	start, from, depth, height, arithmetic int
}

// open reads the "(" at s[j] in the substitution's text, which opens
// arithmetic right after another "(".
func (p *parenScan) open(s string, j int) {
	p.depth++
	p.height = max(p.height, p.depth)
	if p.arithmetic == 0 && s[j-1] == '(' && !isEscaped(s, j-1) {
		p.arithmetic = p.depth
	}
}

// nest records a substitution in the text, whose parentheses nest height
// deep.
func (p *parenScan) nest(height int) {
	p.height = max(p.height, p.depth+height)
}

// close reads the ")" at s[j] and reports whether it closes the
// substitution, which it does not after a case pattern. ends are where the
// substitutions in it end.
func (p *parenScan) close(s string, j int, ends substitutionEnds) bool {
	if p.depth--; p.depth < p.arithmetic {
		p.arithmetic = 0
	}
	if p.depth > 0 {
		return false
	}
	if !p.hasCase {
		return true
	}
	if !p.readCases {
		p.cases, p.readCases = caseScan{start: true, ends: ends}, true
	}
	if !p.cases.read(s[:j], p.from) {
		return true
	}
	p.depth, p.from = 1, j
	return false
}

// substitutionMemo keeps where the command substitutions read so far end,
// so each is read once however many flags and commands are in and after it
// (["mysql", "-p$(x"], ["mysql", "-p$(y"], wget --password -$(x --password
// -$(y ...))). Where one ends depends only on where it starts, on the string
// holding the command and on whether it goes on across lines.
type substitutionMemo map[substitutionKey]substitutionResult

type substitutionKey struct {
	start, stringEnd int
	multiline        bool
	// heredoc, when set, keys where the body of a here-document with this
	// delimiter, starting on the line at start, ends (see heredocBodyEnd).
	heredoc heredoc
}

type substitutionResult struct {
	end    int
	closed bool
	// height is how deep the parentheses of one that closes nest.
	height int
}

// substitutionEnds are the ends in subs of the substitutions in the string
// holding the command that ends at stringEnd, read across lines when
// multiline is set.
type substitutionEnds struct {
	subs      substitutionMemo
	stringEnd int
	multiline bool
}

func (e substitutionEnds) key(i int) substitutionKey {
	return substitutionKey{start: i, stringEnd: e.stringEnd, multiline: e.multiline}
}

// closedEnd returns the end of the substitution that starts at i, when it
// is one that closes.
func (e substitutionEnds) closedEnd(i int) (int, bool) {
	r, ok := e.subs[e.key(i)]
	return r.end, ok && r.closed
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
	// acrossLines reads a double-quoted string that holds a command
	// substitution going on to a later line as one string, as the value
	// of a long flag is read ("$(printf x<newline>)").
	acrossLines bool
	// substitutions steps over the command substitutions outside quoted
	// strings, and braces the parameter expansions too; subStart and
	// subEnd bound the last one found.
	// backticks steps over the command substitutions in backticks that
	// close on their line too, which a segment goes on past (see
	// backtickSegment).
	substitutions, braces, backticks bool
	subStart, subEnd                 int
	cases                            caseMemo
	// sep is where the text after the last shell operator outside the
	// quoted strings found starts. wordSep is the same, except that a
	// command substitution that closes is part of a word of the command
	// around it (ssh -i $(printf key) mysql), whose start wordSep goes
	// back to after it; closes holds the ones open where the scan is.
	sep, wordSep int
	closes       []substitutionClose
	// blankOf is one past the start of the last quoted string commandFrom
	// read, 0 when none, and blank whether its text holds a blank.
	blankOf int
	blank   bool
	// pieces keeps the parameter expansions read in the quoted strings of
	// s, without cases.lines.
	pieces substitutionMemo
}

// substitutionClose is a command substitution that closes at end, around
// which a command started at sep.
type substitutionClose struct {
	end, sep int
}

// stringAt returns the end of the quoted string that p is inside, after its
// opening quote, and false when p is not in one. p is never before an
// earlier query's.
func (q *quoteScan) stringAt(p int) (end int, ok bool) {
	if q.start < p && p < q.end {
		return q.end, true
	}
	for q.i < p {
		q.closeSubstitutions()
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
		if strings.HasPrefix(q.s[q.i:], "$'") {
			// An ANSI-C quoted string, in which a backslash escapes its
			// quote ($'abc\';x').
			if end := ansiCQuoteEnd(q.s, q.i); end > 0 {
				q.start, q.end, q.i = q.i+1, end, end
				if p < end {
					return end, true
				}
				continue
			}
		}
		if end, quoteLen := q.quotedPiece(); quoteLen > 0 {
			q.start, q.end, q.i = q.i, end, end
			if p < end {
				return end, true
			}
			continue
		}
		q.openSubstitution()
		if isCommandEnd(q.s, q.i) {
			q.sep, q.wordSep = q.i+1, q.i+1
		}
		if q.s[q.i] == '\\' {
			q.i++
		}
		q.i++
	}
	return 0, false
}

// quotedPiece returns the end of the quoted string that starts where the
// scan is, and its quote's length, as quotedPiece does, or as
// quotedPieceAcrossLines does with acrossLines.
func (q *quoteScan) quotedPiece() (end, quoteLen int) {
	if q.acrossLines {
		return quotedPieceAcrossLines(q.s, q.i, -1, q.cases.memo())
	}
	if q.cases.lines != nil {
		return q.cases.quotedPiece(q.s, q.i)
	}
	if q.pieces == nil {
		q.pieces = substitutionMemo{}
	}
	return braceReader{shallow: true, braces: q.pieces}.quotedPiece(q.s, q.i)
}

// openSubstitution notes the command substitution that starts where the
// scan is, when it closes and the scan does not step over it.
func (q *quoteScan) openSubstitution() {
	if q.substitutions || !isSubstitutionStart(q.s, q.i) {
		return
	}
	if n := q.cases.substitutionLen(q.s, q.i); n > 0 {
		q.closes = append(q.closes, substitutionClose{end: q.i + n, sep: q.wordSep})
	}
}

// closeSubstitutions ends the command substitutions that closed before
// where the scan is, after which the command around each goes on.
func (q *quoteScan) closeSubstitutions() {
	for n := len(q.closes); n > 0 && q.closes[n-1].end <= q.i; n = len(q.closes) {
		q.wordSep, q.closes = q.closes[n-1].sep, q.closes[:n-1]
	}
}

// wordFrom is commandFrom, for a command of whose words a substitution
// that closes before the position of the last stringAt query is one
// (ssh -i $(printf key) mysql).
func (q *quoteScan) wordFrom(closeAt int) int {
	from := q.commandFrom(closeAt)
	if closeAt >= 0 && q.blank {
		return from
	}
	q.closeSubstitutions()
	return q.wordSep
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

// stepSubstitution steps over the command substitution, or with braces the
// parameter expansion, that starts where the scan is, when the scan looks
// for them, and reports whether one did.
func (q *quoteScan) stepSubstitution() bool {
	end := 0
	switch {
	case !q.substitutions:
	case isSubstitutionStart(q.s, q.i):
		if n := q.cases.substitutionLen(q.s, q.i); n > 0 {
			end = q.i + n
		}
	case q.backticks && q.s[q.i] == '`':
		if at, closed := backtickClose(q.s, q.i, len(q.s)); closed {
			end = at + 1
		}
	case q.braces:
		end = q.cases.braceEnd(q.s, q.i)
	}
	if end == 0 {
		return false
	}
	q.subStart, q.subEnd, q.i = q.i, end, end
	return true
}

// isSubstitutionStart reports whether a command substitution ($(...)) or a
// process substitution (<(...), >(...)) starts at s[i].
func isSubstitutionStart(s string, i int) bool {
	return i+1 < len(s) && s[i+1] == '(' && strings.IndexByte("$<>", s[i]) >= 0
}

// caseMemo finds the length of command and process substitutions at
// increasing positions of one string, as readSubstitution reads them. The
// ")" that ends a pattern of a case statement in one does not close it
// ($(case x in x) mysql -pX;; esac), <(case x in x) printf h;; esac)), nor
// one in a substitution in it ($(echo $(case x in x) a;; esac) mysql -pX)).
type caseMemo struct {
	subs substitutionMemo
	// anyHeight finds substitutions however deep their parentheses nest,
	// for where a segment ends rather than which commands in it run on
	// their own.
	anyHeight bool
	// lines, when set, reads an unquoted substitution that does not close
	// on its line on to a later one, as the shell does, in the text it
	// holds, where the text this memo reads starts at base.
	lines *lineReads
	base  int
}

// lineReads reads the command and process substitutions of s across lines,
// keeping the ends found, so a substitution that does not close on its line
// is read once for all the segments and scans of s that meet it (each line
// a client name and a $( that never closes).
type lineReads struct {
	s    string
	subs substitutionMemo
	// within keeps, for each end of a text that a caseMemo reads, the
	// substitutions read in that text, keyed by where they start in s, so
	// the many texts of s that end there share them (each segment read from
	// a command name to the end of its line: mysql -e $(echo `x`mysql -e
	// $(echo `x`...).
	within map[int]substitutionMemo
	// pieces is within for the parameter expansions in the quoted strings
	// that quotedPiece reads, which it reads its own way (see shallow), and
	// braces for those that braceEnd reads, also where one does not close,
	// which each segment of a line of client names after an unclosed
	// ${PW:-) would otherwise read to the line's end again.
	pieces map[int]substitutionMemo
	braces map[int]substitutionMemo
}

// in returns the memo of the substitutions read in the texts of s that end
// at end, and piecesIn that of the quoted strings read there.
func (l *lineReads) in(end int) substitutionMemo {
	return memoAt(&l.within, end)
}

func (l *lineReads) piecesIn(end int) substitutionMemo {
	return memoAt(&l.pieces, end)
}

func (l *lineReads) bracesIn(end int) substitutionMemo {
	return memoAt(&l.braces, end)
}

// memoAt returns the memo that memos holds for end, made on first use.
func memoAt(memos *map[int]substitutionMemo, end int) substitutionMemo {
	if *memos == nil {
		*memos = map[int]substitutionMemo{}
	}
	m := (*memos)[end]
	if m == nil {
		m = substitutionMemo{}
		(*memos)[end] = m
	}
	return m
}

// mayOpenLine reports whether text[:n], where a segment pattern stopped
// before the end of text, holds the start of a substitution, which m may
// read on to a later line, so the pattern may have stopped inside it.
func (m *caseMemo) mayOpenLine(text string, n int) bool {
	if m.lines == nil || n == len(text) {
		return false
	}
	head := text[:n]
	return strings.Contains(head, "$(") || strings.Contains(head, "<(") || strings.Contains(head, ">(")
}

// memo returns the memo of the ends found, made on first use.
func (m *caseMemo) memo() substitutionMemo {
	if m.subs == nil {
		m.subs = substitutionMemo{}
	}
	return m.subs
}

// maxSubstitutionHeight is how deep the parentheses of a substitution that
// a command's segment reads on its own may nest, as in
// cliSubstitutionPattern. A deeper one is text of the command around it,
// whose flags in it are read as that command's, while the substitutions
// nested in it are read on their own. Reading every depth on its own would
// make each command read the rest of the substitutions it is in again, in
// time quadratic in the length of the line (mysql -pX $(mysql -pX $(...))).
const maxSubstitutionHeight = 3

// substitutionLen returns the length of the command or process substitution
// that starts at s[i], or 0 when none does, or it does not close in s, or,
// unless anyHeight is set, its parentheses nest deeper than
// maxSubstitutionHeight. One that stops at the end of its line is read on
// to a later one when lines is set.
func (m *caseMemo) substitutionLen(s string, i int) int {
	if !isSubstitutionStart(s, i) {
		return 0
	}
	r := m.read(s, i)
	if !r.closed && m.lines != nil && r.end < len(s) && (s[r.end] == '\r' || s[r.end] == '\n') {
		r = readSubstitution(m.lines.s, m.base+i, -1, m.lines.subs, true)
		r.end -= m.base
		r.closed = r.closed && r.end <= len(s)
	}
	if r.closed && (m.anyHeight || r.height <= maxSubstitutionHeight) {
		return r.end - i
	}
	return 0
}

// read reads the substitution at s[i] within s, in the memo that lines
// shares for every text of its string that ends where s does, when m has
// lines, which s starts at base in.
func (m *caseMemo) read(s string, i int) substitutionResult {
	if m.lines == nil {
		return readSubstitution(s, i, -1, m.memo(), false)
	}
	end := m.base + len(s)
	r := readSubstitution(m.lines.s[:end], m.base+i, -1, m.lines.in(end), false)
	r.end -= m.base
	return r
}

// quotedPiece is quotedPiece, keeping the parameter expansions it reads in
// the memo that lines shares for every text of its string that ends where s
// does, when m has lines, so each is read once ("psql${ "psql${ "...).
func (m *caseMemo) quotedPiece(s string, i int) (end, quoteLen int) {
	if m.lines == nil {
		return quotedPiece(s, i)
	}
	to := m.base + len(s)
	end, quoteLen = braceReader{shallow: true, braces: m.lines.piecesIn(to)}.quotedPiece(m.lines.s[:to], m.base+i)
	if quoteLen > 0 {
		end -= m.base
	}
	return end, quoteLen
}

// braceEnd is braceExpansionEnd, keeping the ends it reads in the memo
// that lines shares for every text of its string that ends where s does,
// when m has lines, so each is read once (${PW:-)mysql ${PW:-)mysql ...).
func (m *caseMemo) braceEnd(s string, i int) int {
	if m.lines == nil {
		return braceExpansionEnd(s, i, m.memo())
	}
	to := m.base + len(s)
	end := braceExpansionEnd(m.lines.s[:to], m.base+i, m.lines.bracesIn(to))
	if end > 0 {
		end -= m.base
	}
	return end
}

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
	// ends are where the substitutions in the text end, when they are
	// known.
	ends substitutionEnds
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
		case ch == '#' && startsComment(s, i, from):
			// A comment runs to the end of its line, so a ")" in it ends
			// no pattern (case x in # note )<newline>x) ...).
			if n := strings.IndexAny(s[i:], "\r\n"); n >= 0 {
				i += n
			} else {
				i = len(s)
			}
		default:
			end := caseWordEnd(s, i, c.ends)
			c.word(s[i:end])
			i = end
		}
	}
	return c.state == casePattern
}

// caseWordEnd returns the end of the word that starts at s[i], whose quoted
// strings, escaped characters, substitutions and parameter expansions are
// part of it (case ${PW:-x)y} in). ends are where the substitutions end,
// when they are known.
func caseWordEnd(s string, i int, ends substitutionEnds) int {
	for i < len(s) && strings.IndexByte(" \t\r\n;&|()", s[i]) < 0 {
		if end, quoteLen := quotedPiece(s, i); quoteLen == 1 {
			i = end
			continue
		}
		if s[i] == '\\' {
			i++
		} else if end := braceExpansionEnd(s, i, ends.subs); end > 0 {
			i = end - 1
		} else if end, ok := ends.closedEnd(i); ok {
			i = end - 1
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
// (sh -c "mysql -p"; ls "x"). A password such as '+hunter2+' outside such
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

// evalCommand is eval, which joins its arguments into the command it runs.
const evalCommand = "eval"

// execCommand and envCommand are the command wrappers exec and env; exec is
// also the subcommand of a container client that runs a command, and
// loginCommand the one that logs in to a registry (docker login -p).
// podmanCommand is the container client whose exec takes --latest.
const (
	execCommand   = "exec"
	envCommand    = "env"
	loginCommand  = "login"
	podmanCommand = "podman"
)

// isWrapperOperand reports whether the client name at start is the
// destination of an ssh command (ssh mysql -p2222, ssh -i "key" mysql
// -p2222, where -p2222 is ssh's port) or the argument of a wrapper's option
// (sudo -u mysql ssh -p2222), not a command. It reads the words of the
// command from its start (see wrapperWords), so an ssh that is an argument
// of another command (docker exec ssh mysql -pX) does not count. from is
// outside quoted strings, at or before the command's start.
func isWrapperOperand(s string, long *wrapperStarts, from, start int) bool {
	if isListWrapperOperand(s, start) {
		return true
	}
	// The word may be a path ending in the name (ssh -i ~/.ssh/mysql),
	// with escaped operators in it (ssh -i key\;mysql), and quoted
	// (ssh "mysql").
	word := start
	for word > 0 && start-word < maxSSHCommand && (!isShellWordEnd(s[word-1]) || isEscaped(s, word-1)) && s[word-1] != '"' && s[word-1] != '\'' {
		word--
	}
	if word > from && (s[word-1] == '"' || s[word-1] == '\'') {
		if end, quoteLen := long.quotedPiece(s, word-1); quoteLen == 1 && end > start {
			word--
		}
	}
	if word == 0 || s[word-1] != ' ' && s[word-1] != '\t' {
		return false
	}
	part := s[word:start]
	c, ok := wrapperWords(s, long, from, word)
	if !ok || runsJoinedArgument(c.wrapper, part) {
		return false
	}
	// The destination, the argument of an option (ssh -i mysql,
	// sudo -u mysql) unless that option runs it (env -S mysql), or an
	// operand of a container client's subcommand that runs nothing
	// (kubectl patch deployment mysql).
	return c.noRun || !c.text && !c.execArg && (c.optionArg || hasDestination(c.wrapper) && !c.host)
}

// wrapperWords returns what the words before start of the command it is in
// tell of it, reading s from from, which is outside quoted strings, and
// false when no wrapper that takes an argument is in them. A substitution
// that closes is part of a word (ssh -i $(printf key) mysql). A command that
// starts at most maxSSHCommand bytes back is read from where commandFrom
// finds it starts, and a longer one from from, once for names at increasing
// positions in it, by long (ssh -i /a/long/path mysql).
func wrapperWords(s string, long *wrapperStarts, from, start int) (commandWords, bool) {
	if start-from > maxSSHCommand {
		c := long.at(from)
		c.at(s, from, start)
		return c.commandWords, c.wrapper != ""
	}
	from = commandFrom(s, from, start)
	if !slices.ContainsFunc(argWrappers, func(w string) bool { return strings.Contains(s[from:start], w) }) {
		return commandWords{}, false
	}
	c := commandStart{words: &long.words}
	c.at(s, from, start)
	return c.commandWords, true
}

// isOperandOfCommand reports whether the client name at start, in the
// command that starts at from, is an operand of a program that runs none
// of its operands, a path or a name it reads (find /etc/mysql -print,
// go test ./mysql -parallel 8, tar -C /opt/mysql -pxzf a.tgz, nmap mysql
// -p3306), so the flags after it are that program's. A client named first,
// after wrappers, or in prose before any such program, is still read.
func isOperandOfCommand(s string, starts *wrapperStarts, from, start int) bool {
	word := start
	for word > from && start-word < maxSSHCommand && !isShellWordEnd(s[word-1]) {
		word--
	}
	if word <= from || s[word-1] != ' ' && s[word-1] != '\t' {
		return false
	}
	c := starts.at(from)
	c.at(s, from, word)
	return c.text && !c.operandRuns && slices.Contains(operandCommands, c.name)
}

// runsOperands reports whether word, an argument of program, makes it run
// the words after it: find's -exec family, go run, which runs the package
// it builds (go run ./cmd/mysql -ppw), and go tool, which runs the tool it
// names (go tool mysql -ppw).
func runsOperands(program, word string) bool {
	switch program {
	case "find":
		return word == "-exec" || word == "-execdir" || word == "-ok" || word == "-okdir"
	case "go":
		return word == "run" || word == "tool"
	}
	return false
}

// operandCommands are programs whose operands are paths, names or hosts
// they read, and that run none of them as a command (but see
// runsOperands).
var operandCommands = strings.Fields("find ls cd cat less head tail grep egrep fgrep rg ag tar zip unzip cp mv rm ln mkdir touch chmod chown du stat file tree wc diff nmap nc ping go journalctl systemctl service brew apt apt-get yum dnf apk pip pgrep pkill lsof ps kill which whereis man")

// wrapperStarts reads the commands longer than maxSSHCommand bytes for
// isWrapperOperand, one commandStart for each start, so each is read once,
// and words finds where the substitutions in any command close.
type wrapperStarts struct {
	starts map[int]*commandStart
	words  caseMemo
	// pieces keeps the parameter expansions read in quoted strings of s.
	pieces substitutionMemo
}

// quotedPiece is quotedPiece for s, the string w reads, keeping the
// parameter expansions it reads in w.
func (w *wrapperStarts) quotedPiece(s string, i int) (end, quoteLen int) {
	if w.pieces == nil {
		w.pieces = substitutionMemo{}
	}
	return braceReader{shallow: true, braces: w.pieces}.quotedPiece(s, i)
}

// at returns the commandStart of the command that starts at from.
func (w *wrapperStarts) at(from int) *commandStart {
	c := w.starts[from]
	if c == nil {
		if w.starts == nil {
			w.starts = map[int]*commandStart{}
		}
		c = &commandStart{words: &w.words}
		w.starts[from] = c
	}
	return c
}

// argWrappers are the wrappers that take an argument after the command's
// start: a destination, or an option's (wrapperArgOptions).
var argWrappers = []string{sshCommand, "sudo", envCommand, "time", execCommand, "nice", "docker", podmanCommand, "nerdctl", "kubectl"}

// maxSSHCommand bounds how far back isWrapperOperand looks for the start of
// a wrapper command.
const maxSSHCommand = 256

// commandFrom returns where the command that start is in begins, reading s
// from from, which is outside quoted strings: after the last shell operator
// before start, or inside the quoted string or substitution start is in. A
// quoted string or substitution that closes before start is part of a word
// (ssh -i "key" mysql, ssh -i $(printf key) mysql).
func commandFrom(s string, from, start int) int {
	head, b := s[:start], from
	var cases caseMemo
	for i := from; i < start; {
		if isApostrophe(s, i) {
			i++
			continue
		}
		if end, quoteLen := quotedPiece(head, i); quoteLen > 0 {
			i = end
			continue
		}
		// So is a substitution that closes before start
		// (ssh -i $(printf key) mysql).
		if n := cases.substitutionLen(head, i); n > 0 {
			i += n
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
	i := min(shellWordEnd(s, start, stringEnd, nil), end)
	optionArg := false
	for i < end {
		if s[i] == ' ' || s[i] == '\t' {
			i++
			continue
		}
		j := min(max(shellWordEnd(s, i, stringEnd, nil), i+1), end)
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

// isFunctionName reports whether the client name between start and end
// names a shell function being defined (mysql() { ssh -p2222 db; },
// function mysql { ... }), whose body runs other commands, rather than the
// client run. The definition starts a command, so a name or a function
// keyword that is an argument (ssh function mysql -pX) defines nothing.
func isFunctionName(s string, start, end int) bool {
	before := strings.TrimRight(s[:start], " \t")
	if len(before) < start && strings.HasSuffix(before, functionKeyword) && startsCommand(s, len(before)-len(functionKeyword)) {
		return true
	}
	after := strings.TrimLeft(s[end:], " \t")
	return strings.HasPrefix(after, "(") && strings.HasPrefix(strings.TrimLeft(after[1:], " \t"), ")") && startsCommand(s, start)
}

// commandStarts are the characters after which a word starts a command:
// commandEnds, the ")" after a case pattern, and the quote that opens a
// script.
const commandStarts = commandEnds + ")'\""

// startsCommand reports whether the word at s[i] starts a command: it is at
// the start of s, or after one of commandStarts or a keyword that a command
// follows (then function mysql { ... }) and itself starts one, each with
// blanks between. A word that goes on before i (thenfunction mysql) starts
// none.
func startsCommand(s string, i int) bool {
	for {
		if i > 0 && strings.IndexByte(" \t"+commandStarts, s[i-1]) < 0 {
			return false
		}
		before := strings.TrimRight(s[:i], " \t")
		if before == "" || strings.IndexByte(commandStarts, before[len(before)-1]) >= 0 {
			return true
		}
		word := strings.LastIndexAny(before, " \t"+commandStarts) + 1
		if !isShellKeyword(before[word:]) {
			return false
		}
		// A keyword is one only where a command starts, so a destination
		// named like one (ssh then function mysql) is not.
		i = word
	}
}

// functionKeyword starts a function definition (function mysql { ... }).
const functionKeyword = "function"

// isContainerNameArgument reports whether the client name at start is the
// value of --name (docker run --name mysql -p3306 mysql:8), not a command.
// The option must be a word of its own, so an assignment ending in it
// (env TAG=--name openssl ...) is not one.
func isContainerNameArgument(s string, start int) bool {
	before := s[:start]
	if option, ok := strings.CutSuffix(before, "--name="); ok {
		return startsWord(option)
	}
	trimmed := strings.TrimRight(before, " \t")
	option, ok := strings.CutSuffix(trimmed, "--name")
	return ok && len(trimmed) < len(before) && startsWord(option)
}

// startsWord reports whether a shell word starts after before: at the start
// of the text or after a blank.
func startsWord(before string) bool {
	return before == "" || strings.ContainsRune(" \t\r\n", rune(before[len(before)-1]))
}

// appendNetrcPasswords adds the password of every .netrc entry in s, and of
// every one that printf or echo -e writes from a quoted string, where \n,
// \r and \t stand for a line break and blanks (printf "machine h\nlogin
// me\npassword x\n" > ~/.netrc). Each is read as text too, as plain echo
// writes it, so a password with \n in it is hidden whole either way, and
// one outside a quoted string, as a file holds it, is hidden whole.
func appendNetrcPasswords(regions []taggedRegion, s string) []taggedRegion {
	written, ok := netrcEscapesAsBlanks(s)
	regions = appendNetrcEntries(regions, s, written)
	if ok {
		regions = appendNetrcEntries(regions, written, "")
	}
	return regions
}

// netrcEscapesAsBlanks returns s with each \n, \r and \t inside a quoted
// string written as the blanks they stand for, \n as a blank and a line
// break, so the text keeps its offsets. ok is false when s holds none.
func netrcEscapesAsBlanks(s string) (string, bool) {
	var written []byte
	quotes := quoteScan{s: s, prose: true}
	for i := strings.IndexByte(s, '\\'); i >= 0 && i+1 < len(s); {
		if c := s[i+1]; c == 'n' || c == 'r' || c == 't' {
			if _, quoted := quotes.stringAt(i); quoted {
				if written == nil {
					written = []byte(s)
				}
				written[i], written[i+1] = ' ', ' '
				if c == 'n' {
					written[i+1] = '\n'
				}
			}
		}
		// The escaped character is skipped too, so \\n is a backslash and n.
		next := strings.IndexByte(s[i+2:], '\\')
		if next < 0 {
			break
		}
		i += 2 + next
	}
	return string(written), written != nil
}

// appendNetrcEntries adds the password of every .netrc entry in s. written
// is s with the escapes that printf writes as blanks written so (see
// netrcEscapesAsBlanks), or empty when s holds none: a token ends before
// only those (see netrcTokenEnd).
func appendNetrcEntries(regions []taggedRegion, s, written string) []taggedRegion {
	for _, p := range netrcPasswordPatterns {
		lines := quoteScan{s: s, prose: true}
		for _, loc := range p.pattern.FindAllStringSubmatchIndex(s, -1) {
			regions = p.appendPassword(regions, s, written, &lines, loc)
			for _, d := range netrcDefaultPatterns {
				if dloc := d.pattern.FindStringSubmatchIndex(s[loc[1]:]); dloc != nil {
					for k := range dloc {
						if dloc[k] >= 0 {
							dloc[k] += loc[1]
						}
					}
					regions = d.appendPassword(regions, s, written, &lines, dloc)
					break
				}
			}
		}
	}
	return regions
}

// appendPassword adds the password of the .netrc entry that loc, a match of
// p in s, found. written is as for appendNetrcEntries.
func (p netrcPattern) appendPassword(regions []taggedRegion, s, written string, lines *quoteScan, loc []int) []taggedRegion {
	machine := ""
	if loc[2] >= 0 {
		machine = s[loc[2]:loc[3]]
	}
	login := ""
	if p.login > 0 {
		login = s[loc[2*p.login]:loc[2*p.login+1]]
	}
	start, end := loc[2*p.value], loc[2*p.value+1]
	if isNetrcDeclaration(s, loc[0], machine, login, s[start:end]) || isNetrcProse(s, machine, login, start, end) {
		return regions
	}
	if p.login == 0 && !endsNetrcEntry(s, start, end) {
		// Prose about a machine's password, which names no login either
		// and goes on after the word (machine example.com password is
		// in the vault).
		return regions
	}
	regions = appendNetrcValue(regions, s, lines, start, netrcTokenEnd(s, written, start, end))
	if s[start] == '#' {
		// curl reads a token that starts with # as a comment that runs to
		// the end of its line, and the next token as the password
		// (password #rotated<newline>hunter2), even a keyword. Both are
		// redacted, as other readers may take the first.
		if next := netrcTokenAfterComment.FindStringSubmatchIndex(s[end:]); next != nil && s[end+next[2]] != '#' {
			regions = appendNetrcValue(regions, s, lines, end+next[2], end+next[3])
		}
	}
	return regions
}

// netrcTokenEnd returns where the .netrc token s[start:end] ends: before a
// \n, \r or \t in a quoted string, which written holds as blanks, that a
// .netrc keyword and another field follow, which printf writes as the next
// entry or field (printf "machine h login me password x\nmachine b ..."),
// or at end. A keyword that nothing follows is part of the token (password
// x\nmachine), as no field shows that printf writes it, and so is one
// outside a quoted string, which a file holds as it is (password
// abc\nmachine other).
func netrcTokenEnd(s, written string, start, end int) int {
	token := s[start:end]
	for i := strings.IndexByte(token, '\\'); i >= 0 && i+1 < len(token); {
		if c := token[i+1]; (c == 'n' || c == 'r' || c == 't') && written != "" && written[start+i] == ' ' {
			word, _, _ := strings.Cut(token[i+2:], `\`)
			if isNetrcKeyword(word) && netrcFieldFollows(s, start+i+2+len(word)) {
				return start + i
			}
		}
		next := strings.IndexByte(token[i+2:], '\\')
		if next < 0 {
			break
		}
		i += 2 + next
	}
	return end
}

// netrcFieldFollows reports whether a .netrc field starts at s[p], after
// blanks and the \n, \r and \t that printf writes as them, before the text,
// a line, a quoted string or a command ends.
func netrcFieldFollows(s string, p int) bool {
	for p < len(s) {
		switch {
		case s[p] == ' ' || s[p] == '\t':
			p++
		case s[p] == '\\' && p+1 < len(s) && (s[p+1] == 'n' || s[p+1] == 'r' || s[p+1] == 't'):
			p += 2
		default:
			return strings.IndexByte("\r\n\"'`;|&)\\", s[p]) < 0
		}
	}
	return false
}

// isNetrcProse reports whether the fields of a .netrc entry's match, whose
// password token is s[start:end], are prose: a login that joins the
// keywords in a sentence ("login and password below", "login then password
// then mfa"), a password that is a word joining one and more words follow
// ("password is in the vault"), or a machine name that ends a clause
// ("machine name, login and password"). No file names a login that way,
// and a password that ends the entry's text is one (login alice password
// first).
func isNetrcProse(s, machine, login string, start, end int) bool {
	password := strings.ToLower(strings.TrimRight(s[start:end], ".,;:"))
	return strings.ContainsAny(machine[max(len(machine)-1, 0):], ",;") ||
		slices.Contains(netrcLoginProseWords, strings.ToLower(login)) ||
		slices.Contains(netrcProseWords, password) && !endsNetrcEntry(s, start, end)
}

// netrcLoginProseWords join the login keyword to the password keyword in a
// sentence (login and password, login then password), and netrcProseWords
// join the words of one.
var (
	netrcLoginProseWords = strings.Fields("and or then with to for from is are")
	netrcProseWords      = strings.Fields("and or then with the is are to for from below above here first next")
)

// netrcTokenAfterComment matches the rest of a line that a comment ends,
// the blank and comment lines after it, and the next .netrc token, group 1.
var netrcTokenAfterComment = regexp.MustCompile(`^[^\r\n]*\r?\n(?:[ \t]*(?:#[^\r\n]*)?\r?\n)*[ \t]*(` + netrcValue + `)`)

// netrcKeywords are the keywords of a .netrc file.
var netrcKeywords = strings.Fields("machine default login password account macdef")

// isNetrcKeyword reports whether token is a .netrc keyword, which curl reads
// in any case.
func isNetrcKeyword(token string) bool {
	for _, k := range netrcKeywords {
		if strings.EqualFold(token, k) {
			return true
		}
	}
	return false
}

// endsNetrcEntry reports whether the .netrc password token s[start:end]
// ends its entry's text, as a file's or a string's that holds one does: the
// text ends, a line, a comment, a quoted string or a command does, or the
// next entry or field starts (machine h password x login me). A token that
// holds an escaped line break ends its line in a string of code
// ("machine h password x\nmachine ...").
func endsNetrcEntry(s string, start, end int) bool {
	rest := strings.TrimLeft(s[end:], " \t")
	if rest == "" || strings.Contains(s[start:end], `\n`) || strings.IndexByte("\r\n#\"'`;|&<>)]},", rest[0]) >= 0 || strings.HasPrefix(rest, `\"`) {
		return true
	}
	n := strings.IndexAny(rest, " \t\r\n")
	if n < 0 {
		n = len(rest)
	}
	return isNetrcKeyword(rest[:n])
}

// appendNetrcValue appends the region of the .netrc password token at
// s[start:end].
func appendNetrcValue(regions []taggedRegion, s string, lines *quoteScan, start, end int) []taggedRegion {
	valueStart, valueEnd, singleQuoted := unquoteRangeQuoted(s, start, end)
	if end-start >= 4 && strings.HasPrefix(s[start:end], `\"`) && strings.HasSuffix(s[start:end], `\"`) {
		valueStart, valueEnd = start+2, end-2
	}
	// A .netrc file expands nothing, so a password that starts with $ or
	// a backtick ($ecret, $SUMMER2026, `cat /run/pw`) is literal, as in
	// single quotes.
	literal := singleQuoted || strings.HasPrefix(s[valueStart:valueEnd], "$") || strings.HasPrefix(s[valueStart:valueEnd], "`")
	v := cliValue{start: valueStart, end: valueEnd, quoted: valueStart != start, singleQuoted: literal}
	v.gap = v.quoted && valueStart == lines.stringEnd(start-1) && isStringGap(s[valueStart:valueEnd])
	if r, ok := passwordValueRegion(s, v, -1, nil, false, false); ok {
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
// (wget --password swordfish, wget --password string). stringEnd is
// where the quoted string holding the command ends, or -1, and subs keeps the
// substitutions read for the values after it, or is nil.
func passwordValueRegion(s string, v cliValue, stringEnd int, subs substitutionMemo, dashFlag, requireNonWord bool) (taggedRegion, bool) {
	value := s[v.start:v.end]
	switch {
	case value == "", dashFlag && !v.quoted && value[0] == '-', v.gap, isPasswordMetavar(value):
		return taggedRegion{}, false
	case strings.HasPrefix(value, "{{"):
		// A template fills the value in, as ${...} would
		// (--password {{ .Values.password }}); text and quoted pieces
		// joined after it are part of the password ({{ .Values.prefix }}x,
		// {{ .Values.prefix }}"x"), but not the quote that closes the
		// string holding the command (sh -c "tool --password={{ .Values.pw }}").
		if closeEnd, ok := templateEnd(s, v.start); ok {
			wordEnd := v.end
			if !v.quoted && closeEnd >= v.end {
				wordEnd = wordRestEnd(s, closeEnd, stringEnd, false, subs)
			}
			if closeEnd >= v.end && wordEnd <= closeEnd {
				return taggedRegion{}, false
			}
			v.end = max(v.end, wordEnd)
		}
	case v.singleQuoted:
		if !hasNonPlaceholderQuotedPasswordValue(value, true) {
			return taggedRegion{}, false
		}
	case strings.HasPrefix(s[v.start:], "`"):
		// As $(...) below: `cat /run/pw` fetches the password,
		// `printf hunter2` carries it.
		if end := backtickEnd(s, v.start, stringEnd, subs); end == v.end && s[end-1] == '`' && isRecipeSubstitution(s[v.start+1:end-1]) {
			return taggedRegion{}, false
		}
	case strings.HasPrefix(s[v.start:], "$("):
		// Only a substitution that is the whole value and fetches the
		// password from elsewhere stays visible: $(printf hunter2) and
		// $(cat f)suffix carry it.
		// One that does not close takes the rest of an unquoted word, and
		// ends with a quoted value. An unquoted one goes on across lines.
		end, closed := substitutionEnd(s, v.start, stringEnd, subs, subs != nil && !v.quoted)
		if closed && end >= v.end && isRecipeSubstitution(s[v.start+2:end-1]) {
			return taggedRegion{}, false
		}
		if closed || !v.quoted {
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

// isPasswordMetavar reports whether value is the name a help text gives a
// password flag's value: words in capitals joined by '_', one of which
// names a password (--password PASSWORD, -pPASSWORD, --pass=DB_PASS). A
// word that only holds one (-pBYPASS, -pCOMPASS) is a password.
func isPasswordMetavar(value string) bool {
	if value == "" || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_") != "" {
		return false
	}
	return slices.ContainsFunc(strings.Split(value, "_"), func(word string) bool {
		return slices.Contains(passwordMetavarWords, word)
	})
}

// passwordMetavarWords are the words of a metavar that name a password.
var passwordMetavarWords = strings.Fields("PASS PASSWD PASSWORD PASSPHRASE")

// templateEnd returns where the {{ ... }} template that starts at s[start]
// closes on its line, within maxTemplateLen bytes, after its "}}"; ok is
// false when none closes. The bound keeps a line of unclosed templates
// linear.
func templateEnd(s string, start int) (closeEnd int, ok bool) {
	line := s[start:min(len(s), start+maxTemplateLen)]
	if k := strings.IndexAny(line, "\r\n"); k >= 0 {
		line = line[:k]
	}
	closeAt := strings.Index(line, "}}")
	if closeAt < 0 {
		return 0, false
	}
	return start + closeAt + len("}}"), true
}

// maxTemplateLen is the longest {{ ... }} template read as one.
const maxTemplateLen = 256

// isPassNumber reports whether value numbers a pass of a tool that runs
// several, one digit (x264 --pass 2). A password of one digit has ten
// values and hides nothing, while two or more may be a PIN (redis-cli
// --pass 12, curl --pass 42).
func isPassNumber(value string) bool {
	return len(value) == 1 && value[0] >= '0' && value[0] <= '9'
}

// isRecipeSubstitution reports whether body, the text of a $(...), only
// fetches the password from elsewhere ($(cat /run/pw)). A line break in it
// runs another command after the first ($(cat f<newline>printf hunter2)),
// so only the ones around the command can be there.
func isRecipeSubstitution(body string) bool {
	body = strings.TrimSpace(body)
	return !strings.ContainsAny(body, "\r\n") && isSecretRecipeCommand(body)
}

// isProseValue reports whether v, the value after a long flag and a blank,
// is a word of prose (to, flag, string) in a sentence: one that ends a
// sentence or clause ("the --password flag.", "--password option, or"), or
// a grammatical word that another word follows ("--password to set it",
// "--password" is required). A word that names what the flag takes (flag,
// string, value) may also be the password, so another word makes it prose
// only when that word is prose too ("the --password flag is") or the text
// of a help line ("--password string   Password for"); the words after it
// may be arguments (wget --password string http://host/). A word ended by
// punctuation is the password when an argument follows it on its line
// (wget --password string. http://host/).
func isProseValue(s string, v cliValue) bool {
	value := s[v.start:v.end]
	word := strings.TrimRight(value, ".,:!?")
	if !slices.Contains(proseWords, word) {
		return false
	}
	if len(word) < len(value) {
		return v.end == len(s) || unicode.IsSpace(rune(s[v.end])) && !startsArgument(strings.TrimLeft(s[v.end:], " \t"))
	}
	gap := len(s[v.end:]) - len(strings.TrimLeft(s[v.end:], " \t"))
	first, _ := proseWord(s[v.end+gap:])
	if gap == 0 || first == "" {
		return false
	}
	return !slices.Contains(proseNouns, word) || gap > 1 || s[v.end] == '\t' || slices.Contains(proseWords, strings.TrimRight(first, ".,:!?"))
}

// startsArgument reports whether text starts with a word that only a
// command line holds: a flag, a path, an expansion or a URL.
func startsArgument(text string) bool {
	word := text
	if end := strings.IndexAny(text, " \t\r\n"); end >= 0 {
		word = text[:end]
	}
	switch {
	case strings.Contains(word, "://"), strings.HasPrefix(word, "./"), strings.HasPrefix(word, "../"):
		return true
	case len(word) < 2 || word == "--":
		return false
	}
	return strings.IndexByte("-/~$", word[0]) >= 0
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
	"requires", "required", "expects", "takes", "accepts", "needs", "must", "should", "missing",
}, proseNouns...)

// proseNouns are the proseWords that name what a flag takes, which a
// password may also be (wget --password string http://host/).
var proseNouns = []string{"flag", "flags", "option", "options", "argument", "arguments", "arg", "args", "parameter", "parameters", "param", "value", "values", "string", "prompt", "field"}
